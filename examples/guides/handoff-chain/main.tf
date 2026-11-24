# A handoff chain on one Docker host, in one apply: install A issues a kit (`second-site`), the
# first install of B takes it; B issues one back, which A takes by its re-run; each issuer proves
# its kit was taken. The kits never touch a file of the root, the state or a plan: each lives in the provider's
# process from its stowage_kit to the ephemeral stowage_kit that hands it on, in that apply alone.

locals {
  ssh = {
    host             = var.host
    port             = var.port
    user             = var.user
    private_key_file = var.private_key_file
    known_hosts_line = var.known_hosts_line
  }
}

resource "stowage_docker_install" "a" {
  installer = var.installer
  # The key B's kit gives A back, the same value: a refresh reads the value the install records,
  # and a kit giving another would show as a change the next apply gives back.
  inputs              = { ADMIN_EMAIL = var.site_a_admin }
  ports               = var.ports.a
  work_dir            = var.work_dirs.a
  deletion_protection = !var.destroyable
  delete_data         = var.destroyable

  host             = local.ssh.host
  port             = local.ssh.port
  user             = local.ssh.user
  private_key_file = local.ssh.private_key_file
  known_hosts_line = local.ssh.known_hosts_line
}

# A's kit for B: issued by A's kept installer (`kit create second-site --json`).
resource "stowage_kit" "a_to_b" {
  handoff  = "second-site"
  instance = "site-b"
  answers  = { SITE_ADMIN = var.site_b_admin }
  version  = var.kit_versions.a_to_b

  docker {
    host             = local.ssh.host
    port             = local.ssh.port
    user             = local.ssh.user
    private_key_file = local.ssh.private_key_file
    known_hosts_line = local.ssh.known_hosts_line
    install_id       = stowage_docker_install.a.id
    short_name       = stowage_docker_install.a.short_name
  }
}

ephemeral "stowage_kit" "a_to_b" {
  id = stowage_kit.a_to_b.id
}

# B's first install takes A's kit: ADMIN_EMAIL comes from it.
resource "stowage_docker_install" "b" {
  installer           = var.installer
  kit_wo              = ephemeral.stowage_kit.a_to_b.kit
  kit_code_wo         = ephemeral.stowage_kit.a_to_b.code
  kit_wo_version      = stowage_kit.a_to_b.id
  ports               = var.ports.b
  work_dir            = var.work_dirs.b
  deletion_protection = !var.destroyable
  delete_data         = var.destroyable

  host             = local.ssh.host
  port             = local.ssh.port
  user             = local.ssh.user
  private_key_file = local.ssh.private_key_file
  known_hosts_line = local.ssh.known_hosts_line
}

# A proves B took its kit. Its id is A's directory whether or not B was made again: a replaced B
# replaces the check.
resource "stowage_kit_check" "a_to_b" {
  handoff  = stowage_kit.a_to_b.handoff
  instance = stowage_kit.a_to_b.instance
  triggers = { kit = stowage_kit.a_to_b.id }

  docker {
    host             = local.ssh.host
    port             = local.ssh.port
    user             = local.ssh.user
    private_key_file = local.ssh.private_key_file
    known_hosts_line = local.ssh.known_hosts_line
    install_id       = stowage_docker_install.a.id
    short_name       = stowage_docker_install.a.short_name
  }

  lifecycle {
    replace_triggered_by = [stowage_docker_install.b]
  }
}

# B's kit back to A.
resource "stowage_kit" "b_to_a" {
  handoff  = "second-site"
  instance = "site-a"
  answers  = { SITE_ADMIN = coalesce(var.returned_admin, var.site_a_admin) }
  version  = var.kit_versions.b_to_a

  docker {
    host             = local.ssh.host
    port             = local.ssh.port
    user             = local.ssh.user
    private_key_file = local.ssh.private_key_file
    known_hosts_line = local.ssh.known_hosts_line
    install_id       = stowage_docker_install.b.id
    short_name       = stowage_docker_install.b.short_name
  }
}

ephemeral "stowage_kit" "b_to_a" {
  id = stowage_kit.b_to_a.id
}

# A exists: it takes B's kit by its re-run. (As A's own kit_wo it would make A wait for B, which
# waits for A.) A replaced A takes it again once the kit is issued again: bump kit_versions.b_to_a.
# After A's own check: two runs on one install are ordered by the root, as the installer refuses
# one beside another.
resource "stowage_kit_take" "a" {
  depends_on = [stowage_kit_check.a_to_b]

  kit_wo         = ephemeral.stowage_kit.b_to_a.kit
  kit_code_wo    = ephemeral.stowage_kit.b_to_a.code
  kit_wo_version = stowage_kit.b_to_a.id

  docker {
    host             = local.ssh.host
    port             = local.ssh.port
    user             = local.ssh.user
    private_key_file = local.ssh.private_key_file
    known_hosts_line = local.ssh.known_hosts_line
    install_id       = stowage_docker_install.a.id
    short_name       = stowage_docker_install.a.short_name
  }
}

# B proves A took its kit.
resource "stowage_kit_check" "b_to_a" {
  handoff  = stowage_kit.b_to_a.handoff
  instance = stowage_kit.b_to_a.instance
  triggers = { take = stowage_kit_take.a.id }

  docker {
    host             = local.ssh.host
    port             = local.ssh.port
    user             = local.ssh.user
    private_key_file = local.ssh.private_key_file
    known_hosts_line = local.ssh.known_hosts_line
    install_id       = stowage_docker_install.b.id
    short_name       = stowage_docker_install.b.short_name
  }

  lifecycle {
    replace_triggered_by = [stowage_docker_install.a]
  }
}

output "proven" {
  value = {
    a_to_b = stowage_kit_check.a_to_b.proven
    b_to_a = stowage_kit_check.b_to_a.proven
  }
}
