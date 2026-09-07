// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/vmvarela/terraform-provider-llm/internal/provider"
)

var version = "dev"

func main() {
	debug := flag.Bool("debug", false, "Enable managed debugging support")
	flag.Parse()
	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/vmvarela/llm",
		Debug:   *debug,
	}); err != nil {
		log.Fatal(err)
	}
}
