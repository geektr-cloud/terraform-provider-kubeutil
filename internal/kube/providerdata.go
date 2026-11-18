// Package kube holds the provider-instance-scoped Kubernetes state shared by
// the data source and the ephemeral resource. It exists as a separate package
// so the provider can register those implementations without an import cycle.
package kube

import (
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

// ProviderData is built once per Configure call and handed to every data source
// and ephemeral resource through DataSourceData / EphemeralResourceData. The
// registry intentionally outlives Configure: tunnel leases held by data sources
// live until the provider process exits.
type ProviderData struct {
	Client           kubernetes.Interface
	RestConfig       *rest.Config
	DefaultNamespace string
	Registry         *tunnel.Registry
	EphemeralLeases  *LeaseTracker
}
