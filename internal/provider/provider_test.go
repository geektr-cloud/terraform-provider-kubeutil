package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
)

func TestProviderSchemaSensitiveAttributes(t *testing.T) {
	t.Parallel()

	p := New("test")()
	var resp provider.SchemaResponse
	p.Schema(context.Background(), provider.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema diagnostics: %v", resp.Diagnostics)
	}

	for _, name := range []string{"password", "client_key", "token"} {
		attribute, ok := resp.Schema.Attributes[name]
		if !ok {
			t.Fatalf("missing attribute %q", name)
		}
		if !attribute.IsSensitive() {
			t.Fatalf("attribute %q must be sensitive", name)
		}
	}

	execBlock, ok := resp.Schema.Blocks["exec"]
	if !ok {
		t.Fatal("missing exec block")
	}
	single, ok := execBlock.(schema.SingleNestedBlock)
	if !ok {
		t.Fatalf("exec block is %T, want schema.SingleNestedBlock", execBlock)
	}
	env, ok := single.Attributes["env"]
	if !ok {
		t.Fatal("missing exec.env attribute")
	}
	if !env.IsSensitive() {
		t.Fatal("exec.env must be sensitive")
	}
	apiVersion, ok := single.Attributes["api_version"]
	if !ok {
		t.Fatal("missing exec.api_version attribute")
	}
	if !apiVersion.IsOptional() {
		t.Fatal("exec.api_version must be optional at the schema level so an absent exec block is valid")
	}
	command, ok := single.Attributes["command"]
	if !ok {
		t.Fatal("missing exec.command attribute")
	}
	if !command.IsOptional() {
		t.Fatal("exec.command must be optional at the schema level so an absent exec block is valid")
	}
}

func TestProviderSchemaHasNoLegacyCredentialAttributes(t *testing.T) {
	t.Parallel()

	p := New("test")()
	var resp provider.SchemaResponse
	p.Schema(context.Background(), provider.SchemaRequest{}, &resp)

	for _, name := range []string{"load_config_file", "auth_provider"} {
		if _, ok := resp.Schema.Attributes[name]; ok {
			t.Fatalf("attribute %q must not exist", name)
		}
	}
}

func TestProviderMetadata(t *testing.T) {
	t.Parallel()

	p := New("1.2.3")()
	var resp provider.MetadataResponse
	p.Metadata(context.Background(), provider.MetadataRequest{}, &resp)
	if resp.TypeName != "kubeutil" {
		t.Fatalf("type name = %q", resp.TypeName)
	}
	if resp.Version != "1.2.3" {
		t.Fatalf("version = %q", resp.Version)
	}
}
