package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/testsupport"
)

func readEcho(address, payload string) (string, error) {
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("failed to connect to %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return "", fmt.Errorf("failed to set deadline: %w", err)
	}
	if _, err := conn.Write([]byte(payload)); err != nil {
		return "", fmt.Errorf("failed to write payload: %w", err)
	}
	buffer := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buffer); err != nil {
		return "", fmt.Errorf("failed to read echo: %w", err)
	}
	return string(buffer), nil
}

func readExactly(t *testing.T, address string, payload string) string {
	t.Helper()
	result, err := readEcho(address, payload)
	if err != nil {
		t.Fatalf("%s", err)
	}
	return result
}

func readBanner(t *testing.T, address string, size int) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to %s: %s", address, err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("failed to set deadline: %s", err)
	}
	buffer := make([]byte, size)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		t.Fatalf("failed to read banner: %s", err)
	}
	return string(buffer)
}

func TestForwardPodTunnelDataFlow(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/echo",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	if got, want := lease.Tunnel().Hostport(), net.JoinHostPort("127.0.0.1", strconv.Itoa(int(lease.Tunnel().LocalPort()))); got != want {
		t.Fatalf("hostport = %q, want %q", got, want)
	}
	if lease.Tunnel().LocalPort() == 0 {
		t.Fatal("local port was not assigned")
	}

	if got := readExactly(t, lease.Tunnel().Hostport(), "hello tunnel"); got != "hello tunnel" {
		t.Fatalf("echo = %q, want %q", got, "hello tunnel")
	}

	if fake.PortForwardRequests() == 0 {
		t.Fatal("expected a portforward request")
	}
	if fake.WebSocketAttempts() == 0 {
		t.Fatal("expected the WebSocket dialer to be attempted first")
	}
	if fake.SPDYAttempts() == 0 {
		t.Fatal("expected a SPDY fallback upgrade")
	}
}

func TestForwardServiceTargetPortTranslation(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetServices(testsupport.Service(testsupport.ServiceOptions{
		Namespace: "default",
		Name:      "echo",
		Selector:  map[string]string{"app": "echo"},
		Ports: []corev1.ServicePort{
			testsupport.ServicePort("web", 80, intstr.FromString("container-port"), corev1.ProtocolTCP),
		},
	}))
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo-0",
		Labels:    map[string]string{"app": "echo"},
		Ports: []corev1.ContainerPort{
			{Name: "container-port", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		},
	}))
	fake.SetBackend(8080, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "service/echo",
		port:      "web",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	if got := readExactly(t, lease.Tunnel().Hostport(), "translated"); got != "translated" {
		t.Fatalf("echo = %q, want %q", got, "translated")
	}
	if fake.Count("service.get") == 0 || fake.Count("pod.list") == 0 {
		t.Fatalf("expected service and pod reads, got service.get=%d pod.list=%d", fake.Count("service.get"), fake.Count("pod.list"))
	}
}

func TestForwardNamedPodPort(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "db",
		Ports: []corev1.ContainerPort{
			{Name: "postgres", ContainerPort: 5432, Protocol: corev1.ProtocolTCP},
		},
	}))
	fake.SetBackend(5432, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "db",
		port:      "postgres",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	if got := readExactly(t, lease.Tunnel().Hostport(), "named"); got != "named" {
		t.Fatalf("echo = %q, want %q", got, "named")
	}
}

func TestForwardRejectsUDPBeforeForwarding(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "dns",
		Ports: []corev1.ContainerPort{
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
		},
	}))

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	_, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/dns",
		port:      "dns",
	})
	if err == nil || !strings.Contains(err.Error(), "UDP protocol is not supported") {
		t.Fatalf("expected UDP rejection, got %v", err)
	}
	if fake.PortForwardRequests() != 0 {
		t.Fatalf("portforward endpoint was called %d times; UDP rejection must happen before forwarding", fake.PortForwardRequests())
	}
	if registry.Len() != 0 {
		t.Fatalf("failed acquisition was cached: registry has %d entries", registry.Len())
	}
}

func TestForwardTelnetHealthcheck(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "healthy",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/healthy",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lease.Tunnel().CheckTelnet(ctx); err != nil {
		t.Fatalf("telnet healthcheck failed against an echo backend: %s", err)
	}
}

