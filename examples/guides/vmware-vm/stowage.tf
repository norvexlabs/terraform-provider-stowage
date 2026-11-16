# The Stowage side of the root: the seed (the VM's first-boot material, from the release's
# installer, run on this machine), and the install, kept over SSH once the VM answers.

resource "stowage_vm_seed" "vm" {
  installer      = var.installer
  platform       = "vmware"
  hostname       = var.hostname
  user           = var.user
  ssh_public_key = file(pathexpand("${var.ssh_key_file}.pub"))
  network        = var.addressing
  address        = var.address
  gateway        = var.gateway
  dns            = var.dns
  ssh_allow      = var.ssh_allow
  interfaces     = local.seed_interfaces
  tls            = var.tls
  domains        = var.domains
  inputs         = var.inputs
  # The disk written here as a file (disk_file), which the platform imports.
  disk_dir = pathexpand(var.disk_dir)

  # The first boot's: a later installer or setting would make a new seed (a new one-time key, new
  # MACs, the VM made again). The VM upgrades by stowage_vm_install's version.
  lifecycle {
    ignore_changes = all
  }
}

# The NoCloud seed ISO of its user-data and meta-data, on this machine (seed-iso.sh: xorriso, jq).
data "external" "seed" {
  program = ["${path.module}/seed-iso.sh"]
  query = {
    user_data = stowage_vm_seed.vm.user_data
    meta_data = stowage_vm_seed.vm.meta_data
    hostname  = var.hostname
    dir       = pathexpand(var.seed_dir)
  }
}

locals {
  seed         = stowage_vm_seed.vm
  image        = local.seed.disk_file.path
  image_sha256 = local.seed.disk_file.sha256
  image_format = local.seed.disk_file.format
  seed_iso     = data.external.seed.result.path
  seed_sha256  = data.external.seed.result.sha256

  # The size: what the variables give, else the release's recommended.
  cpus       = coalesce(var.cpus, try(local.seed.size.cpus.recommended, 2))
  memory_mib = coalesce(var.memory_mib, try(local.seed.size.memory_mib.recommended, 4096))
  app_gib    = coalesce(var.app_gib, try(local.seed.size.app_gib.recommended, local.seed.app_gib))
  disk_gib   = local.seed.disk["vmware"].os_gib + local.app_gib

  # The NICs, by nic_roles (known at plan: the seed is made in the same apply), the primary first:
  # each by role, the MAC the seed names it by and the network it attaches to ("": the root's own).
  # With nic_roles empty, one NIC: the seed's primary's MAC, else the platform's (""): a release
  # that declares no NIC (an empty nics), or one role the seed gives no MAC (the VM pins its name to
  # the MAC it finds at the first boot).
  # Each NIC's MAC: nic_macs's for its role, else the seed's, else "" (the platform's). With
  # nic_roles empty, the one NIC's role is the seed's primary's.
  primary_role = try(coalesce(one([for r, n in local.seed.nics : r if n.primary])), "")
  nics = length(var.nic_roles) > 0 ? [for role in var.nic_roles : { role = role, mac = try(coalesce(lookup(var.nic_macs, role, null), try(local.seed.nics[role].mac, null)), ""), attach = try(coalesce(local.seed.nics[role].attach), "") }] : [
    { role = "", mac = try(coalesce(lookup(var.nic_macs, local.primary_role, null), try(one([for n in values(local.seed.nics) : n.mac if n.primary]), null)), ""), attach = "" }
  ]
  # The seed's interfaces: seed_interfaces, each role nic_macs names taking its MAC (mac=, after
  # the role's own spec).
  seed_interfaces = length(var.nic_macs) == 0 ? var.seed_interfaces : merge(var.seed_interfaces == null ? {} : var.seed_interfaces,
  { for role, mac in var.nic_macs : role => try(var.seed_interfaces[role], null) != null ? "${var.seed_interfaces[role]},mac=${mac}" : "mac=${mac}" })
  # Every MAC a NIC takes: the seed's NICs' (the platform's, "", left out: it gives each its own)
  # and those attached after it (interfaces).
  macs = concat([for n in local.nics : lower(n.mac) if n.mac != ""], var.interfaces == null ? [] : [for n in values(var.interfaces) : lower(n.mac)])
}

# The release's least size refused, its recommended one warned of.
resource "terraform_data" "size" {
  input = [local.cpus, local.memory_mib, local.app_gib]
  lifecycle {
    precondition {
      condition     = local.seed.nics == null || alltrue([for n in values(local.seed.nics) : n.mac != null || n.primary])
      error_message = "A secondary NIC the release names has no MAC in stowage_vm_seed.vm.nics: the VM names it by the MAC its seed gives it. Give the role's MAC in nic_macs (role → MAC: the seed's interfaces and the NIC both take it), then apply with -replace=stowage_vm_seed.vm (the seed is made once). An installer made with Stowage before 0.40.0 gives a secondary NIC no MAC of its own: upgrade Stowage and ship the release again, or give nic_macs."
    }
    precondition {
      condition     = alltrue([for r, m in var.nic_macs : try(local.seed.nics[r].mac, null) == null || lower(try(local.seed.nics[r].mac, "")) == lower(m)])
      error_message = "nic_macs gives a role another MAC than stowage_vm_seed.vm.nics names it by: the seed is made once, so a later nic_macs does not reach it. Apply with -replace=stowage_vm_seed.vm (a new seed, the VM made again), or set nic_macs to the seed's MACs."
    }
    precondition {
      condition     = alltrue([for r in keys(var.nic_macs) : length(regexall("(^|,)mac=", try(var.seed_interfaces[r], ""))) == 0])
      error_message = "seed_interfaces gives a role nic_macs names a mac= too: give the role's MAC in nic_macs alone."
    }
    precondition {
      condition     = local.seed.nics == null || length(local.seed.nics) <= 1 || alltrue([for r in keys(local.seed.nics) : contains(var.nic_roles, r)])
      error_message = "The release names more than one NIC (stowage_vm_seed.vm.nics): list each role in nic_roles, the primary first."
    }
    precondition {
      condition     = length(var.nic_roles) == 0 || alltrue([for i, n in local.nics : n.mac != "" || (i == 0 && try(local.seed.nics[n.role].primary, false))])
      error_message = "A role in nic_roles has no MAC (not in nic_macs, and not a role stowage_vm_seed.vm.nics gives a MAC): the VM names each NIC by its MAC. Give the role's MAC in nic_macs, or list only the roles the release names; only the primary, listed first, takes the platform's."
    }
    precondition {
      condition     = length(distinct(local.macs)) == length(local.macs)
      error_message = "Two NICs share a MAC (stowage_vm_seed.vm.nics, interfaces): give each its own."
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
  platform         = "vmware"

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

  depends_on = [vsphere_virtual_machine.vm]
}
