package tunnel

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Resolved is the private connection detail produced by resolving a target and
// a requested port. Neither field is part of the tunnel's public identity.
type Resolved struct {
	PodName    string
	TargetPort int32
}

// Resolve selects a backing pod and translates the requested remote port into a
// numeric container port, following kubectl's port-forward behavior for the
// supported built-in resource types. All API reads honor ctx.
func Resolve(ctx context.Context, client kubernetes.Interface, namespace string, target Target, port ParsedPort) (Resolved, error) {
	switch target.Type {
	case TargetPod:
		return resolvePodTarget(ctx, client, namespace, target, port)
	case TargetService:
		return resolveServiceTarget(ctx, client, namespace, target, port)
	case TargetDeployment, TargetReplicaSet, TargetStatefulSet, TargetDaemonSet, TargetJob, TargetReplicationController:
		return resolveWorkloadTarget(ctx, client, namespace, target, port)
	default:
		return Resolved{}, fmt.Errorf("cannot resolve target type %q", target.Type)
	}
}

func resolvePodTarget(ctx context.Context, client kubernetes.Interface, namespace string, target Target, port ParsedPort) (Resolved, error) {
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return Resolved{}, getError("pod", target.Name, namespace, err)
	}
	return resolvePodPort(pod, port)
}

func resolveServiceTarget(ctx context.Context, client kubernetes.Interface, namespace string, target Target, port ParsedPort) (Resolved, error) {
	svc, err := client.CoreV1().Services(namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return Resolved{}, getError("service", target.Name, namespace, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return Resolved{}, fmt.Errorf("cannot use service %q as a target: it is defined without a spec.selector", svc.Name)
	}

	pod, err := selectPod(ctx, client, namespace, labels.SelectorFromSet(svc.Spec.Selector).String())
	if err != nil {
		return Resolved{}, err
	}

	if err := checkServiceUDPPorts(svc, port); err != nil {
		return Resolved{}, err
	}

	targetPort, err := translateServicePort(svc, pod, port)
	if err != nil {
		return Resolved{}, err
	}

	if err := requireRunning(pod); err != nil {
		return Resolved{}, err
	}
	return Resolved{PodName: pod.Name, TargetPort: targetPort}, nil
}

func resolveWorkloadTarget(ctx context.Context, client kubernetes.Interface, namespace string, target Target, port ParsedPort) (Resolved, error) {
	selector, err := workloadSelector(ctx, client, namespace, target)
	if err != nil {
		return Resolved{}, err
	}

	selected, err := selectPod(ctx, client, namespace, selector)
	if err != nil {
		return Resolved{}, err
	}

	// Re-read the selected pod before forwarding so a pod that disappeared or
	// changed phase between list and connect is not used.
	pod, err := client.CoreV1().Pods(namespace).Get(ctx, selected.Name, metav1.GetOptions{})
	if err != nil {
		return Resolved{}, getError("pod", selected.Name, namespace, err)
	}

	if err := checkPodUDPPorts(pod, port); err != nil {
		return Resolved{}, err
	}

	targetPort, err := podPortNumber(pod, port)
	if err != nil {
		return Resolved{}, err
	}

	if err := requireRunning(pod); err != nil {
		return Resolved{}, err
	}
	return Resolved{PodName: pod.Name, TargetPort: targetPort}, nil
}

func workloadSelector(ctx context.Context, client kubernetes.Interface, namespace string, target Target) (string, error) {
	switch target.Type {
	case TargetDeployment:
		obj, err := client.AppsV1().Deployments(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("deployment", target.Name, namespace, err)
		}
		return labelSelector(obj.Spec.Selector)
	case TargetReplicaSet:
		obj, err := client.AppsV1().ReplicaSets(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("replicaset", target.Name, namespace, err)
		}
		return labelSelector(obj.Spec.Selector)
	case TargetStatefulSet:
		obj, err := client.AppsV1().StatefulSets(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("statefulset", target.Name, namespace, err)
		}
		return labelSelector(obj.Spec.Selector)
	case TargetDaemonSet:
		obj, err := client.AppsV1().DaemonSets(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("daemonset", target.Name, namespace, err)
		}
		return labelSelector(obj.Spec.Selector)
	case TargetJob:
		obj, err := client.BatchV1().Jobs(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("job", target.Name, namespace, err)
		}
		return labelSelector(obj.Spec.Selector)
	case TargetReplicationController:
		obj, err := client.CoreV1().ReplicationControllers(namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return "", getError("replicationcontroller", target.Name, namespace, err)
		}
		return labels.SelectorFromSet(obj.Spec.Selector).String(), nil
	default:
		return "", fmt.Errorf("cannot resolve workload selector for target type %q", target.Type)
	}
}

// labelSelector converts an optional metav1.LabelSelector. A nil selector
// matches nothing, matching kubectl's SelectorsForObject behavior.
func labelSelector(sel *metav1.LabelSelector) (string, error) {
	selector, err := metav1.LabelSelectorAsSelector(sel)
	if err != nil {
		return "", fmt.Errorf("invalid label selector: %w", err)
	}
	return selector.String(), nil
}

// selectPod lists pods for the selector and picks the best one using the
// ActivePods ordering kubectl uses for attach-style commands. If the list is
// empty it watches for the first Added or Modified pod, exactly like kubectl.
func selectPod(ctx context.Context, client kubernetes.Interface, namespace, selector string) (*corev1.Pod, error) {
	list, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %q with selector %q: %w", namespace, selector, err)
	}
	if len(list.Items) == 0 {
		return watchForPod(ctx, client, namespace, selector, list.ResourceVersion)
	}

	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, list.Items[i].DeepCopy())
	}
	sort.Sort(ActivePods(pods))
	return pods[len(pods)-1], nil
}

