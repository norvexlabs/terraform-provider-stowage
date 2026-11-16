terraform {
  required_version = ">= 1.11.0"
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.69.1"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "4.4.2"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The account comes from the AWS SDK's own chain (AWS_PROFILE, AWS_ACCESS_KEY_ID…): never from a
# file here.
provider "aws" {
  region = var.region
}

provider "stowage" {
  username = var.pull_username
  password = var.pull_password
}
