output "id" {
  value = truenas_vm.vm.id
}

output "zvol" {
  value = truenas_zvol.disk.id
}

output "staging" {
  value = truenas_dataset.staging.id
}

output "host_key_line" {
  description = "The VM's own SSH host key as a known_hosts line: give it to every later verb over SSH (--known-hosts-line)."
  value       = stowage_vm_install.vm.host_key_line
}

output "seed_boot_id" {
  value = stowage_vm_install.vm.seed_boot_id
}

output "version" {
  value = stowage_vm_install.vm.version
}
