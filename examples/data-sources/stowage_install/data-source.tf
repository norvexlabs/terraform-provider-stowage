# What a Docker install runs, read without managing it (another root's install, a dashboard).
data "stowage_install" "notes" {
  name       = "notes"
  short_name = "notes"

  docker {
    host             = "app.example.com"
    user             = "ops"
    known_hosts_line = "app.example.com ssh-ed25519 AAAA..."
    work_dir         = "/srv"
  }
}

output "notes_version" {
  value = data.stowage_install.notes.installed.version
}
