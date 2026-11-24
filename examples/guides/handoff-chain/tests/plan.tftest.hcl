# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): each kit after the install that issues it, B after A's kit. Nothing runs.

mock_provider "stowage" {}

# The installs' exported values as an install records them (the mock's random strings are no
# install's id or short name, which the kit blocks refuse).
override_resource {
  target = stowage_docker_install.a
  values = { id = "/srv/a/notes", short_name = "notes" }
}

override_resource {
  target = stowage_docker_install.b
  values = { id = "/srv/b/notes", short_name = "notes" }
}

variables {
  installer          = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  release_public_key = "-----BEGIN PUBLIC KEY-----\nMFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE\n-----END PUBLIC KEY-----\n"
  host               = "docker.example"
  user               = "ops"
  known_hosts_line   = "docker.example ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
  work_dirs          = { a = "/srv/a", b = "/srv/b" }
  ports              = { a = { app = 18080, api = 18000 }, b = { app = 18081, api = 18001 } }
  site_a_admin       = "admin@a.example"
  site_b_admin       = "admin@b.example"
}

run "chain" {
  command = plan
  assert {
    condition     = stowage_kit.a_to_b.handoff == "second-site" && stowage_kit.a_to_b.instance == "site-b" && stowage_kit.b_to_a.instance == "site-a"
    error_message = "a kit each way"
  }
  assert {
    condition     = stowage_docker_install.a.deletion_protection && !stowage_docker_install.b.delete_data
    error_message = "protected unless destroyable"
  }
}
