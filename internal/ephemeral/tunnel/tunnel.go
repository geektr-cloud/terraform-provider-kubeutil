// Package tunnel implements the kubeutil_tunnel ephemeral resource.
package tunnel

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/acquisition"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/kube"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/surface"
)

const privateStateKey = "tunnel"

var (
	_ ephemeral.EphemeralResource              = &EphemeralResource{}
	_ ephemeral.EphemeralResourceWithConfigure = &EphemeralResource{}
	_ ephemeral.EphemeralResourceWithClose     = &EphemeralResource{}
)

// EphemeralResource exposes a tunnel for the duration of one Terraform
// operation. Its values never enter state or plan artifacts, and Close releases
// the reference so the registry can tear the tunnel down at zero.
type EphemeralResource struct {
	data *kube.ProviderData
}

// NewEphemeralResource returns a new kubeutil_tunnel ephemeral resource.
func NewEphemeralResource() ephemeral.EphemeralResource {
	return &EphemeralResource{}
}

// Metadata sets the ephemeral resource type name.
func (r *EphemeralResource) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tunnel"
}

// Schema returns the tunnel schema. Unlike the data source it has no id.
func (r *EphemeralResource) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = surface.EphemeralSchema()
}

// Configure stores the provider data for later opens.
func (r *EphemeralResource) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	data, ok := req.ProviderData.(*kube.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			"The kubeutil_tunnel ephemeral resource received provider data it does not understand. This is a provider bug.")
		return
	}
	r.data = data
}

// Open acquires a tunnel and records the reference token in private state so
// Close can release it.
func (r *EphemeralResource) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	if r.data == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The kubeutil provider was not configured before opening kubeutil_tunnel.")
		return
	}

	var model surface.Model
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	input, diags := surface.ParseInput(ctx, model, r.data.DefaultNamespace)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	acqCtx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()

	lease, err := acquisition.Acquire(acqCtx, r.data, input)
	if err != nil {
		resp.Diagnostics.AddError("Failed to open tunnel", err.Error())
		return
	}

	// The registry key (not the lease pointer) travels in private state; the
	// provider-scoped tracker maps it back to the lease for Close.
	token, key := r.data.EphemeralLeases.Track(lease)
	raw, err := kube.EphemeralPrivateState{Token: token, Key: key}.Encode()
	if err != nil {
		r.data.EphemeralLeases.Release(token)
		resp.Diagnostics.AddError("Failed to record tunnel private state", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, privateStateKey, raw)...)
	if resp.Diagnostics.HasError() {
		r.data.EphemeralLeases.Release(token)
		return
	}

	surface.ApplyResult(&model, input, lease.Tunnel().LocalPort())
	if diags := resp.Result.Set(ctx, &model); diags.HasError() {
		// A failed open must not leave a tracked lease behind.
		r.data.EphemeralLeases.Release(token)
		resp.Diagnostics.Append(diags...)
	}
}

// Close releases the tunnel reference recorded by Open. It is idempotent.
func (r *EphemeralResource) Close(ctx context.Context, req ephemeral.CloseRequest, resp *ephemeral.CloseResponse) {
	if r.data == nil {
		// Configure did not run (for example after a provider restart), so this
		// process has no tunnel to release.
		return
	}

	raw, diags := req.Private.GetKey(ctx, privateStateKey)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state, err := kube.DecodeEphemeralPrivateState(raw)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read tunnel private state", err.Error())
		return
	}
	if state.Token == "" {
		return
	}

	if !r.data.EphemeralLeases.Release(state.Token) {
		tflog.Debug(ctx, "kubernetes portforward: ephemeral close found no tracked lease", map[string]any{
			"key": state.Key,
		})
	}
}
