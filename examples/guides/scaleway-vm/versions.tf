terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    scaleway = {
      source  = "scaleway/scaleway"
      version = "2.91.0"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The account comes from the environment (SCW_ACCESS_KEY / SCW_SECRET_KEY, or the scw CLI's
# configuration file and profile): never from a file here. The bucket is in the zone's region,
# where the snapshot's import reads it.
provider "scaleway" {
  zone       = var.zone
  region     = try(regex("^([a-z]+-[a-z]+)-[0-9]+$", var.zone)[0], null)
  project_id = var.project == "" ? null : var.project
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
