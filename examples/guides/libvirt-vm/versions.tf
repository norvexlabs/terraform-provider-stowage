terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    libvirt = {
      source  = "dmacvicar/libvirt"
      version = "0.9.9"
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

provider "libvirt" {
  uri = var.uri
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
