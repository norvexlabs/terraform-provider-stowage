variable "installer" {
  description = "The installer image by digest, as `stowage ship` prints it."
  type        = string
}

variable "name" {
  description = "The instance's name."
  type        = string
  default     = "notes"
}

variable "region" {
  type = string
}

variable "subnet_id" {
  description = "A public subnet the instance gets an address in."
  type        = string
}

variable "instance_type" {
  type    = string
  default = "t3.medium"
}

variable "disk_gib" {
  type    = number
  default = 40
}

variable "ssh_key_file" {
  description = "The administrator's SSH private key on this machine; its public key is <file>.pub."
  type        = string
  default     = "~/.ssh/id_ed25519"
}

variable "ssh_sources" {
  description = "The sources SSH accepts (CIDR): this machine's address, for one."
  type        = list(string)
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

variable "tls" {
  type    = string
  default = "acme"
}

variable "domains" {
  description = "Route → domain: each a DNS name of the instance's address."
  type        = map(string)
  default     = {}
}

variable "deletion_protection" {
  type    = bool
  default = true
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
