package tunnel

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/testsupport"
)

// resolveContext returns a context that bounds a resolution attempt so an
// unexpected watch cannot hang the test.
func resolveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func mustPort(t *testing.T, raw string) ParsedPort {
	t.Helper()
	port, err := ParsePort(raw)
	if err != nil {
		t.Fatalf("invalid test port %q: %s", raw, err)
	}
	return port
}

func mustTarget(t *testing.T, raw string) Target {
	t.Helper()
	target, err := ParseTarget(raw)
	if err != nil {
		t.Fatalf("invalid test target %q: %s", raw, err)
	}
	return target
}

func TestResolvePodTarget(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()

	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "db",
		Ports: []corev1.ContainerPort{
			{Name: "postgres", ContainerPort: 5432, Protocol: corev1.ProtocolTCP},
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
		InitPorts: []corev1.ContainerPort{
			{Name: "init-port", ContainerPort: 9999, Protocol: corev1.ProtocolTCP},
		},
	}))
	client, _ := newFakeClient(t, fake)
	ctx := resolveContext(t)

	resolved, err := Resolve(ctx, client, "default", mustTarget(t, "db"), mustPort(t, "postgres"))
	if err != nil {
		t.Fatalf("Resolve named port failed: %s", err)
	}
	if resolved.PodName != "db" || resolved.TargetPort != 5432 {
		t.Fatalf("unexpected resolution: %#v", resolved)
	}

	resolved, err = Resolve(ctx, client, "default", mustTarget(t, "pod/db"), mustPort(t, "5433"))
	if err != nil {
		t.Fatalf("Resolve numeric port failed: %s", err)
	}
	if resolved.TargetPort != 5433 {
		t.Fatalf("unexpected numeric port: %d", resolved.TargetPort)
	}

	resolved, err = Resolve(ctx, client, "default", mustTarget(t, "pod/db"), mustPort(t, "init-port"))
	if err != nil {
		t.Fatalf("Resolve init container port failed: %s", err)
	}
	if resolved.TargetPort != 9999 {
		t.Fatalf("unexpected init container port: %d", resolved.TargetPort)
	}

	if _, err := Resolve(ctx, client, "default", mustTarget(t, "db"), mustPort(t, "dns")); err == nil || !strings.Contains(err.Error(), "UDP protocol is not supported") {
		t.Fatalf("expected UDP rejection, got %v", err)
	}
	if _, err := Resolve(ctx, client, "default", mustTarget(t, "db"), mustPort(t, "53")); err == nil || !strings.Contains(err.Error(), "UDP protocol is not supported") {
		t.Fatalf("expected UDP rejection for numeric port, got %v", err)
	}
	if _, err := Resolve(ctx, client, "default", mustTarget(t, "db"), mustPort(t, "missing")); err == nil || !strings.Contains(err.Error(), "does not have a named port") {
		t.Fatalf("expected named port error, got %v", err)
	}
	if _, err := Resolve(ctx, client, "default", mustTarget(t, "absent"), mustPort(t, "5432")); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got %v", err)
	}
}

func TestResolvePodTargetNotRunning(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "pending",
		Phase:     corev1.PodPending,
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	client, _ := newFakeClient(t, fake)

	_, err := Resolve(resolveContext(t), client, "default", mustTarget(t, "pending"), mustPort(t, "8080"))
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not running error, got %v", err)
	}
}

