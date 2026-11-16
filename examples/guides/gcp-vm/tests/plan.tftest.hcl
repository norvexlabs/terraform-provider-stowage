# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the VM from the seed (its size, its disk, its image, its firewall), the install from
# the seed, a size under the release's least refused. Nothing runs.

mock_provider "google" {}

mock_provider "stowage" {}

variables {
  installer    = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  name         = "notes1"
  hostname     = "notes1"
  vm_address   = "notes1.lan"
  ssh_key_file = "tests/id_test"
  project        = "p"
  zone           = "europe-west1-b"
  firewall       = true
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
    disk             = { gcp = { format = "qcow2", image = "r/notes-disk@sha256:ab", os_gib = 23, disk_gib = 43, cloud_image = "notes-0123456789ab" } }
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
    condition     = google_compute_instance.vm.boot_disk[0].initialize_params[0].image == "notes-0123456789ab" && length(google_compute_firewall.ssh) == 1
    error_message = "the VM from the seed"
  }
  assert {
    condition     = stowage_vm_install.vm.version == "1.0.0" && stowage_vm_install.vm.short_name == "notes" && stowage_vm_install.vm.platform == "gcp" && stowage_vm_install.vm.host == "notes1.lan"
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
