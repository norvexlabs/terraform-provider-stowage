terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    proxmox = {
      source  = "bpg/proxmox"
      version = "0.117.0"
    }
    external = {
      source  = "hashicorp/external"
      version = "2.3.5"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The API's address and token come from PROXMOX_VE_ENDPOINT and PROXMOX_VE_API_TOKEN (or
# PROXMOX_VE_USERNAME / PROXMOX_VE_PASSWORD): never from a file here.
provider "proxmox" {}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
