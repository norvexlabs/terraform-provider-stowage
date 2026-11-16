variable "name" {
  description = "The VM's name on the platform."
  type        = string
}

variable "description" {
  description = "The domain's description; null for none (libvirt reads an empty one back as none, which an apply would fail on as an inconsistent result)."
  type        = string
  default     = null
}

variable "uri" {
  description = "The libvirt connection, e.g. qemu+ssh://root@kvm.example.org/system (LIBVIRT_DEFAULT_URI is not read)."
  type        = string
  default     = "qemu:///system"
}

variable "pool" {
  description = "The storage pool the disk and the seed go in."
  type        = string
  default     = "default"
}

variable "network" {
  description = "The libvirt network the VM attaches to (its DHCP gives the address)."
  type        = string
  default     = "default"
}

variable "domain_type" {
  description = "The domain's type: kvm (the default), or qemu for a host without KVM (emulated, much slower: its CPU mode is then maximum)."
  type        = string
  default     = "kvm"
  validation {
    condition     = contains(["kvm", "qemu"], var.domain_type)
    error_message = "domain_type is kvm or qemu."
  }
}

# The Stowage side's variables: the release, the VM's first-boot settings, the install.

variable "installer" {
  description = "The installer image by digest, as `stowage ship` prints it: its release is the VM's first."
  type        = string
}

variable "hostname" {
  description = "The VM's host name (the seed's --hostname)."
  type        = string
}

variable "vm_address" {
  description = "The address SSH reaches the VM at, when the platform does not report one (a DHCP server's name for it); left out, the platform's, else the static address."
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
  description = "The app's space on the disk in GiB, added to what Kairos takes; left out, the release's recommended."
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
  description = "The sources SSH accepts (CIDR); left out, every source (a cloud's default: the public address of this machine)."
  type        = list(string)
  default     = null
}

variable "nic_macs" {
  description = "MACs by role (the release's host.interfaces): each given to the seed (its interfaces, mac=, after seed_interfaces' spec for the role, which then gives none) and to that role's NIC. A role left out takes the MAC the seed gives it; the primary, given none, the platform's. The seed is made once: a change after it is refused until -replace=stowage_vm_seed.vm makes it again."
  type        = map(string)
  default     = {}
  validation {
    condition     = alltrue([for m in values(var.nic_macs) : can(regex("^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$", m))])
    error_message = "nic_macs: each value a MAC, e.g. 02:00:00:00:00:01."
  }
}

variable "seed_interfaces" {
  description = "NICs the release names, role → spec (the seed's --interface ROLE=SPEC: mac=…, attach=…)."
  type        = map(string)
  default     = null
}

variable "interfaces" {
  description = "NICs of the release's secondary roles attached after the seed, by role (stowage_vm_install's interfaces): the platform's device comes first."
  type = map(object({
    mac     = string
    mode    = optional(string)
    address = optional(string)
  }))
  default = null
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

variable "disk_dir" {
  description = "The directory on this machine the release's disk is written into (the seed's --disk-dir), on a hypervisor."
  type        = string
  default     = "~/.cache/stowage-vm/disks"
}

variable "seed_dir" {
  description = "The directory on this machine the seed ISO is written into (0700: it holds the one-time host key), on a hypervisor."
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

variable "nic_roles" {
  description = "The NICs' roles (the release's host.interfaces, stowage_vm_seed's nics), the primary first: known at plan, as the seed is made in the same apply. Empty: one NIC, the seed's primary."
  type        = list(string)
  default     = []
}
