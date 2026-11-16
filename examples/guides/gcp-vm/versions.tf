terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    google = {
      source  = "hashicorp/google"
      version = "8.7.0"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The account comes from the environment (GOOGLE_APPLICATION_CREDENTIALS, or the gcloud
# application-default login): never from a file here.
provider "google" {
  project = var.project
  zone    = var.zone
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
