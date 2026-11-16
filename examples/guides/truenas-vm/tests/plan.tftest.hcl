# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the NICs by role, the one NIC by the primary, the size from the seed, the size
# refused under its least, the NICs added after the seed. Nothing runs.

mock_provider "truenas" {
  mock_resource "truenas_vm" {
    defaults = { id = 7 }
  }
  mock_resource "truenas_dataset" {
    defaults = { mountpoint = "/mnt/tank/vms/notes1-staging" }
  }
}

mock_provider "stowage" {}

mock_provider "external" {
  mock_data "external" {
    defaults = { result = { path = "/seeds/notes-seed.iso", sha256 = "cd00000000000000000000000000000000000000000000000000000000000000" } }
  }
}

variables {
  installer      = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  name           = "notes1"
  hostname       = "notes1"
  vm_address     = "notes1.lan"
  pool           = "tank"
  parent_dataset = "vms"
  nic_attach     = "br0"
  ssh_key_file   = "tests/id_test"
}

override_resource {
  target = stowage_vm_seed.vm
  values = {
    id               = "SHA256:x"
    short_name       = "notes"
    version          = "1.0.0"
    ctl_path         = "/cache/notes-stow"
    known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
    disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
    disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
    app_gib          = 20
    nics             = {}
    size = {
      cpus       = { min = 1, recommended = 2 }
      memory_mib = { min = 1024, recommended = 4096 }
      app_gib    = { min = 4, recommended = 20 }
    }
  }
}

run "one_nic" {
  command = plan
  assert {
    condition     = length(truenas_vm_device.nic) == 1 && jsondecode(truenas_vm_device.nic["0"].attributes).nic_attach == "br0"
    error_message = "one NIC, on nic_attach"
  }
  assert {
    condition     = truenas_vm.vm.cores == 2 && truenas_vm.vm.memory == 4096 && truenas_zvol.disk.volsize == 43 * 1024 * 1024 * 1024
    error_message = "the seed's recommended size, the zvol os_gib + app_gib"
  }
  assert {
    condition     = stowage_vm_install.vm.version == "1.0.0" && stowage_vm_install.vm.short_name == "notes" && stowage_vm_install.vm.platform == "truenas"
    error_message = "the install from the seed"
  }
}

run "macs_by_role" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
  }
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      ctl_path         = "/cache/notes-stow"
      short_name       = "notes"
      version          = "1.0.0"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics = {
        uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }
        edge   = { mac = "02:00:00:00:00:0b", attach = "br1", primary = false }
      }
    }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac == "02:00:00:00:00:0a" && jsondecode(truenas_vm_device.nic["0"].attributes).nic_attach == "br0" && jsondecode(truenas_vm_device.nic["1"].attributes).mac == "02:00:00:00:00:0b" && jsondecode(truenas_vm_device.nic["1"].attributes).nic_attach == "br1"
    error_message = "MACs by role, the primary first"
  }
}

run "roles_missing" {
  command = plan
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      ctl_path         = "/cache/notes-stow"
      short_name       = "notes"
      version          = "1.0.0"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics = {
        uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }
        edge   = { mac = "02:00:00:00:00:0b", attach = null, primary = false }
      }
    }
  }
  expect_failures = [truenas_vm.vm]
}

run "under_the_least_size" {
  command = plan
  variables {
    memory_mib = 512
  }
  expect_failures = [truenas_vm.vm, check.recommended_size]
}

run "under_the_recommended_size" {
  command = plan
  variables {
    memory_mib = 2048
  }
  expect_failures = [check.recommended_size]
}

run "a_nic_added" {
  command = plan
  variables {
    extra_nics = { edge = { mac = "02:00:00:00:00:0e", mode = "static", address = "192.0.2.20/24" } }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.extra_nic["edge"].attributes).mac == "02:00:00:00:00:0e" && stowage_vm_install.vm.interfaces["edge"].address == "192.0.2.20/24"
    error_message = "the NIC's device and its interface"
  }
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac == "02:00:00:00:00:0a"
    error_message = "the seed's MAC on the one NIC"
  }
}

# One role the seed gives no MAC (its primary): the NIC takes the root's own MAC, and the VM pins
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = null, attach = null, primary = true } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac != null && startswith(jsondecode(truenas_vm_device.nic["0"].attributes).mac, "02:")
    error_message = "the primary without a seed MAC takes the root's"
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = null, attach = null, primary = false } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  expect_failures = [truenas_vm.vm]
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = null, attach = null, primary = true } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  expect_failures = [truenas_vm.vm]
}

# A NIC attached after the seed with the MAC the seed gives the primary (another case): refused
# at plan.
run "two_nics_share_a_mac" {
  command = plan
  variables {
    nic_roles  = ["uplink", "edge"]
    extra_nics = { lan = { mac = "02:00:00:00:00:0A" } }
  }
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  expect_failures = [truenas_vm.vm]
}

# The same, each NIC its own MAC: planned.
run "each_nic_its_own_mac" {
  command = plan
  variables {
    nic_roles  = ["uplink", "edge"]
    extra_nics = { lan = { mac = "02:00:00:00:00:0c" } }
  }
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
}

# nic_macs names a role's MAC: the seed takes it (interfaces, mac=), the role's NIC too, before
# the MAC the seed would give; the primary keeps the seed's.
run "nic_macs" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
    nic_macs  = { edge = "02:00:00:00:00:1b" }
  }
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:1b", attach = null, primary = false } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  assert {
    condition     = stowage_vm_seed.vm.interfaces == tomap({ edge = "mac=02:00:00:00:00:1b" })
    error_message = "the seed takes nic_macs"
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac == "02:00:00:00:00:0a" && jsondecode(truenas_vm_device.nic["1"].attributes).mac == "02:00:00:00:00:1b"
    error_message = "the NICs take nic_macs, else the seed's"
  }
}

# One NIC, nic_roles left out: nic_macs names the seed's primary's role, before the seed's MAC.
run "nic_macs_of_the_primary" {
  command = plan
  variables {
    nic_macs = { uplink = "02:00:00:00:00:1a" }
  }
  override_resource {
    target = stowage_vm_seed.vm
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      ctl_path         = "/cache/notes-stow"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = null, attach = null, primary = true } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac == "02:00:00:00:00:1a"
    error_message = "the primary takes nic_macs before the derived MAC"
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = { uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }, edge = { mac = "02:00:00:00:00:0b", attach = null, primary = false } }
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  expect_failures = [truenas_vm.vm]
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
      disk             = { truenas = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = null } }
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab00000000000000000000000000000000000000000000000000000000000000", disk_gib = 43 }
      app_gib          = 20
      nics             = null
      size = {
        cpus       = { min = 1, recommended = 2 }
        memory_mib = { min = 1024, recommended = 4096 }
        app_gib    = { min = 4, recommended = 20 }
      }
    }
  }
  assert {
    condition     = jsondecode(truenas_vm_device.nic["0"].attributes).mac == "02:00:00:00:00:2a" && jsondecode(truenas_vm_device.nic["1"].attributes).mac == "02:00:00:00:00:2b"
    error_message = "each NIC takes nic_macs"
  }
}
