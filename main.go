// Command terraform-provider-stowage is the OpenTofu provider for Stowage.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/norvexlabs/terraform-provider-stowage/internal/provider"
)

// version is set by the release build (GoReleaser's ldflags).
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run with support for debuggers such as delve")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address:         "registry.opentofu.org/norvexlabs/stowage",
		Debug:           debug,
		ProtocolVersion: 6,
	})
	if err != nil {
		log.Fatal(err.Error())
	}
}
