package provider

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/testsupport"
)

// accFactories starts the provider in the test process. The factory key is the
// provider type name; the namespace is controlled by
// TF_ACC_PROVIDER_NAMESPACE, so reattach resolves to
// registry.terraform.io/geektr-cloud/kubeutil.
func accFactories(t *testing.T, p *KubeutilProvider) map[string]func() (tfprotov6.ProviderServer, error) {
	t.Helper()
	t.Setenv("TF_ACC_PROVIDER_NAMESPACE", "geektr-cloud")
	return map[string]func() (tfprotov6.ProviderServer, error){
		"kubeutil": providerserver.NewProtocol6WithError(p),
	}
}

func isolateKubeEnv(t *testing.T) {
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
	// A missing kubeconfig keeps the explicit host in the provider block as the
	// only source of cluster connection information.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "absent-kubeconfig"))
}

func fakeProviderConfig(url string) string {
	return fmt.Sprintf(`terraform {
  required_providers {
    kubeutil = {
      source = "geektr-cloud/kubeutil"
    }
  }
}

provider "kubeutil" {
  host              = %q
  default_namespace = "default"
  token             = "test-token"
}
`, url)
}

const fakeServiceAndPod = `
data "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "web"
}
`

func fakeTunnelBackend(t *testing.T, backendAddress string) *testsupport.Fake {
	t.Helper()
	fake := testsupport.New()
	fake.SetServices(testsupport.Service(testsupport.ServiceOptions{
		Namespace: "default",
		Name:      "echo",
		Selector:  map[string]string{"app": "echo"},
		Ports: []corev1.ServicePort{
			testsupport.ServicePort("web", 80, intstr.FromString("container-port"), corev1.ProtocolTCP),
		},
	}))
	fake.SetPods(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "echo",
		Labels:    map[string]string{"app": "echo"},
		Ports: []corev1.ContainerPort{
			{Name: "container-port", ContainerPort: 8080, Protocol: corev1.ProtocolTCP},
		},
	}))
	fake.SetBackend(8080, backendAddress)
	return fake
}

func dialFromState(address string, payload string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		resourceState, ok := state.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("resource %s not found in state", address)
		}
		host := resourceState.Primary.Attributes["host"]
		port := resourceState.Primary.Attributes["local_port"]
		if host != "127.0.0.1" {
			return fmt.Errorf("host = %q, want 127.0.0.1", host)
		}
		if _, err := strconv.Atoi(port); err != nil {
			return fmt.Errorf("local_port %q is not numeric: %w", port, err)
		}
		hostport := resourceState.Primary.Attributes["hostport"]
		if hostport != net.JoinHostPort(host, port) {
			return fmt.Errorf("hostport %q does not match %s:%s", hostport, host, port)
		}

		conn, err := net.DialTimeout("tcp", hostport, 5*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", hostport, err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		if _, err := conn.Write([]byte(payload)); err != nil {
			return fmt.Errorf("failed to write through tunnel: %w", err)
		}
		buffer := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, buffer); err != nil {
			return fmt.Errorf("failed to read through tunnel: %w", err)
		}
		if string(buffer) != payload {
			return fmt.Errorf("tunnel echo = %q, want %q", string(buffer), payload)
		}
		return nil
	}
}

func TestAccDataSourceTunnel(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()
	fake := fakeTunnelBackend(t, echoAddress)
	defer fake.Close()

	p := New("test")().(*KubeutilProvider)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config: fakeProviderConfig(fake.URL()) + fakeServiceAndPod,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.test", "host", "127.0.0.1"),
					resource.TestCheckResourceAttrSet("data.kubeutil_tunnel.test", "local_port"),
					resource.TestCheckResourceAttrSet("data.kubeutil_tunnel.test", "hostport"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.test", "namespace", "default"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.test", "id", "default/service/echo/web"),
					dialFromState("data.kubeutil_tunnel.test", "through-the-cli"),
				),
			},
		},
	})
}