func TestForwardTelnetHealthcheckDetectsRejectedPort(t *testing.T) {
	t.Parallel()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "refusing",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, testsupport.ClosedAddress())

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/refusing",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("acquisition without healthcheck should succeed: %s", err)
	}
	defer lease.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = lease.Tunnel().CheckTelnet(ctx)
	if err == nil {
		t.Fatal("telnet healthcheck passed although the remote port refuses connections")
	}
}

func TestForwardReconnectPreservesLocalPortAndReresolves(t *testing.T) {
	t.Parallel()

	firstAddress, stopFirst := testsupport.BannerServer("one\n")
	defer stopFirst()
	secondAddress, stopSecond := testsupport.BannerServer("two\n")
	defer stopSecond()

	fake := testsupport.New()
	defer fake.Close()
	service := testsupport.Service(testsupport.ServiceOptions{
		Namespace: "default",
		Name:      "echo",
		Selector:  map[string]string{"app": "echo"},
		Ports: []corev1.ServicePort{
			testsupport.ServicePort("http", 80, intstr.FromInt(8080), corev1.ProtocolTCP),
		},
	})
	fake.SetServices(service)
	fake.AddPod(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo-0",
		Labels:    map[string]string{"app": "echo"},
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, firstAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "service/echo",
		port:      "http",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	localPort := lease.Tunnel().LocalPort()
	hostport := lease.Tunnel().Hostport()
	if got := readBanner(t, hostport, 4); got != "one\n" {
		t.Fatalf("first backend = %q", got)
	}

	// Change the service's target port and register a distinguishable backend.
	service.Spec.Ports[0].TargetPort = intstr.FromInt(9090)
	fake.SetServices(service)
	fake.SetBackend(9090, secondAddress)

	servicesBefore := fake.Count("service.get")
	fake.KillPortForwardConnections()

	deadline := time.Now().Add(10 * time.Second)
	var banner string
	for time.Now().Before(deadline) {
		banner = tryReadBanner(hostport, 4)
		if banner == "two\n" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if banner != "two\n" {
		t.Fatalf("tunnel did not recover with the re-resolved target port; last banner %q", banner)
	}

	if lease.Tunnel().LocalPort() != localPort {
		t.Fatalf("local port changed from %d to %d after reconnect", localPort, lease.Tunnel().LocalPort())
	}
	if fake.Count("service.get") <= servicesBefore {
		t.Fatal("expected the target to be re-resolved on reconnect")
	}
}

func TestForwardCloseRemovesListener(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()
	lease, err := acquireForTest(t, registry, client, config, acquireOptions{
		namespace: "default",
		target:    "pod/echo",
		port:      "8080",
	})
	if err != nil {
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	hostport := lease.Tunnel().Hostport()
	if got := readExactly(t, hostport, "before close"); got != "before close" {
		t.Fatalf("echo = %q", got)
	}

	lease.Release()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", hostport, 200*time.Millisecond)
		if dialErr != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("listener on %s still accepts connections after release", hostport)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if registry.Len() != 0 {
		t.Fatalf("registry still has %d entries after release", registry.Len())
	}
}

func TestForwardRequestContextDoesNotOwnTunnel(t *testing.T) {
	t.Parallel()

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := testsupport.New()
	defer fake.Close()
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Ports:     []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(8080, echoAddress)

	client, config := newFakeClient(t, fake)
	registry := newTestRegistry()

	ctx, cancel := context.WithCancel(context.Background())
	lease, err := registry.Acquire(ctx, NewKey("default", mustTarget(t, "pod/echo"), mustPort(t, "8080")), func(ctx context.Context) (*Tunnel, error) {
		return NewTunnel(ctx, Options{
			Client:     client,
			Config:     config,
			Namespace:  "default",
			Target:     mustTarget(t, "pod/echo"),
			Port:       mustPort(t, "8080"),
			Timeout:    5 * time.Second,
			LogContext: context.WithoutCancel(ctx),
		}), nil
	})
	if err != nil {
		cancel()
		t.Fatalf("failed to acquire tunnel: %s", err)
	}
	defer lease.Release()

	// The RPC context ends here; the tunnel must survive.
	cancel()
	time.Sleep(100 * time.Millisecond)

	if got := readExactly(t, lease.Tunnel().Hostport(), "still alive"); got != "still alive" {
		t.Fatalf("echo after request context cancellation = %q", got)
	}
}

func tryReadBanner(address string, size int) string {
	conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	buffer := make([]byte, size)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		return ""
	}
	return string(buffer)
}
