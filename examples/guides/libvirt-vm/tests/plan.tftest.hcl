# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the VM from the seed (its size, its disk, its image, its firewall), the install from
# the seed, a size under the release's least refused. Nothing runs.

mock_provider "libvirt" {}

mock_provider "stowage" {}

mock_provider "external" {
  mock_data "external" {
    defaults = { result = { path = "/seeds/notes-seed.iso", sha256 = "cd00000000000000000000000000000000000000000000000000000000000000" } }
  }
}

variables {
  installer    = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  name         = "notes1"
  hostname     = "notes1"
  vm_address   = "notes1.lan"
  ssh_key_file = "tests/id_test"
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
    disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
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

run "from_the_seed" {
  command = plan
  assert {
    condition     = libvirt_domain.vm.vcpu == 2 && libvirt_domain.vm.memory == 4096 && libvirt_volume.disk.capacity == 43 * 1024 * 1024 * 1024
    error_message = "the VM from the seed"
  }
  assert {
    condition     = stowage_vm_install.vm.version == "1.0.0" && stowage_vm_install.vm.short_name == "notes" && stowage_vm_install.vm.platform == "libvirt" && stowage_vm_install.vm.host == "notes1.lan"
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

# One NIC the release names, nic_roles left out: the NIC takes the MAC the seed gives the primary.
run "the_seeds_mac" {
  command = plan
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      user_data        = "#cloud-config\n"
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true } }
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
  assert {
    condition     = libvirt_domain.vm.devices.interfaces[0].mac.address == "02:00:00:00:00:0a"
    error_message = "the seed's MAC on the one NIC"
  }
}

# One role the seed gives no MAC (its primary): the NIC takes the platform's MAC, and the VM pins
# its name to it at the first boot.
run "a_primary_without_a_mac" {
  command = plan
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      user_data        = "#cloud-config\n"
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = null, attach = null, primary = true } }
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
  assert {
    condition     = libvirt_domain.vm.devices.interfaces[0].mac == null
    error_message = "the primary without a seed MAC takes the platform's"
  }
}

# A secondary role the seed gives no MAC: refused at plan.
run "a_secondary_without_a_mac" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = null, attach = null, primary = false } }
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
  expect_failures = [terraform_data.size]
}

# A role in nic_roles the seed does not name, its primary without a MAC: refused at plan (the
# primary alone takes a MAC the seed does not give).
run "a_role_the_seed_does_not_name" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = null, attach = null, primary = true } }
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
  expect_failures = [terraform_data.size]
}

# A NIC attached after the seed with the MAC the seed gives the primary (another case): refused
# at plan.
run "two_nics_share_a_mac" {
  command = plan
  variables {
    nic_roles  = ["uplink", "edge"]
    interfaces = { lan = { mac = "02:00:00:00:00:0A" } }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
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
  expect_failures = [terraform_data.size]
}

# The same, each NIC its own MAC: planned.
run "each_nic_its_own_mac" {
  command = plan
  variables {
    nic_roles  = ["uplink", "edge"]
    interfaces = { lan = { mac = "02:00:00:00:00:0c" } }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
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
}

# nic_macs names a role's MAC: the seed takes it (interfaces, mac=, after the role's own spec),
# the role's NIC too, before the MAC the seed would give; the primary keeps the seed's.
run "nic_macs" {
  command = plan
  variables {
    nic_roles       = ["uplink", "edge"]
    nic_macs        = { edge = "02:00:00:00:00:1b" }
    seed_interfaces = { edge = "dhcp" }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:1b", attach = null, primary = false } }
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
  assert {
    condition     = stowage_vm_seed.vm.interfaces == tomap({ edge = "dhcp,mac=02:00:00:00:00:1b" })
    error_message = "the seed takes nic_macs after the role's spec"
  }
  assert {
    condition     = libvirt_domain.vm.devices.interfaces[0].mac.address == "02:00:00:00:00:0a" && libvirt_domain.vm.devices.interfaces[1].mac.address == "02:00:00:00:00:1b"
    error_message = "the NICs take nic_macs, else the seed's"
  }
}

# nic_macs naming another MAC than the seed's: refused (the seed is made once).
run "nic_macs_not_the_seeds" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
    nic_macs  = { edge = "02:00:00:00:00:1c" }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
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
  expect_failures = [terraform_data.size]
}

# nic_macs and seed_interfaces both giving a role's MAC: refused.
run "nic_macs_and_a_seed_mac" {
  command = plan
  variables {
    nic_roles       = ["uplink", "edge"]
    nic_macs        = { edge = "02:00:00:00:00:0b" }
    seed_interfaces = { edge = "mac=02:00:00:00:00:0b" }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
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
  expect_failures = [terraform_data.size]
}

# An installer that names no NIC (nics null): nic_macs gives each role in nic_roles its MAC.
run "nic_macs_without_seed_nics" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
    nic_macs  = { uplink = "02:00:00:00:00:2a", edge = "02:00:00:00:00:2b" }
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
      disk             = { libvirt = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = null
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
  assert {
    condition     = libvirt_domain.vm.devices.interfaces[0].mac.address == "02:00:00:00:00:2a" && libvirt_domain.vm.devices.interfaces[1].mac.address == "02:00:00:00:00:2b"
    error_message = "each NIC takes nic_macs"
  }
}