func TestAccDataSourceTunnelInvalidPortMakesNoRequests(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()
	fake := fakeTunnelBackend(t, echoAddress)
	defer fake.Close()

	p := New("test")().(*KubeutilProvider)
	validConfig := fakeProviderConfig(fake.URL()) + fakeServiceAndPod
	invalidConfig := fakeProviderConfig(fake.URL()) + `
data "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "8888:5432"
}
`

	afterFirstStep := 0
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config: validConfig,
				Check: func(_ *terraform.State) error {
					afterFirstStep = fake.Count("service.get")
					return nil
				},
			},
			{
				Config:      invalidConfig,
				ExpectError: regexp.MustCompile("local port cannot be specified"),
			},
			{
				Config: validConfig,
				PreConfig: func() {
					if got := fake.Count("service.get"); got != afterFirstStep {
						t.Fatalf("invalid configuration produced %d extra service reads", got-afterFirstStep)
					}
				},
			},
		},
	})
}

func TestAccDataSourceTunnelHealthcheckFailure(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)

	fake := fakeTunnelBackend(t, testsupport.ClosedAddress())
	defer fake.Close()

	p := New("test")().(*KubeutilProvider)
	config := fakeProviderConfig(fake.URL()) + `
data "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "web"

  healthcheck {
    type = "telnet"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile("(?i)healthcheck"),
			},
		},
	})

	if p.registry.Len() != 0 {
		t.Fatalf("failed acquisition was cached: registry has %d entries", p.registry.Len())
	}
}

func TestAccEphemeralTunnel(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()

	fake := fakeTunnelBackend(t, echoAddress)
	defer fake.Close()
	// A second pod and backend turn the ephemeral tunnel into a path to the API
	// server itself, so the tunnel value can be consumed in an ephemeral context
	// (a provider configuration) and exercised end to end.
	fake.AddPod(testsupport.Pod(testsupport.PodOptions{
		Namespace: "default",
		Name:      "api",
		Ports:     []corev1.ContainerPort{{ContainerPort: 9090, Protocol: corev1.ProtocolTCP}},
	}))
	fake.SetBackend(9090, strings.TrimPrefix(fake.URL(), "http://"))

	p := New("test")().(*KubeutilProvider)
	config := fakeProviderConfig(fake.URL()) + `
ephemeral "kubeutil_tunnel" "api" {
  target = "pod/api"
  port   = "9090"

  healthcheck {
    type = "telnet"
  }
}

provider "kubeutil" {
  alias             = "through_ephemeral"
  host              = "http://${ephemeral.kubeutil_tunnel.api.hostport}"
  token             = "test-token"
  default_namespace = "default"
}

data "kubeutil_tunnel" "inner" {
  provider = kubeutil.through_ephemeral
  target   = "pod/echo"
  port     = "8080"
}
`
	leakedConfig := fakeProviderConfig(fake.URL()) + `
ephemeral "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "web"
}

output "leak" {
  value = ephemeral.kubeutil_tunnel.test.hostport
}
`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config:      leakedConfig,
				ExpectError: regexp.MustCompile("(?i)ephemeral"),
			},
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.inner", "host", "127.0.0.1"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.inner", "id", "default/pod/echo/8080"),
					resource.TestCheckResourceAttrSet("data.kubeutil_tunnel.inner", "local_port"),
				),
				PostApplyFunc: func() {
					if fake.SPDYAttempts() < 2 {
						t.Fatalf("expected at least two forwarding upgrades (ephemeral outer and data source inner), got %d", fake.SPDYAttempts())
					}
					for _, key := range p.registry.Keys() {
						if key.String() == "default/pod/api/9090" {
							t.Fatalf("ephemeral tunnel was not released on Close; registry keys = %v", p.registry.Keys())
						}
					}
				},
			},
		},
	})
}

// TestAccEphemeralTunnelDoesNotPersist verifies against real CLI artifacts
// that an ephemeral tunnel leaves no values in state or in a saved plan. The
// provider is configured entirely through environment variables so that the
// only possible source of a 127.0.0.1:<port> literal in an artifact would be a
// leaked ephemeral value.
func TestAccEphemeralTunnelDoesNotPersist(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)
	// Keep the harness working directories so the artifacts can be inspected.
	t.Setenv("TF_ACC_PERSIST_WORKING_DIR", "1")

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()
	fake := fakeTunnelBackend(t, echoAddress)
	defer fake.Close()
	t.Setenv("KUBE_HOST", fake.URL())
	t.Setenv("KUBE_TOKEN", "test-token")
	t.Setenv("KUBE_NAMESPACE", "default")

	p := New("test")().(*KubeutilProvider)
	workingDir := t.TempDir()

	config := `terraform {
  required_providers {
    kubeutil = {
      source = "geektr-cloud/kubeutil"
    }
  }
}

