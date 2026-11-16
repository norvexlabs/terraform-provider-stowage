# A Kairos VM of a Stowage release on TrueNAS SCALE (25.04 or later), made by TrueNAS's own provider
# (truenas/truenas) from stowage_vm_seed, then installed and kept by stowage_vm_install.
#
# truenas/truenas declares the VM, its devices, its power, its zvol and a staging dataset. What it
# cannot declare — the disk's bytes into the zvol, the seed's file on the host, the VM stopped
# before its devices are deleted — goes through Stowage's stopgap steps, `<short>-stow vm truenas
# import-image|seed|stop`, run by local-exec with the release's installer (stowage_vm_seed.ctl_path);
# they read the API's endpoint and key from the environment as the provider does, so no credential
# enters the state.

locals {
  base = "${var.pool}/${var.parent_dataset}/${lower(var.name)}"
  seed = stowage_vm_seed.vm

  # A locally administered MAC derived from the name, for the primary NIC alone when neither
  # nic_macs nor the seed gives it one: the one NIC of a release that declares none, or the primary
  # listed first in nic_roles (the VM pins its name to the MAC it finds at the first boot): the same
  # on every apply (a DHCP lease, and the host name a DHCP server registers, follow it). Every other
  # NIC takes the MAC nic_macs gives its role, else the one the seed gives it, kept in state; one
  # without is refused (truenas_vm's preconditions), as are two NICs sharing a MAC.
  mac_hex = sha256("stowage-vm/${var.name}")
  mac = format("02:%s:%s:%s:%s:%s",
    substr(local.mac_hex, 0, 2), substr(local.mac_hex, 2, 2), substr(local.mac_hex, 4, 2),
  substr(local.mac_hex, 6, 2), substr(local.mac_hex, 8, 2))

  # The NICs, by nic_roles (known at plan: the seed is made in the same apply), the primary first,
  # each with the MAC the seed's cloud-config names its role by and its bridge (the seed's attach=,
  # else nic_attach); with nic_roles empty, one NIC, the seed's primary, whatever its role.
  # With nic_roles empty, the one NIC's role is the seed's primary's (nic_macs may name it).
  roles    = length(var.nic_roles) > 0 ? var.nic_roles : [try(coalesce(one([for r, n in local.seed.nics : r if n.primary])), "")]
  seed_nic = { for i, role in local.roles : tostring(i) => try(local.seed.nics[role], null) }
  # Each NIC's MAC: nic_macs's for its role, else the seed's, else (the primary alone) the derived.
  nics = { for k, n in local.seed_nic : k => {
    mac = try(var.nic_macs[local.roles[tonumber(k)]], null) != null ? var.nic_macs[local.roles[tonumber(k)]] : (
    try(n.mac, null) != null ? n.mac : (k == "0" && (length(var.nic_roles) == 0 || try(n.primary, false)) ? local.mac : null))
    attach = coalesce(try(n.attach, null), var.nic_attach)
  } }
  # The seed's interfaces: each role nic_macs names, by that MAC (mac=).
  seed_interfaces = length(var.nic_macs) > 0 ? { for role, mac in var.nic_macs : role => "mac=${mac}" } : null
  # Every MAC a NIC device takes: the seed's NICs' and those added after it.
  macs = concat([for n in values(local.nics) : lower(n.mac) if n.mac != null], [for n in values(var.extra_nics) : lower(n.mac)])

  # The size: what the variables give, else the release's recommended.
  cpus       = coalesce(var.cpus, try(local.seed.size.cpus.recommended, 2))
  memory_mib = coalesce(var.memory_mib, try(local.seed.size.memory_mib.recommended, 4096))
  app_gib    = coalesce(var.app_gib, try(local.seed.size.app_gib.recommended, local.seed.app_gib))
  disk_gib   = local.seed.disk["truenas"].os_gib + local.app_gib

  host = coalesce(var.vm_address, try(split("/", var.address)[0], null))
}

resource "stowage_vm_seed" "vm" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = var.hostname
  user           = var.user
  ssh_public_key = file(pathexpand("${var.ssh_key_file}.pub"))
  network        = var.addressing
  address        = var.address
  gateway        = var.gateway
  dns            = var.dns
  ssh_allow      = var.ssh_allow
  tls            = var.tls
  domains        = var.domains
  inputs         = var.inputs
  interfaces     = local.seed_interfaces
  # The disk written here as a file (disk_file), which `vm truenas import-image` uploads.
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

# Holds the uploads (the disk's staging copy, the seed): TrueNAS's API deletes no file, so they
# live as long as this dataset. force_destroy deletes it with its snapshots.
resource "truenas_dataset" "staging" {
  name          = "${local.base}-staging"
  force_destroy = true
}

# The disk image's digest: a new image is a new disk. It replaces the zvol, the disk device and
# terraform_data.stop, so the VM is stopped before the old device and zvol go, the image is written
# into a fresh zvol, and the device made again after it.
resource "terraform_data" "image_digest" {
  input = local.seed.disk_file.sha256
}

