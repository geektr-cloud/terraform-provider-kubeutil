package surface

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/tunnel"
)

func baseModel() Model {
	return Model{
		Target:      types.StringValue("svc/db"),
		Port:        types.StringValue("5432"),
		Namespace:   types.StringNull(),
		Timeout:     types.StringNull(),
		Healthcheck: types.ObjectNull(map[string]attr.Type{"type": types.StringType}),
		Host:        types.StringNull(),
		LocalPort:   types.Int64Null(),
		Hostport:    types.StringNull(),
	}
}

func healthcheck(t *testing.T, kind string) types.Object {
	t.Helper()
	value, diags := types.ObjectValue(
		map[string]attr.Type{"type": types.StringType},
		map[string]attr.Value{"type": types.StringValue(kind)},
	)
	if diags.HasError() {
		t.Fatalf("failed to build healthcheck object: %v", diags)
	}
	return value
}

func TestParseInputNamespacePrecedence(t *testing.T) {
	t.Parallel()

	model := baseModel()
	input, diags := ParseInput(context.Background(), model, "provider-ns")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if input.Namespace != "provider-ns" {
		t.Fatalf("namespace = %q, want provider-ns", input.Namespace)
	}
	if input.ID() != "provider-ns/service/db/5432" {
		t.Fatalf("id = %q", input.ID())
	}

	model.Namespace = types.StringValue("explicit-ns")
	input, diags = ParseInput(context.Background(), model, "provider-ns")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if input.Namespace != "explicit-ns" {
		t.Fatalf("namespace = %q, want explicit-ns", input.Namespace)
	}

	model.Namespace = types.StringUnknown()
	input, diags = ParseInput(context.Background(), model, "provider-ns")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if input.Namespace != "provider-ns" {
		t.Fatalf("namespace = %q, want provider-ns for an unknown value", input.Namespace)
	}

	model.Namespace = types.StringNull()
	input, diags = ParseInput(context.Background(), model, "")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if input.Namespace != "default" {
		t.Fatalf("namespace = %q, want default", input.Namespace)
	}
}

func TestParseInputEmptyNamespaceIsInvalid(t *testing.T) {
	t.Parallel()

	model := baseModel()
	model.Namespace = types.StringValue("")
	_, diags := ParseInput(context.Background(), model, "provider-ns")
	if !diags.HasError() {
		t.Fatal("expected an error for an explicit empty namespace")
	}
	if got := diags[0].Summary(); !strings.Contains(got, "Invalid namespace") {
		t.Fatalf("diagnostic summary = %q", got)
	}
}

func TestParseInputTimeout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		value     types.String
		want      time.Duration
		wantError bool
	}{
		{"default", types.StringNull(), DefaultTimeout, false},
		{"explicit", types.StringValue("5s"), 5 * time.Second, false},
		{"unknown", types.StringUnknown(), DefaultTimeout, false},
		{"zero", types.StringValue("0s"), 0, true},
		{"negative", types.StringValue("-1s"), 0, true},
		{"garbage", types.StringValue("soon"), 0, true},
		{"empty", types.StringValue(""), 0, true},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			model := baseModel()
			model.Timeout = test.value
			input, diags := ParseInput(context.Background(), model, "ns")
			if test.wantError {
				if !diags.HasError() {
					t.Fatalf("expected an error, got %v", diags)
				}
				return
			}
			if diags.HasError() {
				t.Fatalf("unexpected diagnostics: %v", diags)
			}
			if input.Timeout != test.want {
				t.Fatalf("timeout = %s, want %s", input.Timeout, test.want)
			}
		})
	}
}

func TestParseInputHealthcheck(t *testing.T) {
	t.Parallel()

	model := baseModel()
	model.Healthcheck = healthcheck(t, "telnet")
	input, diags := ParseInput(context.Background(), model, "ns")
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if input.Healthcheck != HealthcheckTelnet {
		t.Fatalf("healthcheck = %q", input.Healthcheck)
	}

	model.Healthcheck = healthcheck(t, "http")
	_, diags = ParseInput(context.Background(), model, "ns")
	if !diags.HasError() {
		t.Fatal("expected an error for an unsupported healthcheck type")
	}
}

func TestParseInputInvalidTargetAndPort(t *testing.T) {
	t.Parallel()

	model := baseModel()
	model.Target = types.StringValue("unknown/x")
	model.Port = types.StringValue("8888:5432")
	_, diags := ParseInput(context.Background(), model, "ns")
	if !diags.HasError() {
		t.Fatal("expected errors")
	}
	if len(diags) != 2 {
		t.Fatalf("expected two diagnostics, got %v", diags)
	}
}

func TestApplyResult(t *testing.T) {
	t.Parallel()

	model := baseModel()
	target, err := tunnel.ParseTarget("svc/db")
	if err != nil {
		t.Fatalf("failed to parse target: %s", err)
	}
	port, err := tunnel.ParsePort("postgres")
	if err != nil {
		t.Fatalf("failed to parse port: %s", err)
	}
	input := Input{
		Target:    target,
		Port:      port,
		Namespace: "team",
		Timeout:   DefaultTimeout,
	}

	ApplyResult(&model, input, 41234)

	if model.Namespace.ValueString() != "team" {
		t.Fatalf("namespace = %q", model.Namespace.ValueString())
	}
	if model.Host.ValueString() != "127.0.0.1" {
		t.Fatalf("host = %q", model.Host.ValueString())
	}
	if model.LocalPort.ValueInt64() != 41234 {
		t.Fatalf("local_port = %d", model.LocalPort.ValueInt64())
	}
	if model.Hostport.ValueString() != "127.0.0.1:41234" {
		t.Fatalf("hostport = %q", model.Hostport.ValueString())
	}
	if model.Target.ValueString() != "svc/db" || model.Port.ValueString() != "5432" {
		t.Fatalf("inputs were modified: target=%q port=%q", model.Target.ValueString(), model.Port.ValueString())
	}
}
