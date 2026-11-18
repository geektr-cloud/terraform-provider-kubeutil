package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// providerModel is the credential schema. Every attribute is optional; when an
// attribute is not configured the matching KUBE_* environment variable is used.
type providerModel struct {
	Host     types.String `tfsdk:"host"`
	Username types.String `tfsdk:"username"`
	Password types.String `tfsdk:"password"`
	Insecure types.Bool   `tfsdk:"insecure"`

	TLSServerName        types.String `tfsdk:"tls_server_name"`
	ClientCertificate    types.String `tfsdk:"client_certificate"`
	ClientKey            types.String `tfsdk:"client_key"`
	ClusterCACertificate types.String `tfsdk:"cluster_ca_certificate"`

	ConfigPath  types.String `tfsdk:"config_path"`
	ConfigPaths types.List   `tfsdk:"config_paths"`

	ConfigContext         types.String `tfsdk:"config_context"`
	ConfigContextAuthInfo types.String `tfsdk:"config_context_auth_info"`
	ConfigContextCluster  types.String `tfsdk:"config_context_cluster"`

	Token    types.String `tfsdk:"token"`
	ProxyURL types.String `tfsdk:"proxy_url"`

	DefaultNamespace types.String `tfsdk:"default_namespace"`

	Exec types.Object `tfsdk:"exec"`
}

type execModel struct {
	APIVersion types.String `tfsdk:"api_version"`
	Command    types.String `tfsdk:"command"`
	Args       types.List   `tfsdk:"args"`
	Env        types.Map    `tfsdk:"env"`
}

// configResult is the single-parser output: an in-memory rest.Config plus the
// effective default namespace. Building it performs no network I/O.
type configResult struct {
	Config           *rest.Config
	DefaultNamespace string
}