resource "truenas_zvol" "disk" {
  name          = local.base
  volsize       = local.disk_gib * 1024 * 1024 * 1024
  volblocksize  = 16384
  compression   = "lz4"
  force_destroy = true
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }
}

# The disk's contents, written into each new zvol (import-image marks the zvol with the image it
# holds, so a re-run with the same image changes nothing).
resource "terraform_data" "image" {
  triggers_replace = [truenas_zvol.disk.id, local.seed.disk_file.sha256]

  provisioner "local-exec" {
    command = "exec \"$CTL\" vm truenas import-image"
    environment = {
      CTL                          = local.seed.ctl_path
      IMAGE                        = local.seed.disk_file.path
      IMAGE_SHA256                 = local.seed.disk_file.sha256
      STAGING_DIR                  = truenas_dataset.staging.mountpoint
      ZVOL                         = truenas_zvol.disk.id
      TRUENAS_INSECURE_SKIP_VERIFY = var.insecure ? "1" : "0"
    }
  }
}

# The seed's file on the host, named by its digest: a new seed is a new file.
resource "terraform_data" "seed" {
  triggers_replace = [truenas_dataset.staging.id, data.external.seed.result.sha256]
  input            = "${truenas_dataset.staging.mountpoint}/seed-${substr(data.external.seed.result.sha256, 0, 16)}.iso"

  provisioner "local-exec" {
    command = "exec \"$CTL\" vm truenas seed"
    environment = {
      CTL                          = local.seed.ctl_path
      SEED                         = data.external.seed.result.path
      SEED_PATH                    = self.input
      TRUENAS_INSECURE_SKIP_VERIFY = var.insecure ? "1" : "0"
    }
  }
}

resource "truenas_vm" "vm" {
  name             = var.name
  vcpus            = 1
  cores            = local.cpus
  threads          = 1
  memory           = local.memory_mib
  bootloader       = "UEFI"
  cpu_mode         = "HOST-PASSTHROUGH"
  time             = "UTC"
  autostart        = true
  shutdown_timeout = 90

  lifecycle {
    precondition {
      condition     = local.cpus >= try(local.seed.size.cpus.min, 1) && local.memory_mib >= try(local.seed.size.memory_mib.min, 1024) && local.app_gib >= try(local.seed.size.app_gib.min, 4)
      error_message = "The VM is under the release's least size (stowage_vm_seed.vm.size: cpus, memory_mib, app_gib min): give more."
    }
    precondition {
      condition     = local.seed.nics == null || alltrue([for n in values(local.seed.nics) : n.mac != null || n.primary])
      error_message = "A secondary NIC the release names has no MAC in stowage_vm_seed.vm.nics: the VM names it by the MAC its seed gives it. Give the role's MAC in nic_macs (role → MAC: the seed's interfaces and the NIC both take it), then apply with -replace=stowage_vm_seed.vm (the seed is made once). An installer made with Stowage before 0.40.0 gives a secondary NIC no MAC of its own: upgrade Stowage and ship the release again, or give nic_macs."
    }
    precondition {
      condition     = alltrue([for r, m in var.nic_macs : try(local.seed.nics[r].mac, null) == null || lower(try(local.seed.nics[r].mac, "")) == lower(m)])
      error_message = "nic_macs gives a role another MAC than stowage_vm_seed.vm.nics names it by: the seed is made once, so a later nic_macs does not reach it. Apply with -replace=stowage_vm_seed.vm (a new seed, the VM made again), or set nic_macs to the seed's MACs."
    }
    precondition {
      condition     = local.seed.nics == null || length(local.seed.nics) <= 1 || alltrue([for r in keys(local.seed.nics) : contains(var.nic_roles, r)])
      error_message = "The release names more than one NIC (stowage_vm_seed.vm.nics): list each role in nic_roles, the primary first."
    }
    precondition {
      condition     = alltrue([for n in values(local.nics) : n.mac != null])
      error_message = "A role in nic_roles has no MAC (not in nic_macs, and not a role stowage_vm_seed.vm.nics gives a MAC): the VM names each NIC by its MAC. Give the role's MAC in nic_macs, or list only the roles the release names; only the primary, listed first, takes the MAC derived from name."
    }
    precondition {
      condition     = length(distinct(local.macs)) == length(local.macs)
      error_message = "Two NICs share a MAC (stowage_vm_seed.vm.nics, extra_nics): give each its own."
    }
  }
}

# Under the release's recommended size: a warning, never a refusal.
check "recommended_size" {
  assert {
    condition     = local.cpus >= try(local.seed.size.cpus.recommended, 0) && local.memory_mib >= try(local.seed.size.memory_mib.recommended, 0) && local.app_gib >= try(local.seed.size.app_gib.recommended, 0)
    error_message = "The VM is under the release's recommended size (stowage_vm_seed.vm.size)."
  }
}

