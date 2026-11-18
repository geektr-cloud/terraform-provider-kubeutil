package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/types"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var execObjectType = map[string]attr.Type{
	"api_version": types.StringType,
	"command":     types.StringType,
	"args":        types.ListType{ElemType: types.StringType},
	"env":         types.MapType{ElemType: types.StringType},
}

func emptyProviderModel() providerModel {
	return providerModel{
		Host:                  types.StringNull(),
		Username:              types.StringNull(),
		Password:              types.StringNull(),
		Insecure:              types.BoolNull(),
		TLSServerName:         types.StringNull(),
		ClientCertificate:     types.StringNull(),
		ClientKey:             types.StringNull(),
		ClusterCACertificate:  types.StringNull(),
		ConfigPath:            types.StringNull(),
		ConfigPaths:           types.ListNull(types.StringType),
		ConfigContext:         types.StringNull(),
		ConfigContextAuthInfo: types.StringNull(),
		ConfigContextCluster:  types.StringNull(),
		Token:                 types.StringNull(),
		ProxyURL:              types.StringNull(),
		DefaultNamespace:      types.StringNull(),
		Exec:                  types.ObjectNull(execObjectType),
	}
}

func clearKubeEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"KUBECONFIG",
		"KUBE_HOST",
		"KUBE_USER",
		"KUBE_PASSWORD",
		"KUBE_INSECURE",
		"KUBE_TLS_SERVER_NAME",
		"KUBE_CLIENT_CERT_DATA",
		"KUBE_CLIENT_KEY_DATA",
		"KUBE_CLUSTER_CA_CERT_DATA",
		"KUBE_CONFIG_PATH",
		"KUBE_CONFIG_PATHS",
		"KUBE_CTX",
		"KUBE_CTX_AUTH_INFO",
		"KUBE_CTX_CLUSTER",
		"KUBE_TOKEN",
		"KUBE_PROXY_URL",
		"KUBE_NAMESPACE",
	} {
		t.Setenv(name, "")
	}
}

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("failed to write kubeconfig: %s", err)
	}
	return path
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: file-cluster
  cluster:
    server: https://from-file:6443
    insecure-skip-tls-verify: true
- name: other-cluster
  cluster:
    server: https://other-cluster:6443
    insecure-skip-tls-verify: true
contexts:
- name: file-context
  context:
    cluster: file-cluster
    user: file-user
    namespace: file-namespace
- name: other-context
  context:
    cluster: other-cluster
    user: other-user
    namespace: other-namespace
current-context: file-context
users:
- name: file-user
  user:
    token: file-token
- name: other-user
  user:
    token: other-token
`

func TestBuildConfigStaticOverrides(t *testing.T) {
	clearKubeEnv(t)

	path := writeKubeconfig(t, testKubeconfig)
	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(path)
	model.Host = types.StringValue("https://override:6443")
	model.Token = types.StringValue("static-token")
	model.ClusterCACertificate = types.StringValue("ca-data")

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Host != "https://override:6443" {
		t.Fatalf("host = %q", result.Config.Host)
	}
	if result.Config.BearerToken != "static-token" {
		t.Fatalf("bearer token was not overridden")
	}
	if string(result.Config.CAData) != "ca-data" {
		t.Fatalf("CA data was not overridden")
	}
	if result.Config.Username != "" || result.Config.Password != "" {
		t.Fatalf("basic auth was unexpectedly set")
	}
	if result.DefaultNamespace != "file-namespace" {
		t.Fatalf("default namespace = %q, want file-namespace", result.DefaultNamespace)
	}
}

func TestBuildConfigBasicAuth(t *testing.T) {
	clearKubeEnv(t)

	path := writeKubeconfig(t, `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster:
    server: https://auth:6443
contexts:
- name: ctx
  context:
    cluster: c
    user: u
current-context: ctx
users:
- name: u
  user: {}
