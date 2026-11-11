variable "installer" {
  type        = string
  description = "The installer image by digest, <prefix>/<name>-installer@sha256:<64 hex>."
}

variable "kube_context" {
  type        = string
  description = "The kubeconfig context of the cluster."
}

variable "admin_email" {
  type = string
}
