package testsupport

import (
	"fmt"
	"io"
	"net"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PodOptions describes a fake pod.
type PodOptions struct {
	Namespace string
	Name      string
	Phase     corev1.PodPhase
	Labels    map[string]string
	NodeName  string
	Ready     bool
	NotReady  bool
	Ports     []corev1.ContainerPort
	InitPorts []corev1.ContainerPort
	Deleting  bool
}

// Pod builds a fake pod.
func Pod(opts PodOptions) *corev1.Pod {
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.Phase == "" {
		opts.Phase = corev1.PodRunning
	}
	if opts.NodeName == "" && opts.Phase == corev1.PodRunning {
		opts.NodeName = "node-1"
	}
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         opts.Namespace,
			Name:              opts.Name,
			Labels:            opts.Labels,
			CreationTimestamp: metav1.Now(),
		},
		Spec: corev1.PodSpec{
			NodeName:       opts.NodeName,
			Containers:     []corev1.Container{{Name: "main", Image: "test", Ports: opts.Ports}},
			InitContainers: []corev1.Container{{Name: "init", Image: "test", Ports: opts.InitPorts}},
		},
		Status: corev1.PodStatus{Phase: opts.Phase},
	}
	if len(opts.InitPorts) == 0 {
		pod.Spec.InitContainers = nil
	}
	ready := corev1.ConditionFalse
	if !opts.NotReady && (opts.Ready || opts.Phase == corev1.PodRunning) {
		ready = corev1.ConditionTrue
	}
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodReady,
		Status:             ready,
		LastTransitionTime: metav1.Now(),
	}}
	if opts.Deleting {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
	}
	return pod
}

// ServiceOptions describes a fake service.
type ServiceOptions struct {
	Namespace string
	Name      string
	Selector  map[string]string
	ClusterIP string
	Ports     []corev1.ServicePort
}

// Service builds a fake service.
func Service(opts ServiceOptions) *corev1.Service {
	if opts.Namespace == "" {
		opts.Namespace = "default"
	}
	if opts.ClusterIP == "" {
		opts.ClusterIP = "10.0.0.10"
	}
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"},
		ObjectMeta: metav1.ObjectMeta{
			Namespace: opts.Namespace,
			Name:      opts.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector:  opts.Selector,
			ClusterIP: opts.ClusterIP,
			Ports:     opts.Ports,
		},
	}
}

// ServicePort is a compact service port builder.
func ServicePort(name string, port int32, targetPort intstr.IntOrString, protocol corev1.Protocol) corev1.ServicePort {
	if protocol == "" {
		protocol = corev1.ProtocolTCP
	}
	return corev1.ServicePort{Name: name, Port: port, TargetPort: targetPort, Protocol: protocol}
}

func labelSelector(selector map[string]string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: selector}
}

// Deployment builds a fake apps/v1 deployment.
func Deployment(namespace, name string, selector map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: labelSelector(selector)},
	}
}

// ReplicaSet builds a fake apps/v1 replicaset.
func ReplicaSet(namespace, name string, selector map[string]string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.ReplicaSetSpec{Selector: labelSelector(selector)},
	}
}

// StatefulSet builds a fake apps/v1 statefulset.
func StatefulSet(namespace, name string, selector map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.StatefulSetSpec{Selector: labelSelector(selector)},
	}
}

// DaemonSet builds a fake apps/v1 daemonset.
func DaemonSet(namespace, name string, selector map[string]string) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       appsv1.DaemonSetSpec{Selector: labelSelector(selector)},
	}
}

// Job builds a fake batch/v1 job.
func Job(namespace, name string, selector map[string]string) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta:   metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       batchv1.JobSpec{Selector: labelSelector(selector)},
	}
}

// ReplicationController builds a fake replicationcontroller.
func ReplicationController(namespace, name string, selector map[string]string) *corev1.ReplicationController {
	return &corev1.ReplicationController{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ReplicationController"},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.ReplicationControllerSpec{Selector: selector},
	}
}

// StartTCPServer starts a TCP server on 127.0.0.1 running handler for every
// accepted connection and returns its address plus a stop function.
func StartTCPServer(handler func(net.Conn)) (address string, stop func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("failed to start test TCP server: %s", err))
	}

	var wg sync.WaitGroup
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { _ = conn.Close() }()
				handler(conn)
			}()
		}
	}()

	stop = func() {
		_ = listener.Close()
		wg.Wait()
	}
	return listener.Addr().String(), stop
}

// EchoServer starts a TCP server that echoes every byte it receives.
func EchoServer() (address string, stop func()) {
	return StartTCPServer(func(conn net.Conn) {
		_, _ = io.Copy(conn, conn)
	})
}

// BannerServer starts a TCP server that writes banner on connect and then
// echoes every byte. It lets tests identify which backend served a connection.
func BannerServer(banner string) (address string, stop func()) {
	return StartTCPServer(func(conn net.Conn) {
		_, _ = io.WriteString(conn, banner)
		_, _ = io.Copy(conn, conn)
	})
}

// ClosedAddress returns an address on which nothing listens.
func ClosedAddress() string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(fmt.Sprintf("failed to reserve address: %s", err))
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}