func watchForPod(ctx context.Context, client kubernetes.Interface, namespace, selector, resourceVersion string) (*corev1.Pod, error) {
	watcher, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector:   selector,
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to watch pods in namespace %q with selector %q: %w", namespace, selector, err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return nil, fmt.Errorf("watch for pods in namespace %q with selector %q ended before a pod appeared", namespace, selector)
			}
			switch event.Type {
			case watch.Added, watch.Modified:
				pod, ok := event.Object.(*corev1.Pod)
				if !ok {
					return nil, fmt.Errorf("watch event for pods contained unexpected object %T", event.Object)
				}
				return pod.DeepCopy(), nil
			case watch.Error:
				return nil, fmt.Errorf("watch for pods in namespace %q failed: %w", namespace, apierrors.FromObject(event.Object))
			}
		}
	}
}

func requireRunning(pod *corev1.Pod) error {
	if pod.Status.Phase != corev1.PodRunning {
		return fmt.Errorf("unable to forward to pod %q: pod is not running (phase %s)", pod.Name, pod.Status.Phase)
	}
	return nil
}

// resolvePodPort resolves a requested port against a directly-addressed pod.
func resolvePodPort(pod *corev1.Pod, port ParsedPort) (Resolved, error) {
	if err := checkPodUDPPorts(pod, port); err != nil {
		return Resolved{}, err
	}
	targetPort, err := podPortNumber(pod, port)
	if err != nil {
		return Resolved{}, err
	}
	if err := requireRunning(pod); err != nil {
		return Resolved{}, err
	}
	return Resolved{PodName: pod.Name, TargetPort: targetPort}, nil
}

// podPortNumber converts a requested port to a numeric container port. Named
// lookups scan containers first, then init containers, preserving the first
// match exactly like kubectl.
func podPortNumber(pod *corev1.Pod, port ParsedPort) (int32, error) {
	if port.Kind == PortNumber {
		return port.Number, nil
	}
	return lookupContainerPortNumberByName(pod, port.Name)
}

