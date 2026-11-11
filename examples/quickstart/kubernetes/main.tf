# Quick start: a Stowage product on a Kubernetes cluster. The installer runs on this machine
# (linux/amd64, with docker, helm >= 3.14 and kubectl), against the cluster the provider's
# kubernetes block reaches. `make install-local` in the provider's checkout first, then `tofu init`.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "0.1.0"
    }
  }
}

provider "stowage" {
  kubernetes {
    config_path    = pathexpand("~/.kube/config")
    config_context = var.kube_context
  }
}

resource "stowage_install" "app" {
  installer = var.installer
  inputs    = { ADMIN_EMAIL = var.admin_email }

  kubernetes {
    namespace = "app"
  }
}

output "status" {
  value = stowage_install.app.status
}
