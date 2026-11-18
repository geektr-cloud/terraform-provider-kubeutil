// Package testsupport provides a hermetic fake Kubernetes API server that also
// speaks the portforward SPDY upgrade protocol. Tests use it to exercise real
// client-go forwarding against local TCP backends without a cluster.
package testsupport

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/portforward"
	streamhttp "k8s.io/streaming/pkg/httpstream"
	streamspdy "k8s.io/streaming/pkg/httpstream/spdy"
)

// Fake is an httptest-backed Kubernetes API and portforward server.
type Fake struct {
	Server *httptest.Server

	mu                     sync.Mutex
	pods                   map[string]*corev1.Pod
	services               map[string]*corev1.Service
	deployments            map[string]*appsv1.Deployment
	replicasets            map[string]*appsv1.ReplicaSet
	statefulsets           map[string]*appsv1.StatefulSet
	daemonsets             map[string]*appsv1.DaemonSet
	jobs                   map[string]*batchv1.Job
	replicationcontrollers map[string]*corev1.ReplicationController
	backends               map[uint16]string
	dialFunc               func(pod string, containerPort uint16) (net.Conn, error)
	watchCh                chan watch.Event
	counts                 map[string]int

	connectionsMu sync.Mutex
	connections   map[streamhttp.Connection]struct{}

	websocketAttempts atomic.Int64
	spdyAttempts      atomic.Int64
	spdyUpgrades      atomic.Int64
	portforwards      atomic.Int64
}

