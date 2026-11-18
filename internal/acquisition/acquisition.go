// Package acquisition connects the Terraform-facing input model to the tunnel
// registry. It is shared by the data source and the ephemeral resource so both
// surfaces run exactly the same acquisition and healthcheck logic.
package acquisition

import (
	"context"
	"fmt"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/kube"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/surface"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

// Acquire obtains a reference-counted lease on the tunnel described by input.
// ctx must already carry the acquisition timeout; it covers target resolution,
// the initial upgrade and listener, and the optional healthcheck while leaving
// the established tunnel running on its own keeper context.
func Acquire(ctx context.Context, data *kube.ProviderData, input surface.Input) (*tunnel.Lease, error) {
	key := tunnel.NewKey(input.Namespace, input.Target, input.Port)

	lease, err := data.Registry.Acquire(ctx, key, func(ctx context.Context) (*tunnel.Tunnel, error) {
		return tunnel.NewTunnel(ctx, tunnel.Options{
			Client:     data.Client,
			Config:     data.RestConfig,
			Namespace:  input.Namespace,
			Target:     input.Target,
			Port:       input.Port,
			Timeout:    input.Timeout,
			LogContext: context.WithoutCancel(ctx),
		}), nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to acquire tunnel %s: %w", key.String(), err)
	}

	if input.Healthcheck == surface.HealthcheckTelnet {
		if err := lease.Tunnel().CheckTelnet(ctx); err != nil {
			lease.Release()
			return nil, fmt.Errorf("tunnel %s failed its %s healthcheck: %w", key.String(), surface.HealthcheckTelnet, err)
		}
	}

	return lease, nil
}
