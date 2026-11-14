# A Kairos VM on TrueNAS SCALE made with the upstream TrueNAS provider alone, then installed and
# kept by stowage_install. The TrueNAS provider declares the datasets, the zvol, the VM and its
# devices; truenas-step.sh, run by local-exec over SSH to TrueNAS's shell (`midclt`), does what it
# has no resource for: the disk image's upload and conversion into the zvol, the NoCloud seed ISO,
# the power, and the snapshots that block a delete. No credential of TrueNAS's API enters a
# command line or the state: the provider reads TRUENAS_API_KEY from the environment.
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    truenas = {
      source  = "pjsalty/truenas"
      version = "3.4.0"
    }
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.2"
    }
  }
}

provider "truenas" {
  url = var.truenas_url # https://<truenas>; the API key from TRUENAS_API_KEY
}

provider "stowage" {}

locals {
  name    = "notes"
  host    = split("/", var.address)[0]
  step    = "${path.module}/truenas-step.sh"
  staging = "${var.pool}/${var.parent_dataset}/${local.name}-staging"
  ssh_env = {
    TNAS_SSH_HOST = var.truenas_ssh_host
    TNAS_SSH_USER = var.truenas_ssh_user
    TNAS_SSH_PORT = "22"
    TNAS_SSH_KEY  = pathexpand(var.truenas_ssh_key_file)
  }
  # The seed ISO's path on TrueNAS, named by the seed: a new seed is a new file.
  seed_path = "/mnt/${local.staging}/seed-${substr(sha256(stowage_vm_seed.notes.id), 0, 16)}.iso"
  # The VM's NICs, by var.nic_roles (known at plan; one NIC when it is empty), each with the MAC
  # the seed's cloud-config names its role by (null for a VM of one NIC: TrueNAS gives it one) and
  # its bridge (the seed's attach=, else the primary's, else var.bridge).
  # With nic_roles empty, the one NIC is the seed's primary, whatever its role.
  seed_nic = { for i, role in(length(var.nic_roles) > 0 ? var.nic_roles : ["-"]) : tostring(i) => (
    length(var.nic_roles) > 0 ? try(stowage_vm_seed.notes.nics[role], null) : try(one([for n in values(stowage_vm_seed.notes.nics) : n if n.primary]), null)
  ) }
  primary_attach = coalesce(try(local.seed_nic["0"].attach, null), var.bridge)
  nics = { for k, n in local.seed_nic : k => {
    mac    = try(n.mac, null)
    attach = coalesce(try(n.attach, null), local.primary_attach)
  } }
}

resource "stowage_vm_seed" "notes" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = local.name
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  # A static address: TrueNAS does not report the VM's.
  network = "static"
  address = var.address
  gateway = var.gateway
  dns     = [var.gateway]
  tls     = "internal"
  inputs  = { ADMIN_EMAIL = var.admin_email }
  # The disk written here as a file (disk_file), for truenas-step.sh to upload.
  disk_dir = pathexpand("~/.cache/notes/disks")

  # The first boot's: a later installer or setting would make a new seed (new MACs, a new seed ISO,
  # the VM powered off and on). The VM upgrades by stowage_install's vm.version (var.release_version).
  lifecycle {
    ignore_changes = all
  }
}

# Holds the uploads (the disk image on its way into the zvol, the seed ISO).
resource "truenas_dataset" "staging" {
  pool           = var.pool
  parent_dataset = var.parent_dataset
  name           = "${local.name}-staging"
}

# The VM's disk, the size the seed asks for (the app's space and what Kairos takes).
resource "truenas_zvol" "disk" {
  pool         = var.pool
  name         = "${var.parent_dataset}/${local.name}"
  volsize      = stowage_vm_seed.notes.disk_file.disk_gib * 1024 * 1024 * 1024
  volblocksize = "16K"
  compression  = "LZ4"
}

# Destroyed before the zvol and the staging dataset: drops their snapshots first (a periodic
# snapshot task may have taken some; TrueNAS refuses to delete a dataset that has any).
resource "terraform_data" "snapshots" {
  input = merge(local.ssh_env, { DATASETS = join(" ", [truenas_zvol.disk.id, truenas_dataset.staging.id]) })

  provisioner "local-exec" {
    when        = destroy
    command     = "\"$STEP\" drop-snapshots"
    environment = merge(self.input, { STEP = "${path.module}/truenas-step.sh" })
  }
}

# The disk image written into the new zvol once: the VM upgrades itself on its own disk, so a later
# seed's image is ignored, and the step refuses a zvol that already holds data.
resource "terraform_data" "image" {
  triggers_replace = [truenas_zvol.disk.id]
  input            = { path = stowage_vm_seed.notes.disk_file.path, sha256 = stowage_vm_seed.notes.disk_file.sha256 }

  lifecycle {
    ignore_changes = [input]
  }

  provisioner "local-exec" {
    command = "\"${local.step}\" import-image"
    environment = merge(local.ssh_env, {
      IMAGE        = self.input.path
      IMAGE_SHA256 = self.input.sha256
      STAGING_DIR  = "/mnt/${truenas_dataset.staging.id}"
      ZVOL         = truenas_zvol.disk.id
    })
  }
}

