# A Stowage product installed on a Docker host reached over SSH.
resource "stowage_install" "notes" {
  # The installer by digest, as `stowage ship` prints it: the digest is the release.
  installer = "registry.example.com/notes/notes-installer@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

  inputs = {
    ADMIN_EMAIL = "ops@example.com"
  }
  # Write-only: never in plan or state. Bump the version to apply a change.
  secret_inputs_wo = {
    SMTP_PASSWORD = var.smtp_password
  }
  secret_inputs_wo_version = 1

  tls     = "internal"
  domains = { app = "notes.lan" }

  docker {
    host             = "app.example.com"
    user             = "ops"
    private_key_file = pathexpand("~/.ssh/id_ed25519")
    # The host's own key, checked against its fingerprint: no other key is accepted.
    known_hosts_line = "app.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
    work_dir         = "/srv"
  }

  # deletion_protection = false   # before a destroy
  # delete_data         = true    # a destroy deletes the volumes too

  timeouts {
    create = "45m"
  }
}

variable "smtp_password" {
  type      = string
  sensitive = true
  ephemeral = true
}
