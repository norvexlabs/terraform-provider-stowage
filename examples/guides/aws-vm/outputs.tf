output "id" {
  value = aws_instance.vm.id
}

output "address" {
  value = local.address
}

output "host_key_line" {
  description = "The VM's own SSH host key as a known_hosts line: give it to every later verb over SSH (--known-hosts-line)."
  value       = stowage_vm_install.vm.host_key_line
}

output "version" {
  value = stowage_vm_install.vm.version
}
