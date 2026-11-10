terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source = "registry.opentofu.org/norvexlabs/stowage"
    }
  }
}

# The pull account fetches the installer image on the target; left out, the target's own
# `docker login` does.
provider "stowage" {
  username = "pull"
  password = var.pull_password
}

variable "pull_password" {
  type      = string
  sensitive = true
}
