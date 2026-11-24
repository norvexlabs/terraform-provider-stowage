terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.5"
    }
  }
}

provider "stowage" {
  username    = var.pull_username
  password    = var.pull_password
  public_keys = [var.release_public_key]
}
