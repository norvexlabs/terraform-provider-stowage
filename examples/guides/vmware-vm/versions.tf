terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    vsphere = {
      source  = "vmware/vsphere"
      version = "2.17.1"
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

# The server and the account come from VSPHERE_SERVER, VSPHERE_USER and VSPHERE_PASSWORD (and
# VSPHERE_ALLOW_UNVERIFIED_SSL): never from a file here.
provider "vsphere" {}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