// translateServicePort resolves a requested service port through the service's
// spec.ports and targetPort, including named target ports and headless
// services, matching kubectl's LookupContainerPortNumberByServicePort.
func translateServicePort(svc *corev1.Service, pod *corev1.Pod, port ParsedPort) (int32, error) {
	var servicePort int32
	if port.Kind == PortNumber {
		servicePort = port.Number
	} else {
		resolved, err := lookupServicePortNumberByName(svc, port.Name)
		if err != nil {
			return 0, err
		}
		servicePort = resolved
	}

	for _, spec := range svc.Spec.Ports {
		if spec.Port != servicePort {
			continue
		}
		if svc.Spec.ClusterIP == corev1.ClusterIPNone {
			return servicePort, nil
		}
		if spec.TargetPort.Type == intstr.Int {
			if spec.TargetPort.IntValue() == 0 {
				return spec.Port, nil
			}
			return int32(spec.TargetPort.IntValue()), nil
		}
		return lookupContainerPortNumberByName(pod, spec.TargetPort.String())
	}
	return 0, fmt.Errorf("service %q does not have a service port %d", svc.Name, servicePort)
}

func lookupServicePortNumberByName(svc *corev1.Service, name string) (int32, error) {
	for _, spec := range svc.Spec.Ports {
		if spec.Name == name {
			return spec.Port, nil
		}
	}
	return -1, fmt.Errorf("service %q does not have a named port %q", svc.Name, name)
}

func lookupContainerPortNumberByName(pod *corev1.Pod, name string) (int32, error) {
	for _, container := range pod.Spec.Containers {
		for _, spec := range container.Ports {
			if spec.Name == name {
				return spec.ContainerPort, nil
			}
		}
	}
	for _, container := range pod.Spec.InitContainers {
		for _, spec := range container.Ports {
			if spec.Name == name {
				return spec.ContainerPort, nil
			}
		}
	}
	return -1, fmt.Errorf("pod %q does not have a named port %q", pod.Name, name)
}

// checkServiceUDPPorts rejects ports that the service declares UDP-only.
func checkServiceUDPPorts(svc *corev1.Service, port ParsedPort) error {
	udpOnly, tcpOnly := map[int32]bool{}, map[int32]bool{}
	for _, spec := range svc.Spec.Ports {
		switch spec.Protocol {
		case corev1.ProtocolUDP:
			udpOnly[spec.Port] = true
		case corev1.ProtocolTCP:
			tcpOnly[spec.Port] = true
		}
	}
	for p := range tcpOnly {
		delete(udpOnly, p)
	}
	if len(udpOnly) == 0 {
		return nil
	}

	var number int32
	if port.Kind == PortNumber {
		number = port.Number
	} else {
		resolved, err := lookupServicePortNumberByName(svc, port.Name)
		if err != nil {
			return err
		}
		number = resolved
	}
	if udpOnly[number] {
		return fmt.Errorf("UDP protocol is not supported for %s", port.String())
	}
	return nil
}

// checkPodUDPPorts rejects ports that the pod declares UDP-only. Kubectl only
// considers regular container ports when computing the UDP-only set.
func checkPodUDPPorts(pod *corev1.Pod, port ParsedPort) error {
	udpOnly, tcpOnly := map[int32]bool{}, map[int32]bool{}
	for _, container := range pod.Spec.Containers {
		for _, spec := range container.Ports {
			switch spec.Protocol {
			case corev1.ProtocolUDP:
				udpOnly[spec.ContainerPort] = true
			case corev1.ProtocolTCP:
				tcpOnly[spec.ContainerPort] = true
			}
		}
	}
	for p := range tcpOnly {
		delete(udpOnly, p)
	}
	if len(udpOnly) == 0 {
		return nil
	}

	var number int32
	if port.Kind == PortNumber {
		number = port.Number
	} else {
		resolved, err := lookupContainerPortNumberByName(pod, port.Name)
		if err != nil {
			return err
		}
		number = resolved
	}
	if udpOnly[number] {
		return fmt.Errorf("UDP protocol is not supported for %s", port.String())
	}
	return nil
}

func getError(kind, name, namespace string, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %q was not found in namespace %q", kind, name, namespace)
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return fmt.Errorf("not authorized to read %s %q in namespace %q: %w", kind, name, namespace, err)
	}
	return fmt.Errorf("failed to read %s %q in namespace %q: %w", kind, name, namespace, err)
}

