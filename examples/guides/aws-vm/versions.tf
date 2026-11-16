terraform {
  required_version = ">= 1.11.0"
  required_providers {
    # Pinned exactly, with its checksums in .terraform.lock.hcl.
    aws = {
      source  = "hashicorp/aws"
      version = "6.69.1"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The account comes from the environment (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY, AWS_PROFILE,
# or the instance's role): never from a file here.
provider "aws" {
  region = var.region
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  registry    = var.registry
  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null
}
