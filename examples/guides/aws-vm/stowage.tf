# The Stowage side of the root: the seed (the VM's first-boot material, from the release's
# installer, run on this machine), and the install, kept over SSH once the VM answers.

resource "stowage_vm_seed" "vm" {
  installer      = var.installer
  platform       = "aws"
  hostname       = var.hostname
  user           = var.user
  ssh_public_key = file(pathexpand("${var.ssh_key_file}.pub"))
  network        = var.addressing
  address        = var.address
  gateway        = var.gateway
  dns            = var.dns
  ssh_allow      = var.ssh_allow
  interfaces     = var.seed_interfaces
  tls            = var.tls
  domains        = var.domains
  inputs         = var.inputs

  # The first boot's: a later installer or setting would make a new seed (a new one-time key, new
  # MACs, the VM made again). The VM upgrades by stowage_vm_install's version.
  lifecycle {
    ignore_changes = all
  }
}

locals {
  seed = stowage_vm_seed.vm

  # The size: what the variables give, else the release's recommended.
  cpus       = coalesce(var.cpus, try(local.seed.size.cpus.recommended, 2))
  memory_mib = coalesce(var.memory_mib, try(local.seed.size.memory_mib.recommended, 4096))
  app_gib    = coalesce(var.app_gib, try(local.seed.size.app_gib.recommended, local.seed.app_gib))
  disk_gib   = local.seed.disk["aws"].os_gib + local.app_gib

  # The NICs, by nic_roles (known at plan: the seed is made in the same apply), the primary first:
  # each by role, the MAC the seed names it by ("": the platform's) and the network it attaches to
  # ("": the root's own). Empty: the platform's one NIC.
  nics = [for role in var.nic_roles : { role = role, mac = coalesce(try(local.seed.nics[role].mac, null), ""), attach = coalesce(try(local.seed.nics[role].attach, null), "") }]
}

# The release's least size refused, its recommended one warned of.
resource "terraform_data" "size" {
  input = [local.cpus, local.memory_mib, local.app_gib]
  lifecycle {
    precondition {
      condition     = local.seed.nics == null || length(local.seed.nics) <= 1 || alltrue([for r in keys(local.seed.nics) : contains(var.nic_roles, r)])
      error_message = "The release names more than one NIC (stowage_vm_seed.vm.nics): list each role in nic_roles, the primary first."
    }
    precondition {
      condition     = local.cpus >= try(local.seed.size.cpus.min, 1) && local.memory_mib >= try(local.seed.size.memory_mib.min, 1024) && local.app_gib >= try(local.seed.size.app_gib.min, 4)
      error_message = "The VM is under the release's least size (stowage_vm_seed.vm.size: cpus, memory_mib, app_gib min): give more."
    }
  }
}

check "recommended_size" {
  assert {
    condition     = local.cpus >= try(local.seed.size.cpus.recommended, 0) && local.memory_mib >= try(local.seed.size.memory_mib.recommended, 0) && local.app_gib >= try(local.seed.size.app_gib.recommended, 0)
    error_message = "The VM is under the release's recommended size (stowage_vm_seed.vm.size)."
  }
}

resource "stowage_vm_install" "vm" {
  host             = local.address
  user             = var.user
  private_key_file = pathexpand(var.ssh_key_file)
  known_hosts_line = var.vm_known_hosts_line != null ? var.vm_known_hosts_line : stowage_vm_seed.vm.known_hosts_line
  short_name       = stowage_vm_seed.vm.short_name
  version          = coalesce(var.release_version, stowage_vm_seed.vm.version)
  platform         = "aws"

  inputs                   = var.inputs
  secret_inputs_wo         = var.secret_inputs
  secret_inputs_wo_version = var.secret_inputs_version
  tls                      = var.tls
  domains                  = var.domains
  interfaces               = var.interfaces

  allow_rollback      = var.allow_rollback
  plan_dry_run        = var.plan_dry_run
  deletion_protection = var.deletion_protection

  timeouts {
    create = "40m"
    update = "40m"
  }

  depends_on = [aws_instance.vm]
}
