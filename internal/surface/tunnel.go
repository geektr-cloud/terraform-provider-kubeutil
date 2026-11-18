// Package surface defines the Terraform-facing schema and input parsing shared
// by the kubeutil_tunnel data source and ephemeral resource.
package surface

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	ephemeralschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

// DefaultTimeout is the effective acquisition timeout when timeout is omitted.
const DefaultTimeout = 60 * time.Second

// HealthcheckTelnet is the only supported healthcheck type.
const HealthcheckTelnet = "telnet"

const (
	tunnelDescription = "A process-scoped port-forward tunnel from 127.0.0.1 to an abstract Kubernetes target port."

	targetDescription = "Target of the tunnel: a bare pod name or TYPE/NAME. TYPE accepts pod, service, deployment, replicaset, statefulset, daemonset, job, and replicationcontroller, plus common kubectl aliases such as po, svc, deploy, rs, sts, ds, and rc. Custom resources and group/version-qualified names are not supported."
	portDescription   = "Remote port of the target: a number (5432), a port name (postgres), or a number with an empty local component (:5432). A non-empty local port is rejected; the provider always allocates a random local port on 127.0.0.1."
	namespaceDescr    = "Namespace of the target. Defaults to the provider's default_namespace, which in turn defaults to the credential context namespace and then to the literal namespace \"default\"."
	timeoutDescr      = "Maximum time for the whole acquisition (target read, pod selection, port resolution, initial upgrade and listener, and the optional healthcheck), as a Go duration such as 60s. Defaults to 60s. This is not a request timeout and does not limit the lifetime of the tunnel."
	healthcheckDescr  = "Optional healthcheck run on every acquisition, including a shared tunnel. telnet opens the local listener without sending application data and observes the forwarding path briefly; it is negative rejection detection, not proof of remote application health."
	healthcheckType   = "Healthcheck type. Only \"telnet\" is supported. Required when the healthcheck block is configured."
	hostDescr         = "Loopback address the tunnel listens on. Always 127.0.0.1."
	localPortDescr    = "OS-assigned local port of the tunnel listener."
	hostportDescr     = "Consumer-facing endpoint of the tunnel, 127.0.0.1:<local_port>."
	idDescr           = "Stable target-scoped identifier, <namespace>/<canonical-target>/<normalized-requested-port>."
)

// HealthcheckModel is the nested healthcheck block.
type HealthcheckModel struct {
	Type types.String `tfsdk:"type"`
}

// Model is the shared user schema plus computed tunnel outputs. The data source
// additionally exposes id; the ephemeral resource does not.
type Model struct {
	Target    types.String `tfsdk:"target"`
	Port      types.String `tfsdk:"port"`
	Namespace types.String `tfsdk:"namespace"`
	Timeout   types.String `tfsdk:"timeout"`

	Healthcheck types.Object `tfsdk:"healthcheck"`

	Host      types.String `tfsdk:"host"`
	LocalPort types.Int64  `tfsdk:"local_port"`
	Hostport  types.String `tfsdk:"hostport"`
}

// DataModel is Model plus the data source's computed stable id. The ephemeral
// resource cannot use this shape because it deliberately has no id.
type DataModel struct {
	Model

	ID types.String `tfsdk:"id"`
}

// DataSourceSchema returns the kubeutil_tunnel data source schema, including
// the computed id.
func DataSourceSchema() schema.Schema {
	return schema.Schema{
		Description: tunnelDescription,
		Attributes: map[string]schema.Attribute{
			"target": schema.StringAttribute{Required: true, Description: targetDescription},
			"port":   schema.StringAttribute{Required: true, Description: portDescription},
			"namespace": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: namespaceDescr,
			},
			"timeout":    schema.StringAttribute{Optional: true, Description: timeoutDescr},
			"host":       schema.StringAttribute{Computed: true, Description: hostDescr},
			"local_port": schema.Int64Attribute{Computed: true, Description: localPortDescr},
			"hostport":   schema.StringAttribute{Computed: true, Description: hostportDescr},
			"id":         schema.StringAttribute{Computed: true, Description: idDescr},
		},
		Blocks: map[string]schema.Block{
			// type is optional at the schema level because the framework validates
			// nested required attributes even when the block is absent (see
			// TestFrameworkRejectsRequiredNestedAttributeInAbsentBlock); ParseInput
			// requires it whenever healthcheck is configured, and its description
			// states that.
			"healthcheck": schema.SingleNestedBlock{
				Description: healthcheckDescr,
				Attributes: map[string]schema.Attribute{
					"type": schema.StringAttribute{Optional: true, Description: healthcheckType},
				},
			},
		},
	}
}