func TestResolveServiceTargetPortTranslation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		service   *corev1.Service
		podPorts  []corev1.ContainerPort
		requested string
		wantPort  int32
	}{
		{
			name: "named service port and named target port",
			service: testsupport.Service(testsupport.ServiceOptions{
				Name:     "db",
				Selector: map[string]string{"app": "db"},
				Ports: []corev1.ServicePort{
					testsupport.ServicePort("postgres", 5432, intstr.FromString("pg"), corev1.ProtocolTCP),
				},
			}),
			podPorts:  []corev1.ContainerPort{{Name: "pg", ContainerPort: 6432, Protocol: corev1.ProtocolTCP}},
			requested: "postgres",
			wantPort:  6432,
		},
		{
			name: "numeric target port",
			service: testsupport.Service(testsupport.ServiceOptions{
				Name:     "web",
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					testsupport.ServicePort("http", 80, intstr.FromInt(8080), corev1.ProtocolTCP),
				},
			}),
			podPorts:  []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
			requested: "80",
			wantPort:  8080,
		},
		{
			name: "omitted target port falls back to service port",
			service: testsupport.Service(testsupport.ServiceOptions{
				Name:     "web",
				Selector: map[string]string{"app": "web"},
				Ports: []corev1.ServicePort{
					testsupport.ServicePort("http", 80, intstr.IntOrString{}, corev1.ProtocolTCP),
				},
			}),
			podPorts:  []corev1.ContainerPort{{ContainerPort: 80, Protocol: corev1.ProtocolTCP}},
			requested: "http",
			wantPort:  80,
		},
		{
			name: "headless service ignores target port",
			service: testsupport.Service(testsupport.ServiceOptions{
				Name:      "headless",
				Selector:  map[string]string{"app": "web"},
				ClusterIP: corev1.ClusterIPNone,
				Ports: []corev1.ServicePort{
					testsupport.ServicePort("http", 80, intstr.FromString("not-on-pod"), corev1.ProtocolTCP),
				},
			}),
			podPorts:  []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
			requested: "80",
			wantPort:  80,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := testsupport.New()
			defer fake.Close()
			fake.SetServices(test.service)
			fake.SetPods(testsupport.Pod(testsupport.PodOptions{
				Namespace: test.service.Namespace,
				Name:      "backend",
				Labels:    test.service.Spec.Selector,
				Ports:     test.podPorts,
			}))
			client, _ := newFakeClient(t, fake)

			resolved, err := Resolve(resolveContext(t), client, test.service.Namespace, mustTarget(t, "service/"+test.service.Name), mustPort(t, test.requested))
			if err != nil {
				t.Fatalf("Resolve failed: %s", err)
			}
			if resolved.TargetPort != test.wantPort {
				t.Fatalf("target port = %d, want %d", resolved.TargetPort, test.wantPort)
			}
		})
	}
}

func TestResolveServiceErrors(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetServices(
		testsupport.Service(testsupport.ServiceOptions{Name: "no-selector"}),
		testsupport.Service(testsupport.ServiceOptions{
			Name:     "udp",
			Selector: map[string]string{"app": "udp"},
			Ports:    []corev1.ServicePort{testsupport.ServicePort("dns", 53, intstr.FromInt(53), corev1.ProtocolUDP)},
		}),
		testsupport.Service(testsupport.ServiceOptions{
			Name:     "web",
			Selector: map[string]string{"app": "web"},
			Ports:    []corev1.ServicePort{testsupport.ServicePort("http", 80, intstr.FromInt(8080), corev1.ProtocolTCP)},
		}),
	)
	fake.SetPods(
		testsupport.Pod(testsupport.PodOptions{
			Namespace: "default",
			Name:      "web-0",
			Labels:    map[string]string{"app": "web"},
			Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		}),
		testsupport.Pod(testsupport.PodOptions{
			Namespace: "default",
			Name:      "udp-0",
			Labels:    map[string]string{"app": "udp"},
			Ports:     []corev1.ContainerPort{{ContainerPort: 53, Protocol: corev1.ProtocolUDP}},
		}),
	)
	client, _ := newFakeClient(t, fake)
	ctx := resolveContext(t)

	if _, err := Resolve(ctx, client, "default", mustTarget(t, "service/no-selector"), mustPort(t, "80")); err == nil || !strings.Contains(err.Error(), "without a spec.selector") {
		t.Fatalf("expected selector error, got %v", err)
	}
	if _, err := Resolve(ctx, client, "default", mustTarget(t, "service/udp"), mustPort(t, "dns")); err == nil || !strings.Contains(err.Error(), "UDP protocol is not supported") {
		t.Fatalf("expected UDP error, got %v", err)
	}
	if _, err := Resolve(ctx, client, "default", mustTarget(t, "service/web"), mustPort(t, "9999")); err == nil || !strings.Contains(err.Error(), "does not have a service port") {
		t.Fatalf("expected missing service port error, got %v", err)
	}
}