provider "kubeutil" {}

ephemeral "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "web"
}
`
	leakedConfig := config + `
output "leak" {
  value = ephemeral.kubeutil_tunnel.test.hostport
}
`

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               workingDir,
		Steps: []resource.TestStep{
			{
				Config:      leakedConfig,
				ExpectError: regexp.MustCompile("(?i)ephemeral"),
			},
			{
				Config: config,
				Check: func(state *terraform.State) error {
					for address := range state.RootModule().Resources {
						if strings.HasPrefix(address, "ephemeral.") {
							return fmt.Errorf("ephemeral resource %q appeared in harness state", address)
						}
					}
					return nil
				},
			},
		},
	})

	if fake.SPDYAttempts() == 0 {
		t.Fatal("the ephemeral resource never opened a tunnel; the persistence check would be vacuous")
	}
	assertNoEphemeralArtifacts(t, workingDir)
}

type tunnelArtifacts struct {
	states [][]byte
	plans  [][]byte
}

func collectTunnelArtifacts(t *testing.T, root string) tunnelArtifacts {
	t.Helper()
	var artifacts tunnelArtifacts

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		name := entry.Name()
		switch {
		case name == "tfplan" || strings.HasSuffix(name, ".tfplan"):
			artifacts.plans = append(artifacts.plans, readTunnelArtifact(t, path))
		case strings.HasSuffix(name, ".tfstate"), strings.HasSuffix(name, ".tfstate.backup"):
			artifacts.states = append(artifacts.states, readTunnelArtifact(t, path))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk working directory: %s", err)
	}
	return artifacts
}

// readTunnelArtifact returns the file contents, expanding plan files that are
// zip archives.
func readTunnelArtifact(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read artifact %s: %s", path, err)
	}

	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return data
	}

	var expanded []byte
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatalf("failed to open plan entry %s: %s", file.Name, err)
		}
		content, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatalf("failed to read plan entry %s: %s", file.Name, err)
		}
		expanded = append(expanded, content...)
	}
	return expanded
}

// artifactKind selects the leak indicators that make sense for an artifact.
// Saved plans embed configuration and, on some CLIs, provider schema text, so
// they are only checked for concrete endpoint values. State must additionally
// never contain an ephemeral resource instance.
type artifactKind int

const (
	artifactState artifactKind = iota
	artifactPlan
)

var (
	// loopbackEndpoint matches a concrete tunnel endpoint such as
	// 127.0.0.1:54321. It deliberately does not match schema descriptions like
	// "127.0.0.1:<local_port>" or attribute names such as hostport/local_port.
	loopbackEndpoint = regexp.MustCompile(`127\.0\.0\.1:[0-9]+`)
	// ephemeralMode matches a persisted ephemeral resource instance.
	ephemeralMode = regexp.MustCompile(`"mode"\s*:\s*"ephemeral"`)
	// ephemeralAddress matches the ephemeral resource instance address.
	ephemeralAddress = regexp.MustCompile(`ephemeral\.kubeutil_tunnel`)
)

// findEphemeralLeaks returns concrete indicators that an ephemeral tunnel value
// or instance was persisted. Schema attribute names are intentionally ignored:
// some CLIs embed provider schema text in saved plans.
func findEphemeralLeaks(data []byte, kind artifactKind) []string {
	leaks := make([]string, 0, 1)
	for _, match := range loopbackEndpoint.FindAll(data, -1) {
		leaks = append(leaks, string(match))
	}
	if kind == artifactState {
		for _, match := range ephemeralMode.FindAll(data, -1) {
			leaks = append(leaks, string(match))
		}
		for _, match := range ephemeralAddress.FindAll(data, -1) {
			leaks = append(leaks, string(match))
		}
	}
	return leaks
}

func assertNoEphemeralArtifacts(t *testing.T, root string) {
	t.Helper()
	artifacts := collectTunnelArtifacts(t, root)
	if len(artifacts.states) == 0 {
		t.Fatal("no state artifacts were produced; the persistence check would be vacuous")
	}
	if len(artifacts.plans) == 0 {
		t.Fatal("no saved plan artifacts were produced; the persistence check would be vacuous")
	}
	t.Logf("inspecting %d state and %d plan artifacts", len(artifacts.states), len(artifacts.plans))

	// Prove the inspected plan is the real one: its configuration must mention
	// the ephemeral resource type that was applied.
	planMentionsResource := false
	for _, plan := range artifacts.plans {
		if bytes.Contains(plan, []byte("kubeutil_tunnel")) {
			planMentionsResource = true
			break
		}
	}
	if !planMentionsResource {
		t.Fatal("saved plan does not mention the configured ephemeral resource; artifact inspection is not meaningful")
	}

	for _, state := range artifacts.states {
		for _, leak := range findEphemeralLeaks(state, artifactState) {
			t.Errorf("state contains %q, so an ephemeral tunnel value or instance was persisted: %s", leak, excerpt(state, leak))
		}
	}

	for _, plan := range artifacts.plans {
		for _, leak := range findEphemeralLeaks(plan, artifactPlan) {
			t.Errorf("saved plan contains %q, so an ephemeral tunnel endpoint leaked into the plan: %s", leak, excerpt(plan, leak))
		}
	}
}

// TestEphemeralArtifactLeakScanner proves the scanner distinguishes schema text
// from concrete values: provider schema attribute names and descriptions are
// not leaks, while a concrete loopback endpoint or a persisted ephemeral
// instance is.
func TestEphemeralArtifactLeakScanner(t *testing.T) {
	t.Parallel()

	schemaText := []byte(`{"attributes":{"hostport":{"type":"string","description":"127.0.0.1:<local_port>"},"local_port":{"type":"number"}}}`)
	if leaks := findEphemeralLeaks(schemaText, artifactPlan); len(leaks) != 0 {
		t.Fatalf("schema text must not be reported as a plan leak: %v", leaks)
	}
	if leaks := findEphemeralLeaks(schemaText, artifactState); len(leaks) != 0 {
		t.Fatalf("schema text must not be reported as a state leak: %v", leaks)
	}

	planValue := []byte(`{"planned_values":{"hostport":"127.0.0.1:54321"}}`)
	leaks := findEphemeralLeaks(planValue, artifactPlan)
	if len(leaks) != 1 || leaks[0] != "127.0.0.1:54321" {
		t.Fatalf("concrete endpoint not detected in plan: %v", leaks)
	}

	ephemeralInstance := []byte(`{"resources":[{"address":"ephemeral.kubeutil_tunnel.test","mode":"ephemeral","type":"kubeutil_tunnel","name":"test","values":{"hostport":"127.0.0.1:54321"}}]}`)
	leaks = findEphemeralLeaks(ephemeralInstance, artifactState)
	if len(leaks) != 3 {
		t.Fatalf("expected endpoint, mode and address leaks in state, got %v", leaks)
	}

	// A plan's configuration legitimately names the ephemeral resource and its
	// module address; only concrete values are leaks.
	planConfig := []byte(`{"configuration":{"root_module":{"resources":[{"address":"ephemeral.kubeutil_tunnel.test","mode":"ephemeral"}]}}}`)
	if leaks := findEphemeralLeaks(planConfig, artifactPlan); len(leaks) != 0 {
		t.Fatalf("plan configuration addresses must not be reported as leaks: %v", leaks)
	}
}

func excerpt(data []byte, needle string) string {
	index := bytes.Index(data, []byte(needle))
	if index < 0 {
		return ""
	}
	start := index - 40
	if start < 0 {
		start = 0
	}
	end := index + 80
	if end > len(data) {
		end = len(data)
	}
	return string(data[start:end])
}

func TestAccEphemeralTunnelUnsupportedHealthcheck(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	isolateKubeEnv(t)

	echoAddress, stopEcho := testsupport.EchoServer()
	defer stopEcho()
	fake := fakeTunnelBackend(t, echoAddress)
	defer fake.Close()

	p := New("test")().(*KubeutilProvider)
	config := fakeProviderConfig(fake.URL()) + `
ephemeral "kubeutil_tunnel" "test" {
  target = "service/echo"
  port   = "web"

  healthcheck {
    type = "http"
  }
}
`
	requestsBefore := fake.Count("service.get")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile("(?i)healthcheck"),
			},
		},
	})
	if got := fake.Count("service.get"); got != requestsBefore {
		t.Fatalf("invalid healthcheck produced %d API reads", got-requestsBefore)
	}
}
