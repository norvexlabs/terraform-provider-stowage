variable "installer" {
  type        = string
  description = "The installer image by digest, <prefix>/<name>-installer@sha256:<64 hex>: both installs run it."
}

variable "pull_username" {
  type    = string
  default = null
}

variable "pull_password" {
  type      = string
  sensitive = true
  default   = null
}

variable "release_public_key" {
  type        = string
  description = "The PEM public key the project signs its releases with."
}

variable "host" {
  type        = string
  description = "The Docker host, reached by ssh; both installs are on it, each in its own work_dir."
}

variable "port" {
  type    = number
  default = 22
}

variable "user" {
  type        = string
  description = "The SSH user, in the docker group."
}

variable "private_key_file" {
  type    = string
  default = null
}

variable "known_hosts_line" {
  type        = string
  description = "The host's known_hosts line (host keytype base64)."
}

variable "work_dirs" {
  type        = object({ a = string, b = string })
  description = "Each install's work directory on the host, which exists."
}

variable "ports" {
  type        = object({ a = map(number), b = map(number) })
  description = "Each install's loopback ports, by route."
}

variable "site_a_admin" {
  type        = string
  description = "Site A's administrator: A's own ADMIN_EMAIL, and what B's kit gives A back."
}

variable "site_b_admin" {
  type        = string
  description = "Site B's administrator, which A's kit gives B at its first install."
}

variable "returned_admin" {
  type        = string
  default     = null
  description = "What B's kit gives A back as ADMIN_EMAIL; left out, site_a_admin. Another value is A's once taken: A's refresh reads it, and the next plan gives site_a_admin back."
}

variable "kit_versions" {
  type        = object({ a_to_b = number, b_to_a = number })
  default     = { a_to_b = 1, b_to_a = 1 }
  description = "Each kit's version: bump the one an apply lost (it failed after issuing the kit, before it was taken) to issue it again."
}

variable "destroyable" {
  type        = bool
  default     = false
  description = "Turns deletion_protection off and delete_data on, for a destroy that removes the installs' data."
}
