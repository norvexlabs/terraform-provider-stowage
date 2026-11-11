# A Kairos VM on TrueNAS, made from a stowage_vm_seed, then installed and kept by stowage_install.
#
# The VM itself is Stowage's TrueNAS module (the one `vm create` applies), from a Stowage release:
# its steps upload the disk and the seed through TrueNAS's API, run by `<short>-stow vm truenas`,
# which STOWAGE_CTL names (`docker run --rm <installer> > notes.sh && chmod +x notes.sh`, then
# `export STOWAGE_CTL=$PWD/notes.sh`), with TRUENAS_URL and TRUENAS_API_KEY in the environment.

resource "stowage_vm_seed" "notes" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = "notes"
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  # A static address: TrueNAS does not report the VM's.
  network = "static"
  address = "192.0.2.10/24"
  gateway = "192.0.2.1"
  dns     = ["192.0.2.1"]
  tls     = "internal"
  inputs  = { ADMIN_EMAIL = "ops@example.com" }
  # The disk written here as a file (disk_file), for the module to upload.
  disk_dir = pathexpand("~/.cache/notes/disks")
}

# The NoCloud seed ISO of its user-data and meta-data.
data "external" "seed" {
  program = ["${path.module}/seed-iso.sh"]
  query = {
    user_data = stowage_vm_seed.notes.user_data
    meta_data = stowage_vm_seed.notes.meta_data
    hostname  = "notes"
    dir       = pathexpand("~/.cache/notes/seeds")
  }
}

module "vm" {
  source = "git::https://github.com/norvexlabs/stowage.git//internal/install/tofu/truenas?ref=v0.35.0"

  name           = "notes"
  pool           = "tank"
  parent_dataset = "vms"
  nic_attach     = "br0"
  cpus           = 2
  memory_mib     = 4096
  # The app's space the release recommends, and what Kairos takes of the disk.
  disk_gib     = stowage_vm_seed.notes.disk_file.disk_gib
  image        = stowage_vm_seed.notes.disk_file.path
  image_sha256 = stowage_vm_seed.notes.disk_file.sha256
  seed         = data.external.seed.result.path
  seed_sha256  = data.external.seed.result.sha256
}

resource "stowage_install" "notes" {
  secret_inputs_wo         = { API_KEY = var.api_key } # given over SSH once the VM answers
  secret_inputs_wo_version = 1
  inputs                   = { ADMIN_EMAIL = "ops@example.com" }
  tls                      = "internal"

  vm {
    host             = "192.0.2.10"
    user             = "ops"
    private_key_file = pathexpand("~/.ssh/id_ed25519")
    known_hosts_line = stowage_vm_seed.notes.known_hosts_line # the one-time key, until the VM's own is read
    short_name       = stowage_vm_seed.notes.short_name
    version          = stowage_vm_seed.notes.version # a later release upgrades the VM
  }

  # deletion_protection = false   # a destroy then forgets the install; module.vm removes the VM

  depends_on = [module.vm]
}

variable "installer" {
  type        = string
  description = "The installer by digest, as `stowage ship` prints it."
}

variable "api_key" {
  type      = string
  sensitive = true
  ephemeral = true
}
