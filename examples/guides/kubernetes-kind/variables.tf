variable "installer" {
  description = "The installer image by digest, as `stowage ship` prints it."
  type        = string
}

variable "name" {
  description = "The cluster's name."
  type        = string
  default     = "notes"
}

variable "namespace" {
  type    = string
  default = "notes"
}

variable "inputs" {
  type    = map(string)
  default = {}
}

variable "secret_inputs" {
  type      = map(string)
  default   = {}
  sensitive = true
  ephemeral = true
}

variable "values" {
  description = "The chart's values, after the installer's own."
  type        = any
  default     = { ingress = { className = "nginx" } }
}

variable "deletion_protection" {
  type    = bool
  default = true
}

variable "registry" {
  description = "A mirror's prefix the cluster's nodes pull from; left out, the release's registry."
  type        = string
  default     = null
}

variable "pull_username" {
  type    = string
  default = null
}

variable "pull_password" {
  type      = string
  default   = null
  sensitive = true
}
