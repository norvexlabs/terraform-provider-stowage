# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the VM from the seed (its size, its disk, its image, its firewall), the install from
# the seed, a size under the release's least refused. Nothing runs.

mock_provider "azurerm" {}

mock_provider "stowage" {}

variables {
  installer    = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  name         = "notes1"
  hostname     = "notes1"
  vm_address   = "notes1.lan"
  ssh_key_file = "tests/id_test"
  resource_group = "rg"
  location       = "westeurope"
  subnet_id      = "/subscriptions/12345678-1234-9876-4563-123456789012/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vn/subnets/sn"
  admin_user     = "ops"
}

override_resource {
  target = stowage_vm_seed.vm
  values = {
    id               = "SHA256:x"
    short_name       = "notes"
    version          = "1.0.0"
    ctl_path         = "/cache/notes-stow"
    known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
    user_data        = "#cloud-config\n"
    disk             = { azure = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
    disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
    app_gib          = 20
    nics             = {}
    size = {
      cpus       = { min = 1, recommended = 2 }
      memory_mib = { min = 1024, recommended = 4096 }
      app_gib    = { min = 4, recommended = 20 }
    }
    firewall = {
      ssh_sources = ["0.0.0.0/0"]
      open_ports = [
        { protocol = "tcp", port = 443, sources = ["0.0.0.0/0", "::/0"] },
      ]
    }
  }
}

override_data {
  target = data.azurerm_image.release[0]
  values = { id = "/subscriptions/12345678-1234-9876-4563-123456789012/resourceGroups/rg/providers/Microsoft.Compute/images/notes-0123456789ab" }
}

override_resource {
  target = azurerm_public_ip.vm[0]
  values = { id = "/subscriptions/12345678-1234-9876-4563-123456789012/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/notes1-ip", ip_address = "198.51.100.7" }
}

override_resource {
  target = azurerm_network_interface.vm
  values = { id = "/subscriptions/12345678-1234-9876-4563-123456789012/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/notes1-nic" }
}

override_resource {
  target = azurerm_network_security_group.vm
  values = { id = "/subscriptions/12345678-1234-9876-4563-123456789012/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/notes1-nsg" }
}

run "from_the_seed" {
  command = plan
  assert {
    condition     = azurerm_linux_virtual_machine.vm.os_disk[0].disk_size_gb == 43 && length(azurerm_network_security_group.vm.security_rule) == 3
    error_message = "the VM from the seed"
  }
  assert {
    condition     = stowage_vm_install.vm.version == "1.0.0" && stowage_vm_install.vm.short_name == "notes" && stowage_vm_install.vm.platform == "azure" && stowage_vm_install.vm.host == "notes1.lan"
    error_message = "the install from the seed"
  }
}

run "under_the_least_size" {
  command = plan
  variables {
    memory_mib = 512
  }
  expect_failures = [terraform_data.size, check.recommended_size]
}