# Attached once the image is written: import-image refuses a zvol a VM device uses. Replaced with
# the zvol on a new image.
resource "truenas_vm_device" "disk" {
  vm    = truenas_vm.vm.id
  order = 1001
  attributes = jsonencode({
    dtype  = "DISK"
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  })
  depends_on = [terraform_data.image]
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }
}

# The seed's NICs, the primary first.
resource "truenas_vm_device" "nic" {
  for_each = local.nics
  vm       = truenas_vm.vm.id
  order    = 1002 + tonumber(each.key)
  attributes = jsonencode({
    dtype      = "NIC"
    type       = "VIRTIO"
    nic_attach = each.value.attach
    mac        = each.value.mac
  })
}

# The NICs added after the seed, by role: the VM restarts to take them (truenas_vm_power), then
# stowage_vm_install names and addresses each (`network attach`) and reboots it.
resource "truenas_vm_device" "extra_nic" {
  for_each = var.extra_nics
  vm       = truenas_vm.vm.id
  order    = 1100 + index(sort(keys(var.extra_nics)), each.key)
  attributes = jsonencode({
    dtype      = "NIC"
    type       = "VIRTIO"
    nic_attach = each.value.attach != "" ? each.value.attach : var.nic_attach
    mac        = each.value.mac
  })
}

resource "truenas_vm_device" "seed" {
  vm    = truenas_vm.vm.id
  order = 9999
  attributes = jsonencode({
    dtype = "CDROM"
    path  = terraform_data.seed.output
  })
}

# Destroyed before the devices, which it depends on: TrueNAS refuses to delete a running VM's
# device, and truenas_vm_power's delete leaves the VM running.
resource "terraform_data" "stop" {
  input = {
    vm_id    = tostring(truenas_vm.vm.id)
    ctl      = local.seed.ctl_path
    insecure = var.insecure ? "1" : "0"
  }
  depends_on = [truenas_vm_device.disk, truenas_vm_device.nic, truenas_vm_device.extra_nic, truenas_vm_device.seed]
  lifecycle {
    replace_triggered_by = [terraform_data.image_digest]
  }

  # The installer is kept in the provider's cache on the machine that applied, which keeps the
  # last eight used: a destroy refreshes stowage_vm_seed first, which fetches it again at that path.
  # Without that refresh (-refresh=false), or from another machine, it may not be there: refused,
  # naming the fix, before TrueNAS is asked anything.
  provisioner "local-exec" {
    when    = destroy
    command = <<-EOT
      if [ ! -x "$CTL" ]; then
        echo "The installer $CTL is not on this machine: run 'tofu apply -refresh-only' (stowage_vm_seed's refresh fetches it again into this machine's cache) — from another machine than the one that applied, 'tofu apply', which records this machine's path here — then destroy again." >&2
        exit 1
      fi
      exec "$CTL" vm truenas stop
    EOT
    environment = {
      CTL                          = self.input.ctl
      VM_ID                        = self.input.vm_id
      TRUENAS_INSECURE_SKIP_VERIFY = self.input.insecure
    }
  }
}

# Running, and restarted when what the VM reads at boot changes: a NIC added (TrueNAS applies a
# device at a start), the disk written again, the seed, the size.
resource "truenas_vm_power" "vm" {
  vm_id   = truenas_vm.vm.id
  running = true
  restart_triggers = {
    image = terraform_data.image.id
    shape = jsonencode([truenas_vm.vm.cores, truenas_vm.vm.memory])
    disk  = truenas_vm_device.disk.attributes
    nics  = jsonencode(concat([for k, d in truenas_vm_device.nic : d.attributes], [for k, d in truenas_vm_device.extra_nic : d.attributes]))
    seed  = truenas_vm_device.seed.attributes
  }
  depends_on = [terraform_data.stop]
}

# The install: its first boot awaited under the seed's one-time key, the secret inputs given over
# SSH, the VM's own key read and the one-time key retired; later, upgrades, rollbacks, re-runs and
# the NICs added after the seed.
resource "stowage_vm_install" "vm" {
  host             = local.host
  user             = var.user
  private_key_file = pathexpand(var.ssh_key_file)
  known_hosts_line = var.vm_known_hosts_line != null ? var.vm_known_hosts_line : stowage_vm_seed.vm.known_hosts_line
  short_name       = stowage_vm_seed.vm.short_name
  version          = coalesce(var.release_version, stowage_vm_seed.vm.version)
  platform         = "truenas"

  inputs                   = var.inputs
  secret_inputs_wo         = var.secret_inputs
  secret_inputs_wo_version = var.secret_inputs_version
  tls                      = var.tls
  domains                  = var.domains
  interfaces               = { for role, n in var.extra_nics : role => { mac = n.mac, mode = n.mode, address = n.address } }

  allow_rollback      = var.allow_rollback
  plan_dry_run        = var.plan_dry_run
  deletion_protection = var.deletion_protection

  timeouts {
    create = "40m"
    update = "40m"
  }

  # After the VM runs (and restarted to take a NIC added): the power resource, which follows every
  # device.
  depends_on = [truenas_vm_power.vm]
}