// ActivePods orders pods from worst to best candidate, replicating kubectl's
// podutils.ActivePods comparison. The last element after sorting is the pod
// kubectl would attach to (which sorts Reverse(ActivePods) and takes the first).
type ActivePods []*corev1.Pod

func (s ActivePods) Len() int      { return len(s) }
func (s ActivePods) Swap(i, j int) { s[i], s[j] = s[j], s[i] }

func (s ActivePods) Less(i, j int) bool {
	// 1. Unassigned < assigned
	if s[i].Spec.NodeName != s[j].Spec.NodeName && (len(s[i].Spec.NodeName) == 0 || len(s[j].Spec.NodeName) == 0) {
		return len(s[i].Spec.NodeName) == 0
	}
	// 2. PodPending < PodUnknown < PodRunning
	m := map[corev1.PodPhase]int{corev1.PodPending: 0, corev1.PodUnknown: 1, corev1.PodRunning: 2}
	if m[s[i].Status.Phase] != m[s[j].Status.Phase] {
		return m[s[i].Status.Phase] < m[s[j].Status.Phase]
	}
	// 3. Not ready < ready
	if isPodReady(s[i]) != isPodReady(s[j]) {
		return !isPodReady(s[i])
	}
	// 4. Deleting < not deleting
	if isPodDeleting(s[i]) != isPodDeleting(s[j]) {
		return isPodDeleting(s[i])
	}
	// 5. Older deletion timestamp < newer deletion timestamp
	if isPodDeleting(s[i]) && isPodDeleting(s[j]) && !s[i].DeletionTimestamp.Equal(s[j].DeletionTimestamp) {
		return s[i].DeletionTimestamp.Before(s[j].DeletionTimestamp)
	}
	// 6. Ready for empty time < less time < more time: the latest ready pod is
	// the smallest, so a longer-ready pod wins.
	if isPodReady(s[i]) && isPodReady(s[j]) && !podReadyTime(s[i]).Equal(podReadyTime(s[j])) {
		return afterOrZero(podReadyTime(s[i]), podReadyTime(s[j]))
	}
	// 7. Higher container restart counts < lower restart counts
	if res := compareMaxContainerRestarts(s[i], s[j]); res != nil {
		return *res
	}
	// 8. Empty creation time < newer pods < older pods
	if !s[i].CreationTimestamp.Equal(&s[j].CreationTimestamp) {
		return afterOrZero(&s[i].CreationTimestamp, &s[j].CreationTimestamp)
	}
	return false
}

func isPodReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func isPodDeleting(pod *corev1.Pod) bool {
	return pod.DeletionTimestamp != nil
}

func afterOrZero(t1, t2 *metav1.Time) bool {
	if t1.Time.IsZero() || t2.Time.IsZero() {
		return t1.Time.IsZero()
	}
	return t1.After(t2.Time)
}

func podReadyTime(pod *corev1.Pod) *metav1.Time {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return &condition.LastTransitionTime
		}
	}
	return &metav1.Time{}
}

func compareMaxContainerRestarts(pi, pj *corev1.Pod) *bool {
	regularI, sidecarI := maxContainerRestarts(pi)
	regularJ, sidecarJ := maxContainerRestarts(pj)
	if regularI != regularJ {
		result := regularI > regularJ
		return &result
	}
	if sidecarI != sidecarJ {
		result := sidecarI > sidecarJ
		return &result
	}
	return nil
}

func maxContainerRestarts(pod *corev1.Pod) (regularRestarts, sidecarRestarts int) {
	for _, status := range pod.Status.ContainerStatuses {
		if int(status.RestartCount) > regularRestarts {
			regularRestarts = int(status.RestartCount)
		}
	}
	sidecars := map[string]bool{}
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy != nil && *container.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecars[container.Name] = true
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if sidecars[status.Name] && int(status.RestartCount) > sidecarRestarts {
			sidecarRestarts = int(status.RestartCount)
		}
	}
	return regularRestarts, sidecarRestarts
}