func TestResolveWorkloadSelectors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		setup  func(*testsupport.Fake)
		target string
	}{
		{
			name: "deployment",
			setup: func(f *testsupport.Fake) {
				f.SetDeployments(testsupport.Deployment("default", "web", map[string]string{"app": "web"}))
			},
			target: "deployment/web",
		},
		{
			name: "replicaset",
			setup: func(f *testsupport.Fake) {
				f.SetReplicaSets(testsupport.ReplicaSet("default", "web", map[string]string{"app": "web"}))
			},
			target: "rs/web",
		},
		{
			name: "statefulset",
			setup: func(f *testsupport.Fake) {
				f.SetStatefulSets(testsupport.StatefulSet("default", "web", map[string]string{"app": "web"}))
			},
			target: "sts/web",
		},
		{
			name: "daemonset",
			setup: func(f *testsupport.Fake) {
				f.SetDaemonSets(testsupport.DaemonSet("default", "web", map[string]string{"app": "web"}))
			},
			target: "ds/web",
		},
		{
			name: "job",
			setup: func(f *testsupport.Fake) {
				f.SetJobs(testsupport.Job("default", "web", map[string]string{"app": "web"}))
			},
			target: "job/web",
		},
		{
			name: "replicationcontroller",
			setup: func(f *testsupport.Fake) {
				f.SetReplicationControllers(testsupport.ReplicationController("default", "web", map[string]string{"app": "web"}))
			},
			target: "rc/web",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fake := testsupport.New()
			defer fake.Close()
			test.setup(fake)
			fake.SetPods(testsupport.Pod(testsupport.PodOptions{
				Namespace: "default",
				Name:      "web-0",
				Labels:    map[string]string{"app": "web"},
				Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
			}))
			client, _ := newFakeClient(t, fake)

			resolved, err := Resolve(resolveContext(t), client, "default", mustTarget(t, test.target), mustPort(t, "http"))
			if err != nil {
				t.Fatalf("Resolve failed: %s", err)
			}
			if resolved.PodName != "web-0" || resolved.TargetPort != 8080 {
				t.Fatalf("unexpected resolution: %#v", resolved)
			}
		})
	}
}

func TestResolveWorkloadPicksBestPod(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetDeployments(testsupport.Deployment("default", "web", map[string]string{"app": "web"}))
	fake.SetPods(
		testsupport.Pod(testsupport.PodOptions{
			Namespace: "default",
			Name:      "web-pending",
			Phase:     corev1.PodPending,
			Labels:    map[string]string{"app": "web"},
			Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		}),
		testsupport.Pod(testsupport.PodOptions{
			Namespace: "default",
			Name:      "web-running",
			Labels:    map[string]string{"app": "web"},
			Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		}),
	)
	client, _ := newFakeClient(t, fake)

	resolved, err := Resolve(resolveContext(t), client, "default", mustTarget(t, "deployment/web"), mustPort(t, "8080"))
	if err != nil {
		t.Fatalf("Resolve failed: %s", err)
	}
	if resolved.PodName != "web-running" {
		t.Fatalf("selected %q, want the running pod", resolved.PodName)
	}
}

func TestResolveWorkloadPendingPodDoesNotWait(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetDeployments(testsupport.Deployment("default", "web", map[string]string{"app": "web"}))
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "web-pending",
		Phase:     corev1.PodPending,
		Labels:    map[string]string{"app": "web"},
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	client, _ := newFakeClient(t, fake)

	start := time.Now()
	_, err := Resolve(resolveContext(t), client, "default", mustTarget(t, "deployment/web"), mustPort(t, "8080"))
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not running error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("resolution with a pending pod took %s; it must not wait for Running", elapsed)
	}
}

func TestResolveWorkloadWatchesForFirstPod(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetDeployments(testsupport.Deployment("default", "web", map[string]string{"app": "web"}))
	fake.SetPods()
	client, _ := newFakeClient(t, fake)

	go func() {
		time.Sleep(75 * time.Millisecond)
		fake.EmitPod(testsupport.Pod(testsupport.PodOptions{
			Namespace: "default",
			Name:      "web-1",
			Labels:    map[string]string{"app": "web"},
			Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		}))
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resolved, err := Resolve(ctx, client, "default", mustTarget(t, "deployment/web"), mustPort(t, "8080"))
	if err != nil {
		t.Fatalf("Resolve failed: %s", err)
	}
	if resolved.PodName != "web-1" {
		t.Fatalf("selected %q, want web-1", resolved.PodName)
	}
	if fake.Count("pod.watch") == 0 {
		t.Fatal("expected the resolver to watch for a pod")
	}
}

func TestActivePodsOrdering(t *testing.T) {
	t.Parallel()

	pendingUnassigned := testsupport.Pod(testsupport.PodOptions{Name: "a", Phase: corev1.PodPending, NodeName: "", NotReady: true})
	runningNotReady := testsupport.Pod(testsupport.PodOptions{Name: "b", Phase: corev1.PodRunning, NotReady: true})
	runningReady := testsupport.Pod(testsupport.PodOptions{Name: "c", Phase: corev1.PodRunning, Ready: true})
	deletingReady := testsupport.Pod(testsupport.PodOptions{Name: "d", Phase: corev1.PodRunning, Ready: true, Deleting: true})

	pods := ActivePods{pendingUnassigned, runningNotReady, runningReady, deletingReady}
	sort.Sort(pods)
	if pods[len(pods)-1].Name != "c" {
		t.Fatalf("best pod = %q, want c", pods[len(pods)-1].Name)
	}
	if pods[0].Name != "a" {
		t.Fatalf("worst pod = %q, want a", pods[0].Name)
	}
}
