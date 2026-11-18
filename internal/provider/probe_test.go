package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// probeProvider exposes one data source whose schema contains a single nested
// block with a required child attribute. It pins down framework behavior for
// absent blocks.
type probeProvider struct{}

func (probeProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "probe"
}

func (probeProvider) Schema(context.Context, provider.SchemaRequest, *provider.SchemaResponse) {}

func (probeProvider) Configure(context.Context, provider.ConfigureRequest, *provider.ConfigureResponse) {
}

func (probeProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{func() datasource.DataSource { return probeDataSource{} }}
}

func (probeProvider) Resources(context.Context) []func() resource.Resource { return nil }

type probeDataSource struct{}

func (probeDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_probe"
}

func (probeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		Blocks: map[string]dschema.Block{
			"child": dschema.SingleNestedBlock{
				Attributes: map[string]dschema.Attribute{
					"name": dschema.StringAttribute{Required: true},
				},
			},
		},
	}
}

func (probeDataSource) Read(context.Context, datasource.ReadRequest, *datasource.ReadResponse) {}

// TestFrameworkRejectsRequiredNestedAttributeInAbsentBlock documents why the
// kubeutil exec and healthcheck blocks declare their child attributes as
// Optional even though they are semantically required: with plugin-framework
// v1.19.0 a Required attribute inside an absent SingleNestedBlock fails
// validation. Configure and ParseInput enforce the requirement themselves.
func TestFrameworkRejectsRequiredNestedAttributeInAbsentBlock(t *testing.T) {
	t.Parallel()

	server := providerserver.NewProtocol6(probeProvider{})()
	ctx := context.Background()

	childType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"name": tftypes.String,
	}}
	configType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"child": childType,
	}}
	config := tftypes.NewValue(configType, map[string]tftypes.Value{
		"child": tftypes.NewValue(childType, nil),
	})

	dynamicValue, err := tfprotov6.NewDynamicValue(configType, config)
	if err != nil {
		t.Fatalf("failed to build dynamic value: %s", err)
	}
	resp, err := server.ValidateDataResourceConfig(ctx, &tfprotov6.ValidateDataResourceConfigRequest{
		TypeName: "probe_probe",
		Config:   &dynamicValue,
	})
	if err != nil {
		t.Fatalf("ValidateDataResourceConfig returned an error: %s", err)
	}

	if len(resp.Diagnostics) == 0 {
		t.Fatal("framework accepted a required nested attribute in an absent single nested block; restore Required in the real schemas")
	}
	if !strings.Contains(resp.Diagnostics[0].Summary, "Missing Configuration for Required Attribute") {
		t.Fatalf("unexpected diagnostic: %s", resp.Diagnostics[0].Summary)
	}
}
