package provider

import (
	"fmt"
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccDockerLocal installs a real installer on this machine's Docker and uninstalls it,
// through OpenTofu. It runs only with TF_ACC=1, TF_ACC_TERRAFORM_PATH set to a tofu binary
// (TF_ACC_PROVIDER_NAMESPACE=norvexlabs, TF_ACC_PROVIDER_HOST=registry.opentofu.org) and
// STOWAGE_ACC_INSTALLER an installer image by digest; STOWAGE_ACC_WORK_DIR is the directory it
// installs in.
func TestAccDockerLocal(t *testing.T) {
	ref, dir := os.Getenv("STOWAGE_ACC_INSTALLER"), os.Getenv("STOWAGE_ACC_WORK_DIR")
	if os.Getenv("TF_ACC") != "" && (ref == "" || dir == "") {
		t.Skip("STOWAGE_ACC_INSTALLER and STOWAGE_ACC_WORK_DIR are not set")
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){
			"stowage": providerserver.NewProtocol6WithError(New("acc")()),
		},
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource "stowage_install" "this" {
  installer           = %q
  deletion_protection = false
  delete_data         = true
  docker {
    work_dir = %q
  }
}`, ref, dir),
			Check: resource.TestCheckResourceAttr("stowage_install.this", "status", "installed"),
		}},
	})
}