func buildConfig(ctx context.Context, model providerModel) (configResult, diag.Diagnostics) {
	var diags diag.Diagnostics

	// config_path and config_paths are mutually exclusive.
	explicitConfigPath := knownString(model.ConfigPath)
	explicitConfigPaths := knownNonEmptyStrings(ctx, model.ConfigPaths, &diags)
	if explicitConfigPath != "" && len(explicitConfigPaths) > 0 {
		diags.AddAttributeError(path.Root("config_path"), "Conflicting provider configuration",
			"config_path and config_paths are mutually exclusive; configure only one of them")
		return configResult{}, diags
	}

	configPaths := []string{}
	switch {
	case explicitConfigPath != "":
		configPaths = []string{explicitConfigPath}
	case len(explicitConfigPaths) > 0:
		configPaths = explicitConfigPaths
	default:
		// Not explicitly configured: fall back to the environment. An explicit
		// empty value suppresses the environment fallback, matching SDKv2.
		if envPath := os.Getenv("KUBE_CONFIG_PATH"); envPath != "" && (model.ConfigPath.IsNull() || model.ConfigPath.IsUnknown()) {
			configPaths = []string{envPath}
		} else if envPaths := os.Getenv("KUBE_CONFIG_PATHS"); envPaths != "" && (model.ConfigPaths.IsNull() || model.ConfigPaths.IsUnknown()) {
			for _, p := range filepath.SplitList(envPaths) {
				if p != "" {
					configPaths = append(configPaths, p)
				}
			}
		}
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if len(configPaths) > 0 {
		expanded := make([]string, 0, len(configPaths))
		for _, p := range configPaths {
			expandedPath, err := expandPath(p)
			if err != nil {
				diags.AddError("Invalid kubeconfig path", fmt.Sprintf("failed to expand kubeconfig path %q: %s", p, err))
				return configResult{}, diags
			}
			expanded = append(expanded, expandedPath)
		}
		if len(expanded) == 1 {
			loadingRules.ExplicitPath = expanded[0]
		} else {
			loadingRules.Precedence = expanded
		}
	}

	overrides := &clientcmd.ConfigOverrides{}

	// Context overrides apply to the standard loading rules as well as to an
	// explicitly supplied kubeconfig.
	currentContext := envOr(model.ConfigContext, "KUBE_CTX")
	authInfo := envOr(model.ConfigContextAuthInfo, "KUBE_CTX_AUTH_INFO")
	cluster := envOr(model.ConfigContextCluster, "KUBE_CTX_CLUSTER")
	if currentContext != "" || authInfo != "" || cluster != "" {
		overrides.CurrentContext = currentContext
		overrides.Context = clientcmdapi.Context{
			AuthInfo: authInfo,
			Cluster:  cluster,
		}
	}

	// Static credentials override whatever the kubeconfig supplied. This order
	// mirrors the SDKv2 provider: insecure, TLS server name, CA data, client
	// certificate data, host (which needs the TLS defaults), then auth data.
	insecure := false
	if !model.Insecure.IsNull() && !model.Insecure.IsUnknown() {
		insecure = model.Insecure.ValueBool()
	} else if env := os.Getenv("KUBE_INSECURE"); env != "" {
		parsed, err := strconv.ParseBool(env)
		if err != nil {
			diags.AddError("Invalid KUBE_INSECURE", fmt.Sprintf("KUBE_INSECURE must be a boolean, got %q", env))
			return configResult{}, diags
		}
		insecure = parsed
	}
	if insecure {
		overrides.ClusterInfo.InsecureSkipTLSVerify = true
	}

	// A configured TLS certificate or CA enables TLS for a scheme-less host,
	// matching rest.DefaultServerURL's defaulting in the SDKv2 provider.
	if v := envOr(model.TLSServerName, "KUBE_TLS_SERVER_NAME"); v != "" {
		overrides.ClusterInfo.TLSServerName = v
	}
	if v := envOr(model.ClusterCACertificate, "KUBE_CLUSTER_CA_CERT_DATA"); v != "" {
		overrides.ClusterInfo.CertificateAuthorityData = []byte(v)
	}
	if v := envOr(model.ClientCertificate, "KUBE_CLIENT_CERT_DATA"); v != "" {
		overrides.AuthInfo.ClientCertificateData = []byte(v)
	}

	if host := envOr(model.Host, "KUBE_HOST"); host != "" {
		hasCA := len(overrides.ClusterInfo.CertificateAuthorityData) != 0
		hasCert := len(overrides.AuthInfo.ClientCertificateData) != 0
		defaultTLS := (hasCA || hasCert) && !overrides.ClusterInfo.InsecureSkipTLSVerify
		server, _, err := rest.DefaultServerURL(host, "", schema.GroupVersion{}, defaultTLS)
		if err != nil {
			diags.AddAttributeError(path.Root("host"), "Invalid Kubernetes host", fmt.Sprintf("failed to parse host %q: %s", host, err))
			return configResult{}, diags
		}
		overrides.ClusterInfo.Server = server.String()
	}

	if v := envOr(model.Username, "KUBE_USER"); v != "" {
		overrides.AuthInfo.Username = v
	}
	if v := envOr(model.Password, "KUBE_PASSWORD"); v != "" {
		overrides.AuthInfo.Password = v
	}
	if v := envOr(model.ClientKey, "KUBE_CLIENT_KEY_DATA"); v != "" {
		overrides.AuthInfo.ClientKeyData = []byte(v)
	}
	if v := envOr(model.Token, "KUBE_TOKEN"); v != "" {
		overrides.AuthInfo.Token = v
	}
	if v := envOr(model.ProxyURL, "KUBE_PROXY_URL"); v != "" {
		overrides.ClusterDefaults.ProxyURL = v
	}

	execConfig, execDiags := buildExec(ctx, model.Exec)
	diags.Append(execDiags...)
	if diags.HasError() {
		return configResult{}, diags
	}
	if execConfig != nil {
		overrides.AuthInfo.Exec = execConfig
	}

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)

	cfg, err := clientConfig.ClientConfig()
	configErr := err
	if err != nil {
		// A missing kubeconfig or an unreachable cluster must not fail provider
		// configuration. Operations that actually need the API will report the
		// underlying problem.
		diags.AddWarning("Provider was supplied an invalid configuration",
			fmt.Sprintf("Further operations are likely to fail: %s", err))
		cfg = &rest.Config{Host: "http://localhost"}
	}

	namespace := ""
	switch {
	case model.DefaultNamespace.IsUnknown():
		// An unknown provider value cannot be validated; fall back to the
		// credential context namespace.
	case !model.DefaultNamespace.IsNull():
		if model.DefaultNamespace.ValueString() == "" {
			diags.AddAttributeError(path.Root("default_namespace"), "Invalid default namespace",
				"default_namespace must not be empty when set; remove the attribute to inherit the namespace from the kubeconfig")
			return configResult{}, diags
		}
		namespace = model.DefaultNamespace.ValueString()
	default:
		namespace = os.Getenv("KUBE_NAMESPACE")
	}
	if namespace == "" && configErr == nil {
		if resolved, _, err := clientConfig.Namespace(); err != nil {
			diags.AddWarning("Could not determine the kubeconfig namespace",
				fmt.Sprintf("Falling back to the literal namespace \"default\": %s", err))
		} else {
			namespace = resolved
		}
	}
	if namespace == "" {
		namespace = "default"
	}

	return configResult{Config: cfg, DefaultNamespace: namespace}, diags
}

