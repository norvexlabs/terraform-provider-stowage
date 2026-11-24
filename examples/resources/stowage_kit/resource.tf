# A kit the install "issuer" issues for another install of the project its handoff names; the
# ephemeral stowage_kit hands it on in the same apply (see the guide Handoff kits).
resource "stowage_kit" "site_b" {
  handoff    = "second-site"
  instance   = "site-b"
  answers    = { SITE_ADMIN = "admin@b.example" }
  answers_wo = { SITE_TOKEN = var.site_token } # a secret question: write-only, never in state
  expires    = "1h"
  version    = 1 # bump to issue again

  docker {
    host             = stowage_docker_install.issuer.host
    user             = stowage_docker_install.issuer.user
    known_hosts_line = stowage_docker_install.issuer.known_hosts_line
    install_id       = stowage_docker_install.issuer.id
    short_name       = stowage_docker_install.issuer.short_name
  }
}

ephemeral "stowage_kit" "site_b" {
  id = stowage_kit.site_b.id
}

resource "stowage_docker_install" "site_b" {
  installer      = var.installer
  kit_wo         = ephemeral.stowage_kit.site_b.kit
  kit_code_wo    = ephemeral.stowage_kit.site_b.code
  kit_wo_version = stowage_kit.site_b.id
  # …
}
