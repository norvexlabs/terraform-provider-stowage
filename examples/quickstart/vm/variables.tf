variable "installer" {
  type        = string
  description = "The installer image by digest, <prefix>/<name>-installer@sha256:<64 hex>."
}

variable "address" {
  type        = string
  description = "The VM's address, once your hypervisor's resource made it."
}

variable "admin_email" {
  type = string
}
