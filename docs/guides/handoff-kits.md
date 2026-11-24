---
page_title: "Handoff kits - stowage"
subcategory: "Guides"
description: |-
  Issue a handoff kit on one install, give it to another, take one back and prove each, in one apply.
---

# Handoff kits

A handoff kit carries what one install issues to another (inputs, sealed with a code): Stowage's
`kit create` and `--kit`. The provider runs the whole cycle with no verb by hand, and the kit and
its code never reach a file of the root, the state, a plan or a log (the run that issues or takes
one reads and writes them through a private temporary directory, 0700, removed when it ends):

1. **`stowage_kit`** — the issuing install runs `kit create HANDOFF --json`, the request (answers
   included) a file or standard input, never a command line. The kit and its code are kept in the
   provider's process, under the resource's `id`.
2. **The ephemeral `stowage_kit`** (`id = stowage_kit.x.id`) hands them on, in that apply alone:
   into `kit_wo` and `kit_code_wo` of **an install resource** (its first install takes the kit) or
   of **`stowage_kit_take`** (an install that exists takes it by its re-run: a kit going back to the
   install that issued the first one, which its own `kit_wo` cannot, as each install would wait for
   the other).
3. **`stowage_kit_check`** — the issuing install's `kit check HANDOFF INSTANCE`, tried until it
   answers: proven, not proven (an apply error by default), or no verdict by its timeout.

Each of them names its install by one block holding what that install's resource exports
(`docker`, `kubernetes` or `vm`), which orders it after that install. Kit resources on one
install are ordered against one another with `depends_on`: the installer refuses a run beside
another (Docker, the VM); on Kubernetes helm refuses a take over a pending upgrade, and a kit's
Job runs beside anything. On Kubernetes the cluster is the provider's `kubernetes` block: an
install on another cluster has a provider alias of its own; the ephemeral `stowage_kit` uses the
`stowage_kit`'s alias (each alias is a process of its own, holding its own kits), and what takes
the kit may be under any.

## What a later apply sees

The ephemeral is null in every plan and apply but the one that issued the kit, so a kit is issued
and taken in one apply. What takes it sets `kit_wo_version = stowage_kit.x.id`, which changes at
each issue: a kit expected and null (an apply that failed after issuing it, before the take) is an
error naming the fix — bump the `stowage_kit`'s `version`, which issues a kit again (the project's
script decides whether it can be issued twice). A replaced install keeps its `id`: give a
`stowage_kit_check` `replace_triggered_by` on the install the kit went to, so it checks again, as
the root below does. A replaced install that took a kit takes it again only with a kit issued again
(bump the `stowage_kit`'s `version`): never give a `stowage_kit_take` or an install's `kit_wo`
`replace_triggered_by`, as any update of what it names would replace it in an apply with no kit.

A key a kit gives that the target's resource also gives in `inputs` is the kit's on the install
once taken: the resource's refresh reads the recorded value, and its next plan gives the
configuration's back. Give such a key in one place (or the same value in both). A handoff that gives
TLS material (`KIT_TLS_*`) is refused by `stowage_kit` before it issues anything, as taking it would
move the target's front to `byo` behind its resource's `tls`.

## A chain in one apply

Install A issues a kit for B's first install; B issues one back, which A takes; each proves its kit
was taken. Apply once; a plan after it plans nothing; destroy removes both installs
(`destroyable = true` first). The whole root: `examples/guides/handoff-chain`.

```terraform
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
```
