variable "installer" {
  type        = string
  description = "The installer image by digest, <prefix>/<name>-installer@sha256:<64 hex>."
}

variable "host" {
  type        = string
  description = "The Docker host, reached by ssh."
}

variable "user" {
  type        = string
  description = "The SSH user, in the docker group."
}

variable "known_hosts_line" {
  type        = string
  description = "The host's known_hosts line (host keytype base64)."
}

variable "admin_email" {
  type = string
}

variable "smtp_password" {
  type      = string
  sensitive = true
  ephemeral = true
}
