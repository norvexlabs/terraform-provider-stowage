# The install "issuer" exists: it takes the kit site B issued back by its re-run.
resource "stowage_kit_take" "issuer" {
  kit_wo         = ephemeral.stowage_kit.back.kit
  kit_code_wo    = ephemeral.stowage_kit.back.code
  kit_wo_version = stowage_kit.back.id # a new kit is taken again

  docker {
    host             = stowage_docker_install.issuer.host
    user             = stowage_docker_install.issuer.user
    known_hosts_line = stowage_docker_install.issuer.known_hosts_line
    install_id       = stowage_docker_install.issuer.id
    short_name       = stowage_docker_install.issuer.short_name
  }
}
