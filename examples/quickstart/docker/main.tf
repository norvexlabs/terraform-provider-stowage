# Quick start: a Stowage product on a Docker host over SSH.
# `make install-local` in the provider's checkout first (no registry yet), then `tofu init`.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "0.1.0"
    }
  }
}

provider "stowage" {}

resource "stowage_install" "app" {
  # The installer by digest, as `stowage ship` prints it.
  installer = var.installer

  inputs = { ADMIN_EMAIL = var.admin_email }
  # Write-only: never in plan or state; bump the version to send a change.
  secret_inputs_wo         = { SMTP_PASSWORD = var.smtp_password }
  secret_inputs_wo_version = 1

  docker {
    host             = var.host
    user             = var.user
    known_hosts_line = var.known_hosts_line # ssh-keyscan -t ed25519 <host>, checked against the host's fingerprint
    work_dir         = "/srv"
  }
}

output "version" {
  value = stowage_install.app.version
}
