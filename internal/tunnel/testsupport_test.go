package tunnel

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/testsupport"
)

func newFakeClient(t *testing.T, fake *testsupport.Fake) (kubernetes.Interface, *rest.Config) {
	t.Helper()
	config := &rest.Config{Host: fake.URL()}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("failed to build Kubernetes client: %s", err)
	}
	return client, config
}

type acquireOptions struct {
	namespace string
	target    string
	port      string
	timeout   time.Duration
}

func acquireForTest(t *testing.T, registry *Registry, client kubernetes.Interface, config *rest.Config, opts acquireOptions) (*Lease, error) {
	t.Helper()
	if opts.timeout == 0 {
		opts.timeout = 5 * time.Second
	}
	target, err := ParseTarget(opts.target)
	if err != nil {
		t.Fatalf("invalid test target: %s", err)
	}
	port, err := ParsePort(opts.port)
	if err != nil {
		t.Fatalf("invalid test port: %s", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opts.timeout)
	defer cancel()

	return registry.Acquire(ctx, NewKey(opts.namespace, target, port), func(ctx context.Context) (*Tunnel, error) {
		return NewTunnel(ctx, Options{
			Client:     client,
			Config:     config,
			Namespace:  opts.namespace,
			Target:     target,
			Port:       port,
			Timeout:    opts.timeout,
			LogContext: context.WithoutCancel(ctx),
		}), nil
	})
}

func newTestRegistry() *Registry {
	if testing.Short() {
		return NewRegistryWithBackoff(20*time.Millisecond, 50*time.Millisecond)
	}
	return NewRegistryWithBackoff(50*time.Millisecond, 200*time.Millisecond)
}