// EphemeralSchema returns the kubeutil_tunnel ephemeral resource schema. It is
// identical to the data source schema except that it has no id, because nothing
// about an ephemeral instance persists.
func EphemeralSchema() ephemeralschema.Schema {
	return ephemeralschema.Schema{
		Description: tunnelDescription,
		Attributes: map[string]ephemeralschema.Attribute{
			"target": ephemeralschema.StringAttribute{Required: true, Description: targetDescription},
			"port":   ephemeralschema.StringAttribute{Required: true, Description: portDescription},
			"namespace": ephemeralschema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: namespaceDescr,
			},
			"timeout":    ephemeralschema.StringAttribute{Optional: true, Description: timeoutDescr},
			"host":       ephemeralschema.StringAttribute{Computed: true, Description: hostDescr},
			"local_port": ephemeralschema.Int64Attribute{Computed: true, Description: localPortDescr},
			"hostport":   ephemeralschema.StringAttribute{Computed: true, Description: hostportDescr},
		},
		Blocks: map[string]ephemeralschema.Block{
			// See the data source schema note about the nested required attribute.
			"healthcheck": ephemeralschema.SingleNestedBlock{
				Description: healthcheckDescr,
				Attributes: map[string]ephemeralschema.Attribute{
					"type": ephemeralschema.StringAttribute{Optional: true, Description: healthcheckType},
				},
			},
		},
	}
}

// Input is the parsed, validated acquisition request.
type Input struct {
	Target      tunnel.Target
	Port        tunnel.ParsedPort
	Namespace   string
	Timeout     time.Duration
	Healthcheck string
}

// ID returns the stable target-scoped identity.
func (i Input) ID() string {
	return tunnel.NewKey(i.Namespace, i.Target, i.Port).String()
}

// ParseInput validates the user schema and resolves namespace precedence:
// explicit value, then the provider default namespace, then "default".
func ParseInput(ctx context.Context, model Model, defaultNamespace string) (Input, diag.Diagnostics) {
	var diags diag.Diagnostics

	target, err := tunnel.ParseTarget(model.Target.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("target"), "Invalid tunnel target", err.Error())
	}

	port, err := tunnel.ParsePort(model.Port.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("port"), "Invalid tunnel port", err.Error())
	}

	namespace := defaultNamespace
	switch {
	case model.Namespace.IsUnknown():
		// Unknown values are skipped during validation; the provider default is
		// the only value that can be used here.
	case !model.Namespace.IsNull():
		if model.Namespace.ValueString() == "" {
			diags.AddAttributeError(path.Root("namespace"), "Invalid namespace", "namespace must not be empty when set; remove the attribute to use the provider default namespace")
		} else {
			namespace = model.Namespace.ValueString()
		}
	}
	if namespace == "" {
		namespace = "default"
	}

	timeout := DefaultTimeout
	if !model.Timeout.IsNull() && !model.Timeout.IsUnknown() {
		parsed, err := time.ParseDuration(model.Timeout.ValueString())
		switch {
		case err != nil:
			diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", fmt.Sprintf("timeout must be a Go duration such as 60s: %s", err))
		case parsed <= 0:
			diags.AddAttributeError(path.Root("timeout"), "Invalid timeout", "timeout must be greater than zero")
		default:
			timeout = parsed
		}
	}

	healthcheck := ""
	if !model.Healthcheck.IsNull() && !model.Healthcheck.IsUnknown() {
		var hc HealthcheckModel
		diags.Append(model.Healthcheck.As(ctx, &hc, basetypes.ObjectAsOptions{})...)
		if !diags.HasError() {
			kind := hc.Type.ValueString()
			switch kind {
			case "":
				diags.AddAttributeError(path.Root("healthcheck").AtName("type"), "Missing healthcheck type", "healthcheck.type must be set when the healthcheck block is configured")
			case HealthcheckTelnet:
				healthcheck = kind
			default:
				diags.AddAttributeError(path.Root("healthcheck").AtName("type"), "Unsupported healthcheck type", fmt.Sprintf("healthcheck type %q is not supported; the only supported type is %q", kind, HealthcheckTelnet))
			}
		}
	}

	return Input{
		Target:      target,
		Port:        port,
		Namespace:   namespace,
		Timeout:     timeout,
		Healthcheck: healthcheck,
	}, diags
}

// ApplyResult writes the computed tunnel outputs into the result model. The
// input attributes are preserved as configured so they do not produce diffs.
func ApplyResult(model *Model, input Input, localPort uint16) {
	model.Namespace = types.StringValue(input.Namespace)
	model.Host = types.StringValue(tunnel.BindAddress)
	model.LocalPort = types.Int64Value(int64(localPort))
	model.Hostport = types.StringValue(net.JoinHostPort(tunnel.BindAddress, strconv.Itoa(int(localPort))))
}
