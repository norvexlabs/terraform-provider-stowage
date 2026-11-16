terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # TrueNAS's own provider, pinned exactly with its checksums in .terraform.lock.hcl.
    truenas = {
      source  = "truenas/truenas"
      version = "1.9.1"
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

# The API's endpoint and key come from TRUENAS_ENDPOINT (wss://<host>/api/current) and
# TRUENAS_API_KEY: never from a file here. The Stowage steps read the same variables.
provider "truenas" {
  insecure = var.insecure
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
