# Quick start: a Kairos VM. stowage_vm_seed makes its first-boot material once; your hypervisor's
# or cloud's own resource makes the VM from user_data and the disk (examples/resources/
# stowage_vm_seed has a TrueNAS one); stowage_install then installs and keeps it over SSH.
# `make install-local` in the provider's checkout first, then `tofu init`.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "0.1.0"
    }
  }
}

provider "stowage" {}

resource "stowage_vm_seed" "vm" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = "app"
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  tls            = "internal"
  inputs         = { ADMIN_EMAIL = var.admin_email }
}

# The VM, made by your hypervisor's resource from these, answers at var.address.
output "user_data" {
  value     = stowage_vm_seed.vm.user_data
  sensitive = true
}

output "disk" {
  value = stowage_vm_seed.vm.disk["truenas"]
}

resource "stowage_install" "vm" {
  inputs = { ADMIN_EMAIL = var.admin_email }
  tls    = "internal"

  vm {
    host             = var.address
    user             = "ops"
    private_key_file = pathexpand("~/.ssh/id_ed25519")
    known_hosts_line = stowage_vm_seed.vm.known_hosts_line # the one-time key, until the VM's own is read
    short_name       = stowage_vm_seed.vm.short_name
    version          = stowage_vm_seed.vm.version # a later release upgrades the VM
  }
}
