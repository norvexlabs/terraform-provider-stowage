# Plans the root under a mocked TrueNAS provider (`tofu test` in this directory, the stowage
# provider installed): the NICs by role, the one NIC by the primary, the precondition. Nothing runs.

mock_provider "truenas" {
  mock_resource "truenas_vm" {
    defaults = { id = "7" }
  }
}

variables {
  installer        = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  admin_email      = "a@example.com"
  truenas_url      = "https://192.0.2.2"
  truenas_ssh_host = "192.0.2.2"
  address          = "192.0.2.10/24"
  gateway          = "192.0.2.1"
}

run "one_nic" {
  command = plan
  assert {
    condition     = length(truenas_vm_device.nic) == 1
    error_message = "one NIC"
  }
}

run "two_nics" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
  }
  assert {
    condition     = length(truenas_vm_device.nic) == 2
    error_message = "two NICs"
  }
}

run "macs_by_role" {
  command = plan
  variables {
    nic_roles = ["uplink", "edge"]
  }
  override_resource {
    target = stowage_vm_seed.notes
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab", disk_gib = 43 }
      nics = {
        uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }
        edge   = { mac = "02:00:00:00:00:0b", attach = "br1", primary = false }
      }
    }
  }
  assert {
    condition     = truenas_vm_device.nic["0"].attributes.mac == "02:00:00:00:00:0a" && truenas_vm_device.nic["0"].attributes.nic_attach == "br0" && truenas_vm_device.nic["1"].attributes.mac == "02:00:00:00:00:0b" && truenas_vm_device.nic["1"].attributes.nic_attach == "br1"
    error_message = "MACs by role"
  }
}

run "one_nic_any_role" {
  command = plan
  override_resource {
    target = stowage_vm_seed.notes
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab", disk_gib = 43 }
      nics             = { lan = { mac = "02:00:00:00:00:0c", attach = "br2", primary = true } }
    }
  }
  assert {
    condition     = truenas_vm_device.nic["0"].attributes.mac == "02:00:00:00:00:0c" && truenas_vm_device.nic["0"].attributes.nic_attach == "br2"
    error_message = "the one NIC is the primary"
  }
}

run "roles_missing" {
  command = plan
  override_resource {
    target = stowage_vm_seed.notes
    values = {
      id               = "SHA256:x"
      short_name       = "notes"
      version          = "1.0.0"
      known_hosts_line = "vm-seed-host-key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO"
      disk_file        = { path = "/d/n.qcow2", format = "qcow2", sha256 = "ab", disk_gib = 43 }
      nics = {
        uplink = { mac = "02:00:00:00:00:0a", attach = null, primary = true }
        edge   = { mac = "02:00:00:00:00:0b", attach = null, primary = false }
      }
    }
  }
  expect_failures = [truenas_vm.notes]
}