# The NoCloud seed ISO (volume cidata) of the seed's user-data and meta-data. user_data holds the
# one-time host key's private key: it goes in the step's environment, never an argument.
resource "terraform_data" "seed" {
  triggers_replace = [truenas_dataset.staging.id, stowage_vm_seed.notes.id]
  input            = local.seed_path

  provisioner "local-exec" {
    command = "\"${local.step}\" seed"
    environment = merge(local.ssh_env, {
      SEED_USER_DATA = stowage_vm_seed.notes.user_data
      SEED_META_DATA = stowage_vm_seed.notes.meta_data
      WORK_DIR       = pathexpand("~/.cache/notes/seeds")
      DEST           = self.input
    })
  }
}

resource "truenas_vm" "notes" {
  lifecycle {
    # Every NIC the seed's cloud-config names by MAC needs its device: var.nic_roles lists them.
    precondition {
      condition = (try(length(stowage_vm_seed.notes.nics), 0) <= 1 && length(var.nic_roles) <= 1) || try(
        toset(keys(stowage_vm_seed.notes.nics)) == toset(var.nic_roles) && stowage_vm_seed.notes.nics[var.nic_roles[0]].primary, false
      )
      error_message = "nic_roles must list the roles of stowage_vm_seed.notes.nics, the primary first, when the release names more than one NIC (else leave it empty)."
    }
  }

  name             = local.name
  vcpus            = 1
  cores            = 2
  threads          = 1
  memory           = 4096
  bootloader       = "UEFI"
  cpu_mode         = "HOST-PASSTHROUGH"
  time             = "UTC"
  autostart        = true
  shutdown_timeout = 90
}

resource "truenas_vm_device" "disk" {
  vm    = tonumber(truenas_vm.notes.id)
  dtype = "DISK"
  order = 1001
  attributes = {
    path   = "/dev/zvol/${truenas_zvol.disk.id}"
    type   = "VIRTIO"
    iotype = "THREADS"
  }
}

resource "truenas_vm_device" "nic" {
  for_each = local.nics
  vm       = tonumber(truenas_vm.notes.id)
  dtype    = "NIC"
  order    = 1002 + tonumber(each.key)
  attributes = {
    type       = "VIRTIO"
    nic_attach = each.value.attach
    mac        = each.value.mac
  }
}

resource "truenas_vm_device" "seed" {
  vm    = tonumber(truenas_vm.notes.id)
  dtype = "CDROM"
  order = 9999
  attributes = {
    path = terraform_data.seed.output
  }
}

# Starts the VM unless it runs. TrueNAS applies a changed definition at the VM's next start only,
# so a change of the VM or of a device replaces this resource, which powers the VM off, then on.
resource "terraform_data" "power" {
  triggers_replace = [
    terraform_data.image.id,
    truenas_vm.notes.id,
    [truenas_vm.notes.cores, truenas_vm.notes.memory],
    truenas_vm_device.disk.attributes,
    [for d in truenas_vm_device.nic : d.attributes],
    truenas_vm_device.seed.attributes,
  ]
  input = merge(local.ssh_env, { VM_ID = truenas_vm.notes.id })

  provisioner "local-exec" {
    command     = "\"${local.step}\" start"
    environment = self.input
  }

  # Destroyed before the devices it depends on: TrueNAS refuses to delete a running VM's device.
  provisioner "local-exec" {
    when        = destroy
    command     = "\"$STEP\" stop"
    environment = merge(self.input, { STEP = "${path.module}/truenas-step.sh" })
  }
}

# The install, over SSH once the VM's first boot answers.
resource "stowage_install" "notes" {
  inputs = { ADMIN_EMAIL = var.admin_email }
  tls    = "internal"

  vm {
    host             = local.host
    user             = "ops"
    private_key_file = pathexpand("~/.ssh/id_ed25519")
    known_hosts_line = stowage_vm_seed.notes.known_hosts_line # the one-time key, until the VM's own is read
    short_name       = stowage_vm_seed.notes.short_name
    version          = coalesce(var.release_version, stowage_vm_seed.notes.version) # a later release upgrades the VM
  }

  # deletion_protection = false   # a destroy then forgets the install; the resources above remove the VM

  depends_on = [terraform_data.power]
}

variable "installer" {
  type        = string
  description = "The release's installer by digest, as `stowage ship` prints it: the release the seed's disk boots first."
}

variable "release_version" {
  type        = string
  description = "The release the VM runs; null: the seed's. A later one upgrades the VM, the previous one rolls it back with allow_rollback."
  default     = null
}

variable "nic_roles" {
  type        = list(string)
  description = "The roles of the NICs the release names (host.interfaces), the primary first, when there are more than one; empty: one NIC."
  default     = []
}

variable "admin_email" {
  type = string
}

variable "truenas_url" {
  type        = string
  description = "TrueNAS's URL, https://<host>."
}

variable "truenas_ssh_host" {
  type        = string
  description = "TrueNAS's SSH host, its host key already in ~/.ssh/known_hosts."
}

variable "truenas_ssh_user" {
  type        = string
  description = "TrueNAS's SSH user, whose shell runs midclt (the middleware's client) as an administrator."
  default     = "root"
}

variable "truenas_ssh_key_file" {
  type    = string
  default = "~/.ssh/id_ed25519"
}

variable "pool" {
  type    = string
  default = "tank"
}

variable "parent_dataset" {
  type        = string
  description = "An existing dataset of pool the VM's zvol and staging dataset go under."
  default     = "vms"
}

variable "bridge" {
  type        = string
  description = "The bridge the VM's NICs attach to (a NIC's attach= in the seed's interfaces wins)."
  default     = "br0"
}

variable "address" {
  type        = string
  description = "The VM's static address with its prefix, e.g. 192.0.2.10/24."
}

variable "gateway" {
  type = string
}
