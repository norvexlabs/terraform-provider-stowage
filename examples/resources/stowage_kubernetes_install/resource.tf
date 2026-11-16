# A Stowage product's helm release on the cluster the provider's kubernetes block reaches. The
# installer runs on the machine running OpenTofu (its image's entry for that machine's platform,
# read from the registry), with helm and kubectl on PATH.
provider "stowage" {
  kubernetes {
    config_path    = "~/.kube/config"
    config_context = "prod"
  }
}

resource "stowage_kubernetes_install" "notes" {
  installer = "registry.example.com/notes/notes-installer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  namespace = "notes"

  inputs = { ADMIN_EMAIL = "ops@example.com" }
  secret_inputs_wo = {
    SMTP_PASSWORD = var.smtp_password
  }
  secret_inputs_wo_version = 1

  # The chart's values, after the installer's own.
  values = yamlencode({ ingress = { className = "nginx" } })
}

variable "smtp_password" {
  type      = string
  sensitive = true
  ephemeral = true
}