// New starts a fake API server. Close it with Close.
func New() *Fake {
	f := &Fake{
		pods:                   map[string]*corev1.Pod{},
		services:               map[string]*corev1.Service{},
		deployments:            map[string]*appsv1.Deployment{},
		replicasets:            map[string]*appsv1.ReplicaSet{},
		statefulsets:           map[string]*appsv1.StatefulSet{},
		daemonsets:             map[string]*appsv1.DaemonSet{},
		jobs:                   map[string]*batchv1.Job{},
		replicationcontrollers: map[string]*corev1.ReplicationController{},
		backends:               map[uint16]string{},
		watchCh:                make(chan watch.Event, 64),
		counts:                 map[string]int{},
		connections:            map[streamhttp.Connection]struct{}{},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	return f
}

// Close shuts down the HTTP server.
func (f *Fake) Close() {
	f.KillPortForwardConnections()
	f.Server.Close()
}

// URL is the API server base URL.
func (f *Fake) URL() string {
	return f.Server.URL
}

// Kubeconfig renders a minimal kubeconfig pointing at the fake server.
func (f *Fake) Kubeconfig() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user:
    token: fake-token
`, f.URL())
}

// SetPods replaces the pod store.
func (f *Fake) SetPods(pods ...*corev1.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods = map[string]*corev1.Pod{}
	for _, pod := range pods {
		f.pods[objectKey(pod.Namespace, pod.Name)] = pod.DeepCopy()
	}
}

// AddPod inserts or replaces a pod.
func (f *Fake) AddPod(pod *corev1.Pod) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pods[objectKey(pod.Namespace, pod.Name)] = pod.DeepCopy()
}

// DeletePod removes a pod.
func (f *Fake) DeletePod(namespace, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.pods, objectKey(namespace, name))
}

// EmitPod adds a pod and publishes an Added watch event.
func (f *Fake) EmitPod(pod *corev1.Pod) {
	f.AddPod(pod)
	f.watchCh <- watch.Event{Type: watch.Added, Object: pod.DeepCopy()}
}

// SetServices replaces the service store.
func (f *Fake) SetServices(services ...*corev1.Service) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.services = map[string]*corev1.Service{}
	for _, service := range services {
		f.services[objectKey(service.Namespace, service.Name)] = service.DeepCopy()
	}
}

// SetDeployments replaces the deployment store.
func (f *Fake) SetDeployments(items ...*appsv1.Deployment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deployments = map[string]*appsv1.Deployment{}
	for _, item := range items {
		f.deployments[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetReplicaSets replaces the replicaset store.
func (f *Fake) SetReplicaSets(items ...*appsv1.ReplicaSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replicasets = map[string]*appsv1.ReplicaSet{}
	for _, item := range items {
		f.replicasets[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetStatefulSets replaces the statefulset store.
func (f *Fake) SetStatefulSets(items ...*appsv1.StatefulSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statefulsets = map[string]*appsv1.StatefulSet{}
	for _, item := range items {
		f.statefulsets[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetDaemonSets replaces the daemonset store.
func (f *Fake) SetDaemonSets(items ...*appsv1.DaemonSet) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.daemonsets = map[string]*appsv1.DaemonSet{}
	for _, item := range items {
		f.daemonsets[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetJobs replaces the job store.
func (f *Fake) SetJobs(items ...*batchv1.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = map[string]*batchv1.Job{}
	for _, item := range items {
		f.jobs[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetReplicationControllers replaces the replicationcontroller store.
func (f *Fake) SetReplicationControllers(items ...*corev1.ReplicationController) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replicationcontrollers = map[string]*corev1.ReplicationController{}
	for _, item := range items {
		f.replicationcontrollers[objectKey(item.Namespace, item.Name)] = item.DeepCopy()
	}
}

// SetBackend maps a container port to a local TCP address for the portforward
// protocol server.
func (f *Fake) SetBackend(containerPort uint16, address string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backends[containerPort] = address
}

// SetDialFunc overrides backend dialing for the portforward server.
func (f *Fake) SetDialFunc(fn func(pod string, containerPort uint16) (net.Conn, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialFunc = fn
}

// KillPortForwardConnections closes every active server-side forwarding
// connection, simulating a lost pod connection.
func (f *Fake) KillPortForwardConnections() {
	f.connectionsMu.Lock()
	connections := make([]streamhttp.Connection, 0, len(f.connections))
	for connection := range f.connections {
		connections = append(connections, connection)
	}
	f.connections = map[streamhttp.Connection]struct{}{}
	f.connectionsMu.Unlock()

	for _, connection := range connections {
		_ = connection.Close()
	}
}

// Count returns how many requests matched a kind such as "pod.get",
// "pod.list", "pod.watch", or "service.get".
func (f *Fake) Count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[kind]
}

// WebSocketAttempts is the number of WebSocket upgrade attempts.
func (f *Fake) WebSocketAttempts() int64 { return f.websocketAttempts.Load() }

// SPDYAttempts is the number of SPDY upgrade requests received. It is
// incremented before the upgrade completes, so it is safe to assert as soon as
// a tunnel reports readiness.
func (f *Fake) SPDYAttempts() int64 { return f.spdyAttempts.Load() }

// SPDYUpgrades is the number of successful SPDY upgrades.
func (f *Fake) SPDYUpgrades() int64 { return f.spdyUpgrades.Load() }

// PortForwardRequests is the number of portforward endpoint requests.
func (f *Fake) PortForwardRequests() int64 { return f.portforwards.Load() }

func (f *Fake) count(kind string) {
	f.mu.Lock()
	f.counts[kind]++
	f.mu.Unlock()
}

func objectKey(namespace, name string) string {
	return namespace + "/" + name
}

func (f *Fake) serveHTTP(w http.ResponseWriter, r *http.Request) {
	segments := splitPath(r.URL.Path)

	if len(segments) >= 3 && segments[len(segments)-1] == "portforward" {
		f.portforwards.Add(1)
		f.handlePortForward(w, r)
		return
	}

	switch {
	case len(segments) == 5 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "namespaces" && segments[4] == "pods":
		f.count("pod.list")
		f.handlePodList(w, r, segments[3])
	case len(segments) == 6 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "namespaces" && segments[4] == "pods":
		f.count("pod.get")
		f.handlePodGet(w, r, segments[3], segments[5])
	case len(segments) == 6 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "namespaces" && segments[4] == "services":
		f.count("service.get")
		f.handleServiceGet(w, r, segments[3], segments[5])
	case len(segments) == 6 && segments[0] == "api" && segments[1] == "v1" && segments[2] == "namespaces" && segments[4] == "replicationcontrollers":
		f.count("replicationcontroller.get")
		f.handleReplicationControllerGet(w, r, segments[3], segments[5])
	case len(segments) == 7 && segments[0] == "apis" && segments[1] == "apps" && segments[2] == "v1" && segments[3] == "namespaces":
		f.handleAppsGet(w, r, segments[4], segments[5], segments[6])
	case len(segments) == 7 && segments[0] == "apis" && segments[1] == "batch" && segments[2] == "v1" && segments[3] == "namespaces" && segments[5] == "jobs":
		f.count("job.get")
		f.handleJobGet(w, r, segments[4], segments[6])
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("unexpected path %q", r.URL.Path))
	}
}

func (f *Fake) handlePodGet(w http.ResponseWriter, _ *http.Request, namespace, name string) {
	f.mu.Lock()
	pod, ok := f.pods[objectKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("pods %q not found", name))
		return
	}
	pod = pod.DeepCopy()
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	writeJSON(w, http.StatusOK, pod)
}

func (f *Fake) handlePodList(w http.ResponseWriter, r *http.Request, namespace string) {
	if r.URL.Query().Get("watch") == "true" {
		f.count("pod.watch")
		f.handlePodWatch(w, r, namespace)
		return
	}

	selector, err := parseSelector(r.URL.Query().Get("labelSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}

	f.mu.Lock()
	items := make([]corev1.Pod, 0, len(f.pods))
	for _, pod := range f.pods {
		if pod.Namespace != namespace {
			continue
		}
		if selector != nil && !selector.Matches(labels.Set(pod.Labels)) {
			continue
		}
		items = append(items, *pod.DeepCopy())
	}
	f.mu.Unlock()

	writeJSON(w, http.StatusOK, &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		ListMeta: metav1.ListMeta{ResourceVersion: "1"},
		Items:    items,
	})
}

func (f *Fake) handlePodWatch(w http.ResponseWriter, r *http.Request, namespace string) {
	selector, err := parseSelector(r.URL.Query().Get("labelSelector"))
	if err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "streaming is not supported")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-f.watchCh:
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || pod.Namespace != namespace {
				continue
			}
			if selector != nil && !selector.Matches(labels.Set(pod.Labels)) {
				continue
			}
			copied := pod.DeepCopy()
			copied.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			if err := json.NewEncoder(w).Encode(watchEvent{Type: event.Type, Object: copied}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (f *Fake) handleServiceGet(w http.ResponseWriter, _ *http.Request, namespace, name string) {
	f.mu.Lock()
	service, ok := f.services[objectKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("services %q not found", name))
		return
	}
	service = service.DeepCopy()
	service.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	writeJSON(w, http.StatusOK, service)
}

func (f *Fake) handleReplicationControllerGet(w http.ResponseWriter, _ *http.Request, namespace, name string) {
	f.mu.Lock()
	item, ok := f.replicationcontrollers[objectKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("replicationcontrollers %q not found", name))
		return
	}
	item = item.DeepCopy()
	item.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ReplicationController"}
	writeJSON(w, http.StatusOK, item)
}

func (f *Fake) handleAppsGet(w http.ResponseWriter, _ *http.Request, namespace, resource, name string) {
	key := objectKey(namespace, name)

	switch resource {
	case "deployments":
		f.count("deployment.get")
		f.mu.Lock()
		item, ok := f.deployments[key]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("deployments %q not found", name))
			return
		}
		copied := item.DeepCopy()
		copied.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
		writeJSON(w, http.StatusOK, copied)
	case "replicasets":
		f.count("replicaset.get")
		f.mu.Lock()
		item, ok := f.replicasets[key]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("replicasets %q not found", name))
			return
		}
		copied := item.DeepCopy()
		copied.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "ReplicaSet"}
		writeJSON(w, http.StatusOK, copied)
	case "statefulsets":
		f.count("statefulset.get")
		f.mu.Lock()
		item, ok := f.statefulsets[key]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("statefulsets %q not found", name))
			return
		}
		copied := item.DeepCopy()
		copied.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}
		writeJSON(w, http.StatusOK, copied)
	case "daemonsets":
		f.count("daemonset.get")
		f.mu.Lock()
		item, ok := f.daemonsets[key]
		f.mu.Unlock()
		if !ok {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("daemonsets %q not found", name))
			return
		}
		copied := item.DeepCopy()
		copied.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}
		writeJSON(w, http.StatusOK, copied)
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("unexpected apps resource %q", resource))
	}
}

func (f *Fake) handleJobGet(w http.ResponseWriter, _ *http.Request, namespace, name string) {
	f.mu.Lock()
	item, ok := f.jobs[objectKey(namespace, name)]
	f.mu.Unlock()
	if !ok {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("jobs %q not found", name))
		return
	}
	item = item.DeepCopy()
	item.TypeMeta = metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}
	writeJSON(w, http.StatusOK, item)
}

func parseSelector(raw string) (labels.Selector, error) {
	if raw == "" {
		return nil, nil
	}
	return labels.Parse(raw)
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	writeJSON(w, code, &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   reason,
		Code:     int32(code),
	})
}

type watchEvent struct {
	Type   watch.EventType `json:"type"`
	Object any             `json:"object"`
}

func splitPath(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

func (f *Fake) handlePortForward(w http.ResponseWriter, r *http.Request) {
	segments := splitPath(r.URL.Path)
	// .../namespaces/<ns>/pods/<pod>/portforward
	if len(segments) < 2 {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "missing pod name")
		return
	}
	namespace := segments[len(segments)-4]
	pod := segments[len(segments)-2]

	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") != "" {
		f.websocketAttempts.Add(1)
		http.Error(w, "websocket upgrades are not supported by the hermetic test server", http.StatusBadRequest)
		return
	}

	f.spdyAttempts.Add(1)
	if _, err := streamhttp.Handshake(r, w, []string{portforward.PortForwardProtocolV1Name}); err != nil {
		return
	}

	session := &forwardSession{
		fake:      f,
		namespace: namespace,
		pod:       pod,
		pairs:     map[string]*forwardPair{},
	}

	upgrader := streamspdy.NewResponseUpgrader()
	connection := upgrader.UpgradeResponse(w, r, session.newStream)
	if connection == nil {
		return
	}
	f.spdyUpgrades.Add(1)

	f.connectionsMu.Lock()
	f.connections[connection] = struct{}{}
	f.connectionsMu.Unlock()

	defer func() {
		f.connectionsMu.Lock()
		delete(f.connections, connection)
		f.connectionsMu.Unlock()
		_ = connection.Close()
	}()

	<-connection.CloseChan()
}

type forwardSession struct {
	fake      *Fake
	namespace string
	pod       string

	mu    sync.Mutex
	pairs map[string]*forwardPair
}

type forwardPair struct {
	port       uint16
	errStream  streamhttp.Stream
	dataStream streamhttp.Stream
	started    bool
}

func (s *forwardSession) newStream(stream streamhttp.Stream, _ <-chan struct{}) error {
	headers := stream.Headers()
	requestID := headers.Get(corev1.PortForwardRequestIDHeader)
	if requestID == "" {
		requestID = headers.Get(corev1.PortHeader) + "/" + headers.Get(corev1.StreamType)
	}
	port, _ := strconv.ParseUint(headers.Get(corev1.PortHeader), 10, 16)

	s.mu.Lock()
	pair := s.pairs[requestID]
	if pair == nil {
		pair = &forwardPair{}
		s.pairs[requestID] = pair
	}
	pair.port = uint16(port)
	switch headers.Get(corev1.StreamType) {
	case corev1.StreamTypeError:
		pair.errStream = stream
	case corev1.StreamTypeData:
		pair.dataStream = stream
	}
	start := pair.errStream != nil && pair.dataStream != nil && !pair.started
	if start {
		pair.started = true
	}
	s.mu.Unlock()

	if start {
		go s.forward(requestID, pair)
	}
	return nil
}

func (s *forwardSession) forward(requestID string, pair *forwardPair) {
	defer func() {
		s.mu.Lock()
		delete(s.pairs, requestID)
		s.mu.Unlock()
		_ = pair.errStream.Close()
		_ = pair.dataStream.Close()
	}()

	backend, err := s.fake.dialBackend(s.pod, pair.port)
	if err != nil {
		_, _ = fmt.Fprintf(pair.errStream, "error dialing backend for pod %s port %d: %v", s.pod, pair.port, err)
		_ = pair.errStream.Close()
		_ = pair.dataStream.Close()
		return
	}
	defer func() { _ = backend.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(backend, pair.dataStream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(pair.dataStream, backend)
		done <- struct{}{}
	}()

	<-done
	_ = backend.Close()
	_ = pair.dataStream.Close()
	_ = pair.errStream.Close()
}

func (f *Fake) dialBackend(pod string, port uint16) (net.Conn, error) {
	f.mu.Lock()
	dialFunc := f.dialFunc
	address, ok := f.backends[port]
	f.mu.Unlock()

	if dialFunc != nil {
		return dialFunc(pod, port)
	}
	if !ok {
		return nil, fmt.Errorf("no backend registered for container port %d", port)
	}
	return net.Dial("tcp", address)
}
