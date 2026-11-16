# A Kairos VM's first-boot material: the VM is made from it by its platform's own provider (a whole
# root per platform: the platform guides, examples/guides/<platform>-vm).
resource "stowage_vm_seed" "notes" {
  installer      = var.installer
  platform       = "truenas"
  hostname       = "notes"
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  tls            = "internal"
  inputs         = { ADMIN_EMAIL = "ops@example.com" }
  # The disk written here as a file (disk_file), for a hypervisor resource that imports it.
  disk_dir = pathexpand("~/.cache/notes/disks")
}

output "disk" {
  value = stowage_vm_seed.notes.disk_file # path, format, sha256, disk_gib
}

output "size" {
  value = stowage_vm_seed.notes.size # cpus, memory_mib, app_gib: {min, recommended}
}

output "ctl" {
  value = stowage_vm_seed.notes.ctl_path # the release's installer for this machine
}

variable "installer" {
  type        = string
  description = "The installer by digest, as `stowage ship` prints it."
}
