// Package provider implements the kubeutil Terraform provider.
package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"k8s.io/client-go/kubernetes"

	datasourcetunnel "github.com/geektr-cloud/terraform-provider-kubeutil/internal/datasource/tunnel"
	ephemeraltunnel "github.com/geektr-cloud/terraform-provider-kubeutil/internal/ephemeral/tunnel"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/kube"
	tunnellib "github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

// New returns a provider factory bound to version. Each factory call creates a
// provider instance with its own tunnel registry; data source leases keep their
// tunnels until the provider process exits.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &KubeutilProvider{
			version:  version,
			registry: tunnellib.NewRegistry(),
			leases:   kube.NewLeaseTracker(),
		}
	}
}

var (
	_ provider.Provider                       = &KubeutilProvider{}
	_ provider.ProviderWithEphemeralResources = &KubeutilProvider{}
)

// KubeutilProvider is the provider implementation.
type KubeutilProvider struct {
	version  string
	registry *tunnellib.Registry
	leases   *kube.LeaseTracker
}

// Metadata returns the provider type name and version.
func (p *KubeutilProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "kubeutil"
	resp.Version = p.version
}

// Schema returns the credential schema shared by all tunnel surfaces.
func (p *KubeutilProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Exposes process-scoped Kubernetes port-forward tunnels to Terraform and OpenTofu configurations.",
		Attributes: map[string]schema.Attribute{
			"host": schema.StringAttribute{
				Optional:    true,
				Description: "The hostname (in the form of a URI) of the Kubernetes API server. Can also be set with KUBE_HOST.",
			},
			"username": schema.StringAttribute{
				Optional:    true,
				Description: "Username for HTTP basic authentication against the Kubernetes API server. Can also be set with KUBE_USER.",
			},
			"password": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Password for HTTP basic authentication against the Kubernetes API server. Can also be set with KUBE_PASSWORD.",
			},
			"insecure": schema.BoolAttribute{
				Optional:    true,
				Description: "Whether the server should be accessed without verifying the TLS certificate. Can also be set with KUBE_INSECURE.",
			},
			"tls_server_name": schema.StringAttribute{
				Optional:    true,
				Description: "Server name passed to the server for SNI and used to check the server certificate. Can also be set with KUBE_TLS_SERVER_NAME.",
			},
			"client_certificate": schema.StringAttribute{
				Optional:    true,
				Description: "PEM-encoded client certificate for TLS authentication. Can also be set with KUBE_CLIENT_CERT_DATA.",
			},
			"client_key": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "PEM-encoded client certificate key for TLS authentication. Can also be set with KUBE_CLIENT_KEY_DATA.",
			},
			"cluster_ca_certificate": schema.StringAttribute{
				Optional:    true,
				Description: "PEM-encoded root certificate bundle for TLS authentication. Can also be set with KUBE_CLUSTER_CA_CERT_DATA.",
			},
			"config_path": schema.StringAttribute{
				Optional:    true,
				Description: "Path to a kubeconfig file. Conflicts with config_paths. Can also be set with KUBE_CONFIG_PATH.",
			},
			"config_paths": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "List of kubeconfig files used in order of precedence. Conflicts with config_path. Can also be set with KUBE_CONFIG_PATHS, split with the OS path list separator.",
			},
			"config_context": schema.StringAttribute{
				Optional:    true,
				Description: "Name of the kubeconfig context to use. Can also be set with KUBE_CTX.",
			},
			"config_context_auth_info": schema.StringAttribute{
				Optional:    true,
				Description: "Name of the kubeconfig authentication information to use. Can also be set with KUBE_CTX_AUTH_INFO.",
			},
			"config_context_cluster": schema.StringAttribute{
				Optional:    true,
				Description: "Name of the kubeconfig cluster to use. Can also be set with KUBE_CTX_CLUSTER.",
			},
			"token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Bearer token used to authenticate. Can also be set with KUBE_TOKEN.",
			},
			"proxy_url": schema.StringAttribute{
				Optional:    true,
				Description: "URL of a proxy used for all API requests. Can also be set with KUBE_PROXY_URL.",
			},
			"default_namespace": schema.StringAttribute{
				Optional:    true,
				Description: "Namespace used by tunnel instances that do not set their own namespace. Defaults to the credential context namespace, then to the literal namespace \"default\". Can also be set with KUBE_NAMESPACE.",
			},
		},
		Blocks: map[string]schema.Block{
			// api_version and command are declared optional because the framework
			// validates nested required attributes even when the block is absent
			// (see TestFrameworkRejectsRequiredNestedAttributeInAbsentBlock).
			// Configure enforces them whenever exec is configured; their
			// descriptions state that they are required in that case.
			"exec": schema.SingleNestedBlock{
				Description: "Configure an exec credential plugin. At most one block is allowed.",
				Attributes: map[string]schema.Attribute{
					"api_version": schema.StringAttribute{
						Optional:    true,
						Description: "Client authentication API version of the exec plugin, for example client.authentication.k8s.io/v1beta1. Required when the exec block is configured.",
					},
					"command": schema.StringAttribute{
						Optional:    true,
						Description: "Command to execute to retrieve credentials. Required when the exec block is configured.",
					},
					"args": schema.ListAttribute{
						Optional:    true,
						ElementType: types.StringType,
						Description: "Arguments passed to the exec command.",
					},
					"env": schema.MapAttribute{
						Optional:    true,
						Sensitive:   true,
						ElementType: types.StringType,
						Description: "Environment variables passed to the exec command.",
					},
				},
			},
		},
	}
}

// Configure builds an in-memory rest.Config and Kubernetes client. It performs
// no network I/O and shares the provider-instance registry with every tunnel
// surface.
func (p *KubeutilProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var model providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	result, diags := buildConfig(ctx, model)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	if req.TerraformVersion != "" {
		result.Config.UserAgent = fmt.Sprintf("terraform-provider-kubeutil/%s terraform/%s", p.version, req.TerraformVersion)
	}

	client, err := kubernetes.NewForConfig(result.Config)
	if err != nil {
		resp.Diagnostics.AddError("Failed to configure the Kubernetes client",
			fmt.Sprintf("The provider configuration could not produce a usable Kubernetes client: %s", err))
		return
	}

	data := &kube.ProviderData{
		Client:           client,
		RestConfig:       result.Config,
		DefaultNamespace: result.DefaultNamespace,
		Registry:         p.registry,
		EphemeralLeases:  p.leases,
	}

	resp.DataSourceData = data
	resp.EphemeralResourceData = data
}

// DataSources returns the kubeutil_tunnel data source.
func (p *KubeutilProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		datasourcetunnel.NewDataSource,
	}
}

// Resources returns no managed resources.
func (p *KubeutilProvider) Resources(_ context.Context) []func() resource.Resource {
	return nil
}

// EphemeralResources returns the kubeutil_tunnel ephemeral resource.
func (p *KubeutilProvider) EphemeralResources(_ context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{
		ephemeraltunnel.NewEphemeralResource,
	}
}
