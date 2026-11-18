package tunnel

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/testsupport"
)

func setupPodTunnel(t *testing.T) (*testsupport.Fake, string, func()) {
	t.Helper()
	echoAddress, stopEcho := testsupport.EchoServer()
	fake := testsupport.New()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, echoAddress)
	return fake, echoAddress, func() {
		fake.Close()
		stopEcho()
	}
}

func TestRegistrySharesConcurrentMisses(t *testing.T) {
	t.Parallel()

	fake, _, cleanup := setupPodTunnel(t)
	defer cleanup()
	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()

	target := mustTarget(t, "pod/echo")
	port := mustPort(t, "8080")
	key := NewKey("default", target, port)

	const goroutines = 20
	var creates atomic.Int32
	var wg sync.WaitGroup
	leases := make([]*Lease, goroutines)
	errs := make([]error, goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			lease, err := registry.Acquire(ctx, key, func(ctx context.Context) (*Tunnel, error) {
				creates.Add(1)
				return NewTunnel(ctx, Options{
					Client:     client,
					Config:     config,
					Namespace:  "default",
					Target:     target,
					Port:       port,
					Timeout:    10 * time.Second,
					LogContext: context.WithoutCancel(ctx),
				}), nil
			})
			leases[index] = lease
			errs[index] = err
		}(i)
	}
	wg.Wait()

	if got := creates.Load(); got != 1 {
		t.Fatalf("create was called %d times, want 1", got)
	}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("acquire %d failed: %s", i, err)
		}
		if leases[i] == nil {
			t.Fatalf("acquire %d returned a nil lease", i)
		}
		if leases[i].Tunnel().LocalPort() != leases[0].Tunnel().LocalPort() {
			t.Fatalf("acquire %d got local port %d, want %d", i, leases[i].Tunnel().LocalPort(), leases[0].Tunnel().LocalPort())
		}
	}
	if fake.SPDYAttempts() != 1 {
		t.Fatalf("SPDY attempts = %d, want 1", fake.SPDYAttempts())
	}

	for _, lease := range leases {
		lease.Release()
	}
	if registry.Len() != 0 {
		t.Fatalf("registry has %d entries after releasing every lease", registry.Len())
	}
}

func TestRegistryFailedAcquisitionIsNotCached(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	pod := testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Phase:     corev1.PodPending,
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	})
	fake.SetPods(pod)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()

	if _, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/echo",
		port:      "8080",
	}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("expected not running error, got %v", err)
	}
	if registry.Len() != 0 {
		t.Fatalf("failed acquisition was cached")
	}

	pod.Status.Phase = corev1.PodRunning
	pod.Spec.NodeName = "node-1"
	fake.SetPods(pod)
	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()
	fake.SetBackend(8080, echoAddress)

	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/echo",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("second acquisition failed: %s", err)
	}
	defer lease.Release()

	if got := readExactly(t, lease.Tunnel().Hostport(), "retry"); got != "retry" {
		t.Fatalf("echo = %q", got)
	}
	if fake.Count("pod.get") < 2 {
		t.Fatalf("expected the target to be re-read after the failed acquisition, pod.get=%d", fake.Count("pod.get"))
	}
}

func TestRegistryReferenceCounting(t *testing.T) {
	t.Parallel()

	fake, _, cleanup := setupPodTunnel(t)
	defer cleanup()
	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()

	options := acquireOptions{namespace: "default", target: "pod/echo", port: "8080"}
	first, err := acquireForTest(t, registry, client, config, options)
	if err != nil {
		t.Fatalf("first acquisition failed: %s", err)
	}
	second, err := acquireForTest(t, registry, client, config, options)
	if err != nil {
		t.Fatalf("second acquisition failed: %s", err)
	}

	if fake.SPDYAttempts() != 1 {
		t.Fatalf("expected one shared forwarding upgrade, got %d", fake.SPDYAttempts())
	}
	hostport := first.Tunnel().Hostport()

	first.Release()
	first.Release() // idempotent
	if registry.Len() != 1 {
		t.Fatalf("registry lost the entry while a lease was still held")
	}
	if got := readExactly(t, hostport, "still shared"); got != "still shared" {
		t.Fatalf("echo after first release = %q", got)
	}

	second.Release()
	second.Release()
	waitForListenerClosed(t, hostport)
	if registry.Len() != 0 {
		t.Fatalf("registry has %d entries after final release", registry.Len())
	}
}

func TestRegistryBacksOffOnRepeatedReconnectFailures(t *testing.T) {
	t.Parallel()

	fake, _, cleanup := setupPodTunnel(t)
	defer cleanup()
	client, config := newFakeClient(t, fake)
	registry := NewRegistryWithBackoff(50*time.Millisecond, 200*time.Millisecond)

	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/echo",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	// Make every re-resolution fail, then drop the connection.
	fake.DeletePod("default", "echo")
	before := fake.Count("pod.get")
	fake.KillPortForwardConnections()

	deadline := time.Now().Add(5 * time.Second)
	for fake.Count("pod.get") < before+3 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	attempts := fake.Count("pod.get") - before
	if attempts < 3 {
		t.Fatalf("expected at least 3 re-resolution attempts, got %d", attempts)
	}
	// Backoff starts at 50ms and doubles; 500ms cannot accommodate dozens of
	// attempts, so this catches a hot retry loop.
	time.Sleep(300 * time.Millisecond)
	after := fake.Count("pod.get") - before
	if after > attempts+20 {
		t.Fatalf("reconnect appears to spin: %d attempts grew to %d within 300ms", attempts, after)
	}
}

func TestRegistryConcurrentSameKeyReturnsOneTunnel(t *testing.T) {
	t.Parallel()

	fake, _, cleanup := setupPodTunnel(t)
	defer cleanup()
	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()

	const goroutines = 10
	var wg sync.WaitGroup
	results := make([]string, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			lease, err := acquireForTest(t, registry, client, config, acquireOptions{
				namespace: "default",
				target:    "pod/echo",
				port:      "8080",
			})
			if err != nil {
				results[index] = fmt.Sprintf("error: %s", err)
				return
			}
			defer lease.Release()
			result, err := readEcho(lease.Tunnel().Hostport(), "shared")
			if err != nil {
				results[index] = err.Error()
				return
			}
			results[index] = result
		}(i)
	}
	wg.Wait()

	for i, result := range results {
		if result != "shared" {
			t.Fatalf("goroutine %d got %q", i, result)
		}
	}
}

func waitForListenerClosed(t *testing.T, hostport string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", hostport, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("listener %s still accepts connections", hostport)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
