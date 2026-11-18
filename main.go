// Command terraform-provider-kubeutil serves the kubeutil Terraform provider.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/geektr-cloud/terraform-provider-kubeutil/internal/provider"
)

// version is injected at build time with -X main.version=<version>.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "set to true to run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address:         "registry.terraform.io/geektr-cloud/kubeutil",
		ProtocolVersion: 6,
		Debug:           debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
