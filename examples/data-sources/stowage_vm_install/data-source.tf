# What a VM runs, read without managing it.
data "stowage_vm_install" "notes" {
  name             = "notes"
  short_name       = "notes"
  host             = "192.0.2.10"
  user             = "ops"
  known_hosts_line = "192.0.2.10 ssh-ed25519 AAAA..." # the VM's own key
}
