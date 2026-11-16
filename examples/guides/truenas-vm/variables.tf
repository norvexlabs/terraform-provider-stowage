variable "installer" {
  description = "The installer image by digest, as `stowage ship` prints it: its release is the VM's first."
  type        = string
}

variable "name" {
  description = "The VM's name on TrueNAS: letters and digits; its zvol and staging dataset are named after it."
  type        = string
  validation {
    condition     = can(regex("^[A-Za-z0-9]{1,50}$", var.name))
    error_message = "name: letters and digits, at most 50."
  }
}

variable "hostname" {
  description = "The VM's host name (the seed's --hostname), which a DHCP server may register."
  type        = string
}

variable "vm_address" {
  description = "The address SSH reaches the VM at: the name its DHCP server registers (<hostname>.<domain>), or its static address. Left out with addressing = \"static\", the static address."
  type        = string
  default     = null
}

variable "user" {
  description = "The VM's administrator, who logs in over SSH."
  type        = string
  default     = "ops"
}

variable "ssh_key_file" {
  description = "The administrator's SSH private key on this machine; its public key is <file>.pub."
  type        = string
  default     = "~/.ssh/id_ed25519"
}

variable "insecure" {
  description = "Accept the TrueNAS host's self-signed certificate (the provider's, and TRUENAS_INSECURE_SKIP_VERIFY for the Stowage steps)."
  type        = bool
  default     = false
}

variable "pool" {
  type = string
}

variable "parent_dataset" {
  description = "The dataset under the pool the VM's zvol and staging dataset go in."
  type        = string
}

variable "nic_attach" {
  description = "The bridge a NIC that names none attaches to."
  type        = string
}

variable "addressing" {
  description = "How the VM takes its address: dhcp, or static with address, gateway and dns."
  type        = string
  default     = "dhcp"
  validation {
    condition     = contains(["dhcp", "static"], var.addressing)
    error_message = "addressing is dhcp or static."
  }
}

variable "address" {
  description = "With addressing = \"static\": the address with its prefix, e.g. 192.0.2.10/24."
  type        = string
  default     = null
}

variable "gateway" {
  type    = string
  default = null
}

variable "dns" {
  type    = list(string)
  default = null
}

variable "cpus" {
  description = "The VM's CPUs; left out, the release's recommended (the seed's size.cpus)."
  type        = number
  default     = null
}

variable "memory_mib" {
  description = "The VM's memory in MiB; left out, the release's recommended."
  type        = number
  default     = null
}

variable "app_gib" {
  description = "The app's space on the disk in GiB, which the zvol adds to what Kairos takes; left out, the release's recommended."
  type        = number
  default     = null
}

variable "tls" {
  description = "The front's TLS mode: acme, dns-cloudflare, internal or byo."
  type        = string
  default     = "internal"
}

variable "domains" {
  description = "Route → domain."
  type        = map(string)
  default     = {}
}

variable "inputs" {
  description = "Non-secret inputs: in the seed and in state."
  type        = map(string)
  default     = {}
}

variable "secret_inputs" {
  description = "Secret inputs, given over SSH once the VM answers: never in the seed or in state."
  type        = map(string)
  default     = {}
  sensitive   = true
  ephemeral   = true
}

variable "secret_inputs_version" {
  description = "Bump it to give secret_inputs again."
  type        = number
  default     = 1
}

variable "ssh_allow" {
  description = "The sources SSH accepts (CIDR); left out, every source."
  type        = list(string)
  default     = null
}

variable "release_version" {
  description = "The release the VM runs; left out, the seed's (its first). A later one upgrades it."
  type        = string
  default     = null
}

variable "allow_rollback" {
  type    = bool
  default = false
}

variable "plan_dry_run" {
  type    = bool
  default = false
}

variable "deletion_protection" {
  description = "While true, destroying the install is refused: set it false and apply before tofu destroy."
  type        = bool
  default     = true
}

variable "vm_known_hosts_line" {
  description = "The VM's own key as a known_hosts line, for an imported install's first apply; left out, the seed's one-time key."
  type        = string
  default     = null
}

variable "extra_nics" {
  description = "NICs of the release's secondary roles added after the seed, by role: the MAC TrueNAS gives the device, its bridge (empty: nic_attach), and its addressing (mode dhcp, static or none; address with static)."
  type = map(object({
    mac     = string
    attach  = optional(string, "")
    mode    = optional(string)
    address = optional(string)
  }))
  default = {}
}

variable "disk_dir" {
  description = "The directory on this machine the release's disk is written into (the seed's --disk-dir)."
  type        = string
  default     = "~/.cache/stowage-vm/disks"
}

variable "seed_dir" {
  description = "The directory on this machine the seed ISO is written into (0700: it holds the one-time host key)."
  type        = string
  default     = "~/.cache/stowage-vm/seeds"
}

variable "registry" {
  description = "A mirror's prefix the VM pulls from (the provider's registry); left out, the release's registry."
  type        = string
  default     = null
}

variable "registry_ca_file" {
  description = "PEM of the CA the mirror's certificate chains to, on this machine."
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

variable "nic_macs" {
  description = "MACs by role (the release's host.interfaces): each given to the seed (its interfaces, mac=) and to that role's NIC device. A role left out takes the MAC the seed gives it; the primary, given none, the MAC derived from name. The seed is made once: a change after it is refused until -replace=stowage_vm_seed.vm makes it again."
  type        = map(string)
  default     = {}
  validation {
    condition     = alltrue([for m in values(var.nic_macs) : can(regex("^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$", m))])
    error_message = "nic_macs: each value a MAC, e.g. 02:00:00:00:00:01."
  }
}

variable "nic_roles" {
  description = "The NICs' roles (the release's host.interfaces, stowage_vm_seed's nics), the primary first: known at plan, as the seed is made in the same apply. Empty: one NIC, the seed's primary."
  type        = list(string)
  default     = []
}
