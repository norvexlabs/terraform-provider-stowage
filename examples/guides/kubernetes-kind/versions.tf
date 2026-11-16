terraform {
  required_version = ">= 1.11.0"
  required_providers {
    kind = {
      source  = "tehcyx/kind"
      version = "0.13.0"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "3.3.0"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
    }
  }
}

# The cluster's credentials are kind_cluster's attributes: no kubeconfig file is read.
provider "helm" {
  kubernetes = {
    host                   = kind_cluster.this.endpoint
    cluster_ca_certificate = kind_cluster.this.cluster_ca_certificate
    client_certificate     = kind_cluster.this.client_certificate
    client_key             = kind_cluster.this.client_key
  }
}

provider "stowage" {
  username = var.pull_username
  password = var.pull_password
  registry = var.registry

  kubernetes {
    host                   = kind_cluster.this.endpoint
    cluster_ca_certificate = kind_cluster.this.cluster_ca_certificate
    client_certificate     = kind_cluster.this.client_certificate
    client_key             = kind_cluster.this.client_key
  }
}
