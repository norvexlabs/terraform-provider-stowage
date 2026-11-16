# Quick start: a Stowage product on a Kubernetes cluster. The installer runs on this machine
# (its installer image's entry for this machine's platform, read from the registry: no Docker;
# helm >= 3.14 and kubectl on PATH), against the cluster the provider's
# kubernetes block reaches. `make install-local` in the provider's checkout first, then `tofu init`.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

provider "stowage" {
  kubernetes {
    config_path    = pathexpand("~/.kube/config")
    config_context = var.kube_context
  }
}

resource "stowage_kubernetes_install" "app" {
  installer = var.installer
  inputs    = { ADMIN_EMAIL = var.admin_email }
  namespace = "app"
}

output "status" {
  value = stowage_kubernetes_install.app.status
}