`)
	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(path)
	model.Username = types.StringValue("static-user")
	model.Password = types.StringValue("static-password")

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Username != "static-user" || result.Config.Password != "static-password" {
		t.Fatalf("basic auth was not overridden: user=%q", result.Config.Username)
	}
}

func TestBuildConfigHostDefaultTLS(t *testing.T) {
	clearKubeEnv(t)

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(writeKubeconfig(t, testKubeconfig))
	model.Host = types.StringValue("api.example.com:6443")

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Host != "http://api.example.com:6443" {
		t.Fatalf("host without TLS data = %q, want http", result.Config.Host)
	}

	model.ClusterCACertificate = types.StringValue("ca-data")
	result, diags = buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Host != "https://api.example.com:6443" {
		t.Fatalf("host with certificate data = %q, want https", result.Config.Host)
	}

	model.Insecure = types.BoolValue(true)
	result, diags = buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Host != "http://api.example.com:6443" {
		t.Fatalf("host with insecure = %q, want http", result.Config.Host)
	}
}

func TestBuildConfigContextOverrides(t *testing.T) {
	clearKubeEnv(t)

	path := writeKubeconfig(t, testKubeconfig)
	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(path)
	model.ConfigContext = types.StringValue("other-context")

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Host != "https://other-cluster:6443" {
		t.Fatalf("host = %q, expected the overridden context's cluster", result.Config.Host)
	}
	if result.Config.BearerToken != "other-token" {
		t.Fatalf("token = %q, expected the overridden context's user", result.Config.BearerToken)
	}
	if result.DefaultNamespace != "other-namespace" {
		t.Fatalf("namespace = %q, expected the overridden context's namespace", result.DefaultNamespace)
	}
}

func TestBuildConfigConfigPathConflict(t *testing.T) {
	clearKubeEnv(t)

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue("/tmp/one")
	paths, diags := types.ListValueFrom(context.Background(), types.StringType, []string{"/tmp/two"})
	if diags.HasError() {
		t.Fatalf("failed to build list: %v", diags)
	}
	model.ConfigPaths = paths

	_, diags = buildConfig(context.Background(), model)
	if !diags.HasError() {
		t.Fatal("expected config_path/config_paths conflict")
	}
	if !strings.Contains(diags.Errors()[0].Summary(), "Conflicting provider configuration") {
		t.Fatalf("unexpected diagnostic: %v", diags.Errors()[0].Summary())
	}
}

func TestBuildConfigEnvConfigPaths(t *testing.T) {
	clearKubeEnv(t)

	first := writeKubeconfig(t, testKubeconfig)
	second := writeKubeconfig(t, strings.ReplaceAll(testKubeconfig, "file-namespace", "second-namespace"))
	t.Setenv("KUBE_CONFIG_PATHS", strings.Join([]string{first, second}, string(os.PathListSeparator)))

	model := emptyProviderModel()
	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.DefaultNamespace != "file-namespace" {
		t.Fatalf("namespace = %q, expected the first kubeconfig to win", result.DefaultNamespace)
	}
	if result.Config.Host != "https://from-file:6443" {
		t.Fatalf("host = %q", result.Config.Host)
	}
}

func TestBuildConfigExec(t *testing.T) {
	clearKubeEnv(t)

	args, diags := types.ListValueFrom(context.Background(), types.StringType, []string{"get-token", "--cluster", "x"})
	if diags.HasError() {
		t.Fatalf("failed to build args: %v", diags)
	}
	env, diags := types.MapValueFrom(context.Background(), types.StringType, map[string]string{"B": "2", "A": "1"})
	if diags.HasError() {
		t.Fatalf("failed to build env: %v", diags)
	}
	execObject, diags := types.ObjectValue(execObjectType, map[string]attr.Value{
		"api_version": types.StringValue("client.authentication.k8s.io/v1alpha1"),
		"command":     types.StringValue("aws"),
		"args":        args,
		"env":         env,
	})
	if diags.HasError() {
		t.Fatalf("failed to build exec object: %v", diags)
	}

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(writeKubeconfig(t, testKubeconfig))
	model.Exec = execObject

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if len(diags) == 0 {
		t.Fatal("expected a deprecation warning for v1alpha1")
	}
	if result.Config.ExecProvider == nil {
		t.Fatal("exec provider was not set")
	}
	if result.Config.ExecProvider.Command != "aws" {
		t.Fatalf("command = %q", result.Config.ExecProvider.Command)
	}
	if result.Config.ExecProvider.InteractiveMode != clientcmdapi.IfAvailableExecInteractiveMode {
		t.Fatalf("interactive mode = %q", result.Config.ExecProvider.InteractiveMode)
	}
	if len(result.Config.ExecProvider.Args) != 3 || result.Config.ExecProvider.Args[0] != "get-token" {
		t.Fatalf("args = %#v", result.Config.ExecProvider.Args)
	}
	if len(result.Config.ExecProvider.Env) != 2 || result.Config.ExecProvider.Env[0].Name != "A" {
		t.Fatalf("env = %#v", result.Config.ExecProvider.Env)
	}
}

func TestBuildExecArgsPreserveEmptyStrings(t *testing.T) {
	clearKubeEnv(t)

	args, diags := types.ListValueFrom(context.Background(), types.StringType, []string{"", "--cluster", "", "x"})
	if diags.HasError() {
		t.Fatalf("failed to build args: %v", diags)
	}
	execObject, diags := types.ObjectValue(execObjectType, map[string]attr.Value{
		"api_version": types.StringValue("client.authentication.k8s.io/v1beta1"),
		"command":     types.StringValue("plugin"),
		"args":        args,
		"env":         types.MapNull(types.StringType),
	})
	if diags.HasError() {
		t.Fatalf("failed to build exec object: %v", diags)
	}

	execConfig, diags := buildExec(context.Background(), execObject)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	want := []string{"", "--cluster", "", "x"}
	if len(execConfig.Args) != len(want) {
		t.Fatalf("args = %#v, want %#v", execConfig.Args, want)
	}
	for index := range want {
		if execConfig.Args[index] != want[index] {
			t.Fatalf("args = %#v, want %#v", execConfig.Args, want)
		}
	}
}

func TestBuildExecRejectsNullAndUnknownEnvValues(t *testing.T) {
	clearKubeEnv(t)

	env := types.MapValueMust(types.StringType, map[string]attr.Value{
		"C_SET":     types.StringValue("secret-value"),
		"A_NULL":    types.StringNull(),
		"B_UNKNOWN": types.StringUnknown(),
	})
	execObject, diags := types.ObjectValue(execObjectType, map[string]attr.Value{
		"api_version": types.StringValue("client.authentication.k8s.io/v1beta1"),
		"command":     types.StringValue("plugin"),
		"args":        types.ListNull(types.StringType),
		"env":         env,
	})
	if diags.HasError() {
		t.Fatalf("failed to build exec object: %v", diags)
	}

	execConfig, diags := buildExec(context.Background(), execObject)
	if !diags.HasError() {
		t.Fatal("expected diagnostics for null and unknown env values")
	}
	if len(diags) != 2 {
		t.Fatalf("expected exactly two diagnostics, got %v", diags)
	}
	if !strings.Contains(diags[0].Detail(), `exec.env["A_NULL"]`) {
		t.Fatalf("first diagnostic = %q, expected deterministic A_NULL ordering", diags[0].Detail())
	}
	if !strings.Contains(diags[1].Detail(), `exec.env["B_UNKNOWN"]`) {
		t.Fatalf("second diagnostic = %q, expected B_UNKNOWN", diags[1].Detail())
	}
	for _, diagnostic := range diags {
		if strings.Contains(diagnostic.Detail(), "secret-value") {
			t.Fatalf("diagnostic leaked an env value: %q", diagnostic.Detail())
		}
	}
	if len(execConfig.Env) != 1 || execConfig.Env[0].Name != "C_SET" || execConfig.Env[0].Value != "secret-value" {
		t.Fatalf("valid env entries were not preserved: %#v", execConfig.Env)
	}
}

func TestBuildConfigNamespacePrecedence(t *testing.T) {
	clearKubeEnv(t)
	path := writeKubeconfig(t, testKubeconfig)

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(path)
	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.DefaultNamespace != "file-namespace" {
		t.Fatalf("namespace = %q, want the context namespace", result.DefaultNamespace)
	}

	model.DefaultNamespace = types.StringValue("explicit")
	result, _ = buildConfig(context.Background(), model)
	if result.DefaultNamespace != "explicit" {
		t.Fatalf("namespace = %q, want explicit", result.DefaultNamespace)
	}

	model.DefaultNamespace = types.StringNull()
	t.Setenv("KUBE_NAMESPACE", "from-env")
	result, _ = buildConfig(context.Background(), model)
	if result.DefaultNamespace != "from-env" {
		t.Fatalf("namespace = %q, want from-env", result.DefaultNamespace)
	}

	model.DefaultNamespace = types.StringValue("")
	_, diags = buildConfig(context.Background(), model)
	if !diags.HasError() {
		t.Fatal("expected an error for an explicit empty default_namespace")
	}
}

func TestBuildConfigMissingClusterIsNotAnError(t *testing.T) {
	clearKubeEnv(t)
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))

	model := emptyProviderModel()
	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("missing kubeconfig must not fail Configure: %v", diags)
	}
	if result.DefaultNamespace != "default" {
		t.Fatalf("namespace = %q, want default", result.DefaultNamespace)
	}
	if len(diags) == 0 {
		t.Fatal("expected a warning about the invalid configuration")
	}
}

func TestBuildConfigProxyURL(t *testing.T) {
	clearKubeEnv(t)
	t.Setenv("KUBE_PROXY_URL", "http://proxy.example.com:3128")

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(writeKubeconfig(t, testKubeconfig))
	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unexpected diagnostics: %v", diags)
	}
	if result.Config.Proxy == nil {
		t.Fatal("proxy URL was not applied")
	}
}

func TestBuildConfigUnknownHostIsSkipped(t *testing.T) {
	clearKubeEnv(t)

	model := emptyProviderModel()
	model.ConfigPath = types.StringValue(writeKubeconfig(t, testKubeconfig))
	model.Host = types.StringUnknown()

	result, diags := buildConfig(context.Background(), model)
	if diags.HasError() {
		t.Fatalf("unknown host must not fail validation: %v", diags)
	}
	if result.Config.Host != "https://from-file:6443" {
		t.Fatalf("host = %q, expected the kubeconfig host", result.Config.Host)
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home directory is not available")
	}
	expanded, err := expandPath("~/.kube/config")
	if err != nil {
		t.Fatalf("expandPath failed: %s", err)
	}
	if expanded != filepath.Join(home, ".kube", "config") {
		t.Fatalf("expanded = %q", expanded)
	}
	unchanged, err := expandPath("/absolute/path")
	if err != nil {
		t.Fatalf("expandPath failed: %s", err)
	}
	if unchanged != "/absolute/path" {
		t.Fatalf("expanded = %q", unchanged)
	}
}