func buildExec(ctx context.Context, value types.Object) (*clientcmdapi.ExecConfig, diag.Diagnostics) {
	var diags diag.Diagnostics
	if value.IsNull() || value.IsUnknown() {
		return nil, diags
	}

	var model execModel
	diags.Append(value.As(ctx, &model, basetypes.ObjectAsOptions{})...)
	if diags.HasError() {
		return nil, diags
	}

	apiVersion := model.APIVersion.ValueString()
	if apiVersion == "" {
		diags.AddAttributeError(path.Root("exec").AtName("api_version"), "Invalid exec configuration", "exec.api_version must not be empty")
		return nil, diags
	}
	if _, err := schema.ParseGroupVersion(apiVersion); err != nil {
		diags.AddAttributeError(path.Root("exec").AtName("api_version"), "Invalid exec configuration",
			fmt.Sprintf("exec.api_version %q is not a valid group/version: %s", apiVersion, err))
	}
	if apiVersion == "client.authentication.k8s.io/v1alpha1" {
		diags.AddAttributeWarning(path.Root("exec").AtName("api_version"), "Deprecated exec credential API version",
			"client.authentication.k8s.io/v1alpha1 is deprecated; use v1beta1 or v1. The v1alpha1 API may be removed by future Kubernetes client versions.")
	}
	if model.Command.ValueString() == "" {
		diags.AddAttributeError(path.Root("exec").AtName("command"), "Invalid exec configuration", "exec.command must not be empty")
	}
	if diags.HasError() {
		return nil, diags
	}

	// Exec arguments must be preserved exactly, including deliberate empty
	// strings.
	args := knownStrings(ctx, model.Args, &diags)

	execConfig := &clientcmdapi.ExecConfig{
		APIVersion:      apiVersion,
		Command:         model.Command.ValueString(),
		Args:            args,
		InteractiveMode: clientcmdapi.IfAvailableExecInteractiveMode,
	}

	if !model.Env.IsNull() && !model.Env.IsUnknown() {
		elements := model.Env.Elements()
		names := make([]string, 0, len(elements))
		for name := range elements {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			value, ok := elements[name].(types.String)
			if !ok {
				diags.AddAttributeError(path.Root("exec").AtName("env").AtMapKey(name), "Invalid exec configuration",
					fmt.Sprintf("exec.env[%q] must be a string", name))
				continue
			}
			if value.IsNull() || value.IsUnknown() {
				// Never include the value in the diagnostic: env values can be
				// credentials.
				diags.AddAttributeError(path.Root("exec").AtName("env").AtMapKey(name), "Invalid exec configuration",
					fmt.Sprintf("exec.env[%q] must be a known string", name))
				continue
			}
			execConfig.Env = append(execConfig.Env, clientcmdapi.ExecEnvVar{Name: name, Value: value.ValueString()})
		}
	}

	return execConfig, diags
}

// envOr returns an explicitly configured value, or the environment variable
// when the attribute is absent. An explicitly configured empty string does not
// fall back to the environment, matching SDKv2 DefaultFunc semantics.
func envOr(value types.String, env string) string {
	if value.IsUnknown() {
		return ""
	}
	if !value.IsNull() {
		return value.ValueString()
	}
	return os.Getenv(env)
}

func knownString(value types.String) string {
	if value.IsNull() || value.IsUnknown() {
		return ""
	}
	return value.ValueString()
}

// knownStrings returns the list elements unchanged, including empty strings.
func knownStrings(ctx context.Context, value types.List, diags *diag.Diagnostics) []string {
	if value.IsNull() || value.IsUnknown() {
		return nil
	}
	var result []string
	diags.Append(value.ElementsAs(ctx, &result, false)...)
	return result
}

// knownNonEmptyStrings drops empty elements. It is used for kubeconfig path
// lists, where an empty entry carries no meaning.
func knownNonEmptyStrings(ctx context.Context, value types.List, diags *diag.Diagnostics) []string {
	items := knownStrings(ctx, value, diags)
	out := items[:0]
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// expandPath expands a leading "~" or "~/" to the current user's home
// directory. Other forms, including "~user", are left unchanged so path errors
// surface from the kubeconfig loader.
func expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}
