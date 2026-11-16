# Plans the root under mocked providers (`tofu test` in this directory, the stowage provider
# installed): the install pinned to the host key the instance is given. Nothing runs.

mock_provider "aws" {}
mock_provider "tls" {
  mock_resource "tls_private_key" {
    defaults = {
      public_key_openssh  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOOPObYysKrGfdSCx6n8K+NsxHbizEZBwOJgzyzElXSO\n"
      private_key_openssh = "-----BEGIN OPENSSH PRIVATE KEY-----\nx\n-----END OPENSSH PRIVATE KEY-----\n"
    }
  }
}
mock_provider "stowage" {}

variables {
  installer    = "registry.example.com/notes/notes-installer@sha256:0000000000000000000000000000000000000000000000000000000000000000"
  region       = "eu-west-3"
  subnet_id    = "subnet-1"
  ssh_sources  = ["198.51.100.7/32"]
  ssh_key_file = "tests/id_test"
}

override_resource {
  target = aws_instance.docker
  values = { public_ip = "203.0.113.9" }
}

run "pinned" {
  command = plan
  assert {
    condition     = startswith(stowage_docker_install.app.known_hosts_line, "203.0.113.9 ssh-ed25519 ") && stowage_docker_install.app.host == "203.0.113.9"
    error_message = "the install pins the host key the instance is given"
  }
}
