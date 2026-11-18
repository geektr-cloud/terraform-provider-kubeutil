package provider

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// requireKind skips unless both TF_ACC and KUBEUTIL_KIND=1 are set. The
// cluster must already contain the Deployment and Service named echo in the
// default namespace, with a container port named http serving HTTP.
func requireKind(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be set to run acceptance tests")
	}
	if os.Getenv("KUBEUTIL_KIND") != "1" {
		t.Skip("KUBEUTIL_KIND=1 must be set to run kind acceptance tests")
	}
}

func kindProviderConfig() string {
	return `terraform {
  required_providers {
    kubeutil = {
      source = "geektr-cloud/kubeutil"
    }
  }
}

provider "kubeutil" {}
`
}

func httpGetFromState(address string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		resourceState, ok := state.RootModule().Resources[address]
		if !ok {
			return fmt.Errorf("resource %s not found in state", address)
		}
		hostport := resourceState.Primary.Attributes["hostport"]
		conn, err := net.DialTimeout("tcp", hostport, 10*time.Second)
		if err != nil {
			return fmt.Errorf("failed to connect to %s: %w", hostport, err)
		}
		defer func() { _ = conn.Close() }()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return err
		}
		request := "GET / HTTP/1.1\r\nHost: echo\r\nConnection: close\r\n\r\n"
		if _, err := conn.Write([]byte(request)); err != nil {
			return fmt.Errorf("failed to write HTTP request through tunnel: %w", err)
		}
		response, err := io.ReadAll(conn)
		if err != nil {
			return fmt.Errorf("failed to read HTTP response through tunnel: %w", err)
		}
		text := string(response)
		if !strings.HasPrefix(text, "HTTP/1.1 200") && !strings.HasPrefix(text, "HTTP/1.0 200") {
			return fmt.Errorf("unexpected HTTP response through %s: %q", hostport, firstLine(text))
		}
		return nil
	}
}

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return strings.TrimSpace(text)
}

func TestAccKindDataSourceTunnel(t *testing.T) {
	requireKind(t)

	p := New("test")().(*KubeutilProvider)
	config := kindProviderConfig() + `
data "kubeutil_tunnel" "deployment" {
  target    = "deployment/echo"
  port      = "http"
  namespace = "default"
}

data "kubeutil_tunnel" "service" {
  target    = "service/echo"
  port      = "80"
  namespace = "default"
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.deployment", "host", "127.0.0.1"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.deployment", "id", "default/deployment/echo/http"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.deployment", "namespace", "default"),
					resource.TestCheckResourceAttrSet("data.kubeutil_tunnel.deployment", "local_port"),
					httpGetFromState("data.kubeutil_tunnel.deployment"),
					resource.TestCheckResourceAttr("data.kubeutil_tunnel.service", "id", "default/service/echo/80"),
					httpGetFromState("data.kubeutil_tunnel.service"),
				),
			},
		},
	})
}

func TestAccKindEphemeralTunnel(t *testing.T) {
	requireKind(t)

	p := New("test")().(*KubeutilProvider)
	config := kindProviderConfig() + `
ephemeral "kubeutil_tunnel" "test" {
  target    = "service/echo"
  port      = "http"
  namespace = "default"

  healthcheck {
    type = "telnet"
  }
}

# Consume the ephemeral endpoint during apply and make a real HTTP request
# through the returned hostport. Provisioner configuration is not persisted, so
# Terraform allows ephemeral values here.
resource "terraform_data" "ephemeral_probe" {
  provisioner "local-exec" {
    command = "curl -fsS --max-time 15 http://${ephemeral.kubeutil_tunnel.test.hostport}/"
  }
}
`
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: accFactories(t, p),
		WorkingDir:               t.TempDir(),
		Steps: []resource.TestStep{
			{
				Config: config,
				PostApplyFunc: func() {
					if p.registry.Len() != 0 {
						t.Fatalf("ephemeral Close did not release the tunnel: registry has %d entries", p.registry.Len())
					}
				},
			},
		},
	})
}
