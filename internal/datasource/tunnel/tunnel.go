// Package tunnel implements the kubeutil_tunnel data source.
package tunnel

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/acquisition"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/kube"
	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/surface"
)

var (
	_ datasource.DataSource              = &DataSource{}
	_ datasource.DataSourceWithConfigure = &DataSource{}
)

// DataSource exposes a process-scoped tunnel whose result is persisted in
// plan and state. Its local port and endpoint are only valid for the command and
// provider process that read them.
type DataSource struct {
	data *kube.ProviderData
}

// NewDataSource returns a new kubeutil_tunnel data source.
func NewDataSource() datasource.DataSource {
	return &DataSource{}
}

// Metadata sets the data source type name.
func (d *DataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_tunnel"
}

// Schema returns the tunnel schema, including the computed id.
func (d *DataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = surface.DataSourceSchema()
}

// Configure stores the provider data for later reads.
func (d *DataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}

	data, ok := req.ProviderData.(*kube.ProviderData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			"The kubeutil_tunnel data source received provider data it does not understand. This is a provider bug.")
		return
	}
	d.data = data
}

// Read acquires a tunnel and writes its endpoint into state. The lease is kept
// for the lifetime of the provider process.
func (d *DataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	if d.data == nil {
		resp.Diagnostics.AddError("Provider not configured",
			"The kubeutil provider was not configured before reading kubeutil_tunnel.")
		return
	}

	var model surface.DataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &model)...)
	if resp.Diagnostics.HasError() {
		return
	}

	input, diags := surface.ParseInput(ctx, model.Model, d.data.DefaultNamespace)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	acqCtx, cancel := context.WithTimeout(ctx, input.Timeout)
	defer cancel()

	// Data source references are intentionally never released: the tunnel must
	// stay alive for the lifetime of the provider process.
	lease, err := acquisition.Acquire(acqCtx, d.data, input)
	if err != nil {
		resp.Diagnostics.AddError("Failed to open tunnel", err.Error())
		return
	}

	surface.ApplyResult(&model.Model, input, lease.Tunnel().LocalPort())
	model.ID = types.StringValue(input.ID())

	if diags := resp.State.Set(ctx, &model); diags.HasError() {
		// A failed read must not leave a data source reference behind.
		lease.Release()
		resp.Diagnostics.Append(diags...)
	}
}
