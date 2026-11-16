# The install on a Kairos VM a hypervisor's resource made from stowage_vm_seed (a whole root per
# platform: the platform guides).
resource "stowage_vm_seed" "notes" {
  installer      = "registry.example.com/notes/notes-installer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  platform       = "truenas"
  hostname       = "notes"
  user           = "ops"
  ssh_public_key = file(pathexpand("~/.ssh/id_ed25519.pub"))
  network        = "static"
  address        = "192.0.2.10/24"
  gateway        = "192.0.2.1"
  dns            = ["192.0.2.1"]
  tls            = "internal"
}

# ... the VM, from stowage_vm_seed.notes.user_data and .disk (see the platform guides) ...

resource "stowage_vm_install" "notes" {
  host             = "192.0.2.10"
  user             = "ops"
  private_key_file = pathexpand("~/.ssh/id_ed25519")
  known_hosts_line = stowage_vm_seed.notes.known_hosts_line # the one-time key, until the VM's own is read
  short_name       = stowage_vm_seed.notes.short_name
  version          = stowage_vm_seed.notes.version # a later release upgrades the VM
  platform         = stowage_vm_seed.notes.platform

  inputs = { ADMIN_EMAIL = "ops@example.com" }
  tls    = "internal"

  # depends_on = [<the VM's power resource>]
}
