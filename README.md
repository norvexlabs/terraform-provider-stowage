<p align="center">
  <img src="docs/img/logo.svg" alt="Stowage" width="300">
</p>

<p align="center">
  <b>The OpenTofu provider for Stowage: declare a product's install beside the VM, cloud or
  cluster it runs on, in one OpenTofu root.</b>
</p>

<p align="center">
  <a href="LICENSE"><img alt="Licence: Apache 2.0" src="https://img.shields.io/badge/licence-Apache%202.0-7571F9"></a>
  <a href="CHANGELOG.md"><img alt="Status: pre-1.0" src="https://img.shields.io/badge/status-pre--1.0-F780E2"></a>
  <img alt="OpenTofu ≥ 1.11" src="https://img.shields.io/badge/OpenTofu-%E2%89%A5%201.11-02BF87">
  <a href="https://github.com/norvexlabs/stowage"><img alt="Stowage ≥ 0.34.0" src="https://img.shields.io/badge/Stowage-%E2%89%A5%200.34.0-02BF87"></a>
</p>

# terraform-provider-stowage

The OpenTofu provider for [Stowage](https://github.com/norvexlabs/stowage): it declares a Stowage
product's install beside the VM, cloud or cluster it runs on, in one OpenTofu root. It reads only
the installer's machine interface (`describe --json`, `status --json`, the verbs' `--json` result
and exit status): it decides nothing the installer does not decide, orders no version and keeps no
copy of a rule.

**Status: pre-release (0.1.0, unreleased).** What works: the Docker target — create, read,
update (inputs, then the release: upgrade or rollback), import and delete; the VM target and
`stowage_vm_seed` — the same operations on a Kairos VM a hypervisor's or cloud's resource made from
the seed. Not yet: the plan-time dry run, the Kubernetes target — see [TODO.md](TODO.md). Not yet
proven against a real installer (unit tests drive the verbs' results as Stowage prints them).

- Provider address: `registry.opentofu.org/norvexlabs/stowage`, plugin protocol 6.
- Needs **OpenTofu ≥ 1.11** (write-only attributes), and for a remote target the system `ssh`
  binary, OpenSSH ≥ 8.5 (`KnownHostsCommand`), on the machine running OpenTofu. The target needs
  a POSIX `sh`, `tar`, `mktemp` and Docker; a VM, those of its OS bundle and `sudo` without a
  password for its SSH user (the bundle's admin group). `stowage_vm_seed` runs the installer on
  the machine running OpenTofu: there, Docker and a linux/amd64 machine.
- Reads installers made by **Stowage 0.34.0 or later** (machine interface format 1).

## Example

```hcl
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = { source = "registry.opentofu.org/norvexlabs/stowage" }
  }
}

provider "stowage" {}

resource "stowage_install" "notes" {
  installer = "registry.example.com/notes/notes-installer@sha256:<64 hex>"
  inputs    = { ADMIN_EMAIL = "ops@example.com" }
  secret_inputs_wo         = { SMTP_PASSWORD = var.smtp_password }
  secret_inputs_wo_version = 1
  docker {
    host             = "app.example.com"
    user             = "ops"
    known_hosts_line = "app.example.com ssh-ed25519 AAAA…"
    work_dir         = "/srv"
  }
}
```

More in [examples/](examples/).

## Provider configuration

| Attribute | | |
|---|---|---|
| `username` | optional | The registry's pull account, with `password`. The target logs in with it to fetch the installer image, in a Docker configuration of its own (`DOCKER_CONFIG` in a private temporary directory) that is removed afterwards: the credential is not left on the host. Left out, the target's own `docker login` (or none) fetches it. The login goes to the installer reference's registry host (`docker.io` for a reference without one). |
| `password` | optional, sensitive | The pull account's password or token, with `username`; sent on the target's standard input only. One without the other is refused. |
| `registry` | optional | A mirror's prefix (the installer's `--registry`), given at the install. First-install only: the resource records it (`registry`), and a plan whose provider `registry` differs is an error. |
| `registry_ca` | optional | PEM of the mirror's certificate authority (the installer's `--registry-ca`), sent on standard input. Refused unless it holds `-----BEGIN CERTIFICATE-----`. The target's Docker daemon must already trust the mirror to fetch the installer image itself. |

## Resource `stowage_install`

One install on one target: exactly one of the `docker`, `kubernetes` and `vm` blocks (none, or
two, is an error).

### Arguments

| Attribute | | |
|---|---|---|
| `installer` | required with `docker` / `kubernetes`; refused with `vm` | The installer image by digest, `<prefix>/<name>-installer@sha256:<64 hex>` (lower-case hex, no tag beside the digest), as `stowage ship` prints it. An installer carries one release, so the digest is the release. `<name>` is the project's name: `describe` must report the same, or the create is refused. |
| `inputs` | optional map | Non-secret inputs, each given as `--set KEY=VALUE`. They are on the target's command line and in state. A key is a letter or `_`, then letters, digits or `_`; `tls` is refused (the answers file's reserved section). An input given a non-empty value here that `describe` marks `secret` is refused: at create, and at an update (every key of the map, changed or not) for the running release and the one an upgrade goes to, before anything runs. A key taken out of the map is given empty (`--set KEY=`) to the re-run: the installer's rule for an empty value decides (a required input is refused, an advanced one goes back to its default); a key taken out that `secret_inputs_wo` now holds is not (`--set` would win over its answer), and, given another value than `inputs` gave, needs a new `secret_inputs_wo_version` in the same apply (else an error at apply, before anything runs: `an input moved to secret_inputs_wo`). |
| `secret_inputs_wo` | optional map, **write-only** | Secret inputs, never in plan or state, sent on standard input as an answers file (`--answers`). A key in both maps is refused. |
| `secret_inputs_wo_version` | optional number | Change it to apply a change of `secret_inputs_wo` (a write-only value is never compared): the update re-sends every secret input the running release declares. Leaving it out with `secret_inputs_wo` set is a warning. |
| `given_wo` | optional, **write-only** | What another install gives (the installer's `--given` YAML), used at create, on standard input. |
| `secrets_wo` | optional, **write-only** | A decrypted `secrets export` of a machine made again (`--secrets`), used at create, on standard input. Docker only: refused with `vm` (below). |
| `tls` | optional | The front's TLS mode, `--tls`: `acme`, `dns-cloudflare`, `byo` or `internal`. Left out, loopback mode. A change is given to the re-run (`--tls`); taking it out is a plan error. |
| `domains` | optional map | Route → domain, `--domains route=host,…` (sorted). A route name is a letter or digit, then letters, digits, `_ . -`; a value holding `,`, `=` or a space is refused. A changed or added route is given to the re-run; taking one out, or adding one while `installer` changes, is a plan error. |
| `ports` | optional map of numbers | Route → port (1–65535), `--ports route=port,…`. A changed port is given to the re-run; a route state does not record goes to the upgrade's `--ports` when the release changes (taken as a route the release adds: set the port of a route the running release has in an apply of its own), else to the re-run; taking one out is a plan error. |
| `address` | optional | The loopback address the routes bind, `--address`. First-install only. |
| `allow_rollback` | default `false` | Lets a change of `installer` to `previous.installer` run the kept installer's `rollback`. Without it, that change is a plan error (and an apply error when `status --json` names it `previous` only at apply). |
| `deletion_protection` | default `true` | While true, a destroy is refused (`deletion_protection is on`). Set it `false` and apply first. |
| `delete_data` | default `false` | A destroy runs `uninstall --data` (the volumes deleted) instead of `uninstall` (kept). |
| `timeouts` | block | `create` (default 30m; it bounds the wait for a first install still `installing`), `read` (5m), `update` (30m), `delete` (15m). A run cut by its timeout is a failure. |

**`docker` block** — the Docker shape.

| Attribute | | |
|---|---|---|
| `host` | optional | The Docker host, reached by the system `ssh`. Left out, the install is on the machine running OpenTofu, and `user`, `port`, `private_key_file` and `known_hosts_line` are refused. |
| `port` | optional | SSH port (1–65535); left out, 22. |
| `user` | required with `host` | SSH user. A host or user starting with `-` or holding a space is refused. |
| `private_key_file` | optional | The private key's path; left out, ssh's agent and default keys. |
| `known_hosts_line` | required with `host` | The host's `known_hosts` line, `host keytype base64` (the host field may be left out, hashed or a pattern: it is replaced). That key alone is accepted. A marker (`@cert-authority`, `@revoked`), an unknown key type, a key that is not base64 or more than one line is refused. |
| `work_dir` | optional | The directory on the target holding `<name>.sh` and the install directory `<name>/`. Left out, the SSH user's home (`.`; locally, OpenTofu's working directory). First-install only (taking it out is a change too); empty is refused. |

**`kubernetes` block** — schema only (`kubeconfig`, `context`, `namespace` required, `release`):
a create is refused, `The Kubernetes target is not implemented yet`.

**`vm` block** — the Kairos VM shape: a VM its hypervisor's or cloud's own resource made from
`stowage_vm_seed`, reached over SSH. `installer` is refused with it (the VM's release is
`vm.version`; its disk carried the first). A destroy (with `deletion_protection` off) forgets the
resource and runs nothing: removing the VM is its own resource's job.

| Attribute | | |
|---|---|---|
| `host` | required | The VM's address (its resource's address output), reached by the system `ssh`. |
| `port`, `user`, `private_key_file` | | As the `docker` block's; `user` required (the seed's administrator, who runs `sudo -n` without a password). |
| `known_hosts_line` | required to install | The key the VM serves until the provider has read its own: `stowage_vm_seed`'s `known_hosts_line` (the one-time key) for a VM the provider installs, the VM's own key for an imported one. Checked as the `docker` block's. Left out, a create and an imported state's first apply are refused (`vm.known_hosts_line is required to install`); once `host_key_line` is known it may be left out. |
| `short_name` | required | `stowage_vm_seed`'s `short_name`: the VM runs `<short_name>-stow`, on its `PATH`. One path element (`[a-z0-9][a-z0-9._-]*`), else refused. First-install only (the VM's installer is its seed's, or the import ID's; an imported state is held to the ID's too): when the VM runs another installer (a release that renamed it, an import under a wrong name), give `vm.known_hosts_line` the VM's own key (`vm.host_key_line` from state while `vm.seed_boot_id` is null — with it set, reboot the VM first —, else the key checked on its console), then `tofu state rm` the resource and import it under the right name. |
| `version` | required | The release the VM runs. At create, the release the VM's OS carries (`stowage_vm_seed`'s `version`): another is refused before anything changes (`vm.version is not the VM's release`; a create never upgrades). A later change upgrades it, or rolls it back to `previous.version` with `allow_rollback` (below). |
| `channel` | optional | The channel the VM follows, which its seed's `channel` set. A create (once the install is done, before the VM's key is learnt, saving no state: the next apply goes on from the install) and an imported state's first apply (before anything runs) refuse one other than the VM's (`vm.channel is not the VM's`, naming it; a VM whose status reports no channel: leave it out), as state would record it. First-install only, but for an imported state never applied. |
| `host_key_line` | computed | The VM's own SSH host key, `<host> ssh-ed25519 <base64>`, read through the one-time key once its first install is done: every later connection accepts it, and it alone once the one-time key is retired. |
| `seed_boot_id` | computed | Set only when the one-time key's retirement failed: the boot the install finished in, during which the VM still serves that key — or `unread` when the VM reported no boot id, which no read clears. While it is set, a connection also accepts `known_hosts_line`, which a plan refuses to drop (`vm.known_hosts_line is still needed`). It goes null: at a read or an update whose status shows a later boot (which retired the key); after an upgrade's or a rollback's followed reboot; when an update's retirement, tried again, holds — an update without a release change tries it in its first steps, one with a release change whose upgrade or rollback had nothing to do (no reboot) tries it then, unless a retirement already failed earlier in the same apply. Each retry that fails is a warning (`The VM's one-time host key is still not retired`). From the moment it goes null, the update's connection accepts the VM's own key alone. |

`secrets_wo` is refused with the `vm` block (`secrets_wo on a VM`): a seed cannot make the VM's
first boot wait for a secrets export, so the boot installs with new secrets and an install's
re-run takes no `--secrets`. Make a VM again from an export with `<short>-stow vm create
--secrets FILE`, or install it without `secrets_wo` and `restore` a backup.

### Attributes read from the target

| Attribute | |
|---|---|
| `id` | `<work_dir>/<name>`; on a VM `vm:<host>/<short_name>-stow`. |
| `short_name` | The installer's short name (`describe` `short_name`): the install keeps its installer as `<id>/<short_name>-stow`. |
| `version`, `revision` | The release installed (`status --json` `installed`). |
| `channel` | The channel the install follows. |
| `status` | `installed`, `installing` (the first install has not finished), `failed: <why>` (`install_failed`), or `not installed`. |
| `previous` | `{version, revision, installer}`, the release `rollback` returns to; null before an upgrade. |
| `registry` | The provider's `registry` at the install; null in an imported state until its first apply. |

### What each operation runs on a Docker target

Every remote command is one `sh -c` script with its values as positional arguments, quoted for
the remote shell; nothing secret is ever an argument.

- **Create**: `docker run --rm <installer> > <work_dir>/<name>.sh` (written through a temporary
  file, so a failed fetch leaves no half script), the bootstrap script's `describe --json` (its
  `format` checked, its `name` compared, its shapes must include `docker` when it lists any, its
  `short_name` recorded), then its `status --json --dir ./<name>`: exit 3 (the installer refuses
  a directory with no install) is a fresh install; an install of another project is an error;
  an install of the project already
  there (a create that failed or timed out after the install began, or an install a destroy
  kept with its data) is a warning (`An install is already there`), and the install below runs
  without `--secrets` and without those of `--address`, `--registry` and `--registry-ca` that
  `describe` lists in `first_install_only`, which the installer's re-run refuses: the install
  keeps the ones it recorded, which state does not know (it records the configuration's, never
  compared with the install's); another release's install there is the installer's to refuse
  (its re-run runs the release it carries alone, exit 3). Then
  `sh <name>.sh --no-input --json --dir ./<name> [--set K=V]… [--tls] [--domains] [--ports]
  [--address] [--registry]`, with `--answers`, `--given`, `--secrets` and `--registry-ca` files
  sent on standard input as a tar into a private (0700) temporary directory removed afterwards;
  then `status --json`, read again every 10 seconds while it reports `installing` (the first
  install not finished) until it no longer does, within the create timeout. A failed or refused
  create, an `install_failed` and a timeout save no state: applying again runs the installer
  again on the install left, which finishes it (the installer refuses a run while another holds
  the install; a destroy cannot clean what state does not hold).
- **The kept installer** is `<work_dir>/<name>/<short_name>-stow` (`short_name` from `describe`),
  else, when that file is missing or not executable (a release that renamed its installer), the
  one `*-stow` executable there; none or two is an error. A `short_name` in state that is not one
  path element is an error naming the re-import. It runs with `--dir ./<name>` after its arguments
  (not `describe`), and with files on standard input as the install does.
- **Read**: the kept installer's `status --json`. A missing install directory removes the
  resource from state (the script exits 100 with `stowage-provider: no install directory` as
  the last line of stderr, after an SSH banner, and nothing on stdout; any other exit 100 is a
  failure). A release other than the configured one
  (`installed.installer`) replaces `installer` in state, so the next plan shows the change back.
  When `status` lists the recorded input keys, a key of `inputs` with a non-empty value it does not
  record is removed from state, so the next plan gives it again; `inputs` absent (the kept
  installer does not carry the installed release) is unknown and removes nothing. An imported
  remote state (no `known_hosts_line` yet) is not read: the configuration's connection reads it at
  the first apply.
- **Update**: a change of the provider's own settings alone (the connection,
  `deletion_protection`, `delete_data`, `allow_rollback`, `timeouts`) is saved without touching
  the target. Any other change, in this order:
  1. `status --json` and the kept installer's `describe --json` (the running release's inputs).
     No install directory is an error; a kept installer of another project too.
  2. The release to reach: none when `installed.installer` is the desired `installer`; `rollback`
     when it is `previous.installer` (with `allow_rollback`); else the new installer is fetched
     (`<name>.sh` written again, as at create) and described — another project, or no `docker`
     shape, is an error — and when it carries the release installed (the same `version` and
     `revision`) there is nothing to upgrade — an error when the install records another digest of
     its installer (`installer is another digest of the release installed`, naming the one to set:
     Read would put it back at every refresh) — else its `upgrade` runs (the installer refuses an
     older release: exit 3, the diagnostic).
  3. The **re-run** of the running release, when there is anything to give it: `<short>-stow
     --no-input --json [--set K=V]… [--tls] [--domains] [--ports] --dir ./<name>`, the secret
     inputs as `--answers` on standard input. It takes each input of `inputs` changed or added
     that the running release declares (every one without an upgrade), each input taken out that
     the running release declares (`--set KEY=`; with an upgrade, only one the new release still
     declares: the upgrade drops the others), the secret
     inputs the running release declares when `secret_inputs_wo_version` changed, the changed
     `tls`, the changed and added `domains`, and the changed `ports` (and added ones without an
     upgrade). When a release change follows, state is saved here: the release still the old one,
     `inputs` and `ports` as the re-run applied them. A failed or refused re-run changes no state.
  4. The **upgrade**: `sh <name>.sh upgrade --no-input --json --dir ./<name> [--set K=V]…
     [--ports]`, given the inputs the running release does not declare (those the new release
     adds; the installer refuses any other), the secret inputs it does not declare (`--answers`,
     sent with every upgrade), and the ports of routes state does not record (an imported state's go to the re-run). Or the
     **rollback**, which goes alone: a change of inputs, secret inputs, TLS mode, domains or ports
     with it is an error before anything runs (`a rollback goes alone`), and so is a rollback of an
     imported install never applied (`a rollback of an imported install`: apply first with the
     release it runs): `<short>-stow rollback --json --dir ./<name>`. A failure leaves step 3's state,
     so the next plan shows the release change alone.
  5. `status --json` fills the computed attributes; one that cannot be read keeps state's, with
     the error.
- **Import**: `tofu import stowage_install.x '[<host>:]<work_dir>/<name>/<short>-stow'`, the
  installer the install keeps (`ls <work_dir>/<name>/*-stow`); a host is what comes before the
  first colon when no slash does (an `@` is refused: the user comes from the configuration).
  State records the target, the directory and `short_name`, `deletion_protection = true`; the
  first apply, with the configuration's connection, runs the update above with every configured
  input, secret input (when `secret_inputs_wo_version` is set), `tls`, domain and port given to
  the re-run (the installer counts a value given as recorded as no change), and records the
  provider's `registry`. Its `address` is not compared until then.
- **Delete**: refused while `deletion_protection`; else the kept installer's `describe --json`
  (an installer without the machine interface stops there, before `uninstall` is given `--json`),
  then `<short>-stow uninstall --yes --no-input --json [--data] --dir ./<name>`. An install
  directory already gone is success, and runs nothing.

### What each operation runs on a VM target

`<short>-stow` is the VM's own (its OS bundle's, on its `PATH`): the reads run as the SSH user
(`<short>-stow describe --json`, `<short>-stow status --json`), the changes as root without a
password (`sudo -n <short>-stow install|upgrade|rollback … --json`). Files go on standard input
into a private (0700) temporary directory removed afterwards, as on Docker; nothing secret is an
argument.

- **Create** (every step under `vm.known_hosts_line`, the one-time key):
  1. The first boot, as `vm create` waits for it, tried again every 10 seconds within the create
     timeout (`the VM's first boot had not settled … when the timeout ran out`, with the last
     error): `describe --json` (ssh failing, exit 255, while the VM boots; no `<short>-stow` on the
     VM — any failure but ssh's, the VM's shell then not finding it (`command -v`) — is an error on
     `vm.short_name`, `The VM has no
     <short>-stow`, asking for `stowage_vm_seed`'s; another failure is an error at once), then `sudo -n systemctl start <name>.service`, the VM's boot unit, which
     returns once that boot did its part (loaded the release's images, made the first install or
     found it waiting, or wrote another release's OS and rebooted: ssh's exit 255, tried again) —
     so the provider's install never runs beside the boot's, which the install's lock would
     refuse; any other failure of the unit goes on to the install, which finishes what it left or
     says why not —, then `status --json` (tried again while it fails: the VM's daemon may answer
     after sshd), neither on the `recovery` system nor with `reboot_pending`, and `describe --json`
     again, the same release (else the OS changed under the boot: read again; ssh failing, tried
     again; another failure, an error at once). Its shapes must include `kairos` when it lists any.
  2. `vm.version` must be that release (`vm.version is not the VM's release`, naming it): a create
     installs the release the VM's OS carries and never upgrades, as an upgrade failing there
     would leave a created object OpenTofu replaces. An input given a non-empty value that
     `describe` marks `secret` is refused. Neither runs anything.
  3. `sudo -n <short>-stow install --no-input --json [--set K=V]… [--tls] [--domains] [--ports]`,
     with `secret_inputs_wo` as `--answers` and `given_wo` as `--given` on standard input: the
     first install (from the settings the seed's cloud-config wrote), or the re-run of the one the
     first boot made — the installer decides, and refuses what a re-run does not take (exit 3,
     the diagnostic).
  4. `status --json` until it no longer reports `installing` (a read that fails is tried again);
     an `install_failed`, or no release installed, is an error saving no state (apply again: the
     install runs again and finishes). Then `vm.channel`, when given, must be the channel the VM
     follows (the `vm` block's `channel` row), else an error saving no state.
  5. `cat /etc/ssh/ssh_host_ed25519_key.pub` through the one-time key, which vouches for it:
     `vm.host_key_line`. A failure is a warning (`The VM's own host key was not read`): the
     connection keeps accepting `known_hosts_line` alone, which the VM's next boot retires.
  6. The one-time key retired as `vm create` retires it, by the files Stowage's README names:
     `sudo -n sh -c '…' sh <name>` removes `/run/<name>-seed-host-key` and
     `/etc/ssh/sshd_config.d/01-<name>-seed-host-key.conf`, touches
     `/oem/<name>-seed-host-key.retired` and reloads sshd; then a connection accepting the VM's own
     key alone reads it again, tried for six polls, a minute (`… when its six polls (a minute) ran
     out`). A failure is a warning (`The VM's
     one-time host key was not retired`), and `vm.seed_boot_id` is set: the one-time key is
     accepted beside the VM's own until a later boot.
- **Read**: `status --json`; one that finds no `<short>-stow` on the VM (a release that renamed its installer) names the route to take the install over, as an update's read does: the VM's own key in `vm.known_hosts_line` (state's `vm.host_key_line`; with `vm.seed_boot_id` set, reboot the VM first, which retires its one-time key; else the key checked on its console), then `tofu state rm` and `tofu import` under the new name. It counts as missing when the run fails (any exit but ssh's 255: shells give a missing command 127, tcsh 1) and the VM's shell does not find `<short>-stow` either (`sh -c 'command -v <short>-stow'`, whatever the shell's language): a program the installer runs missing is not it. A check that itself fails is said (`could not be checked`). The first boot's wait stops at once on it too. No release installed and no first install running or failed removes
  the resource from state (the next apply installs again). The release the VM runs replaces
  `vm.version` in state, so a VM whose health gate fell back shows its real release and the next
  plan proposes the upgrade again; `inputs` are dropped as on Docker. An imported state (no
  `known_hosts_line` yet) is not read.
- **Update**: a change of the provider's own settings alone is saved without touching the VM.
  Otherwise, in this order:
  1. `status --json` (no release installed is an error; no `<short>-stow` on the VM is the Read
     step's rename route) and `describe --json` (the running release).
     The refusals that need no change come first: a rollback without `allow_rollback`, a secret
     input in `inputs`, a rollback with other changes, an imported state's `vm.channel` other than
     the VM's (`vm.channel is not the VM's`), and a release change on a status without `boot_id`
     (`The VM reports no boot_id`: its reboot could not be followed). Then the keys,
     before anything else changes on the VM: when state has no `vm.host_key_line` yet (it could
     not be read at create; an imported state), the VM's own key is learnt under the
     configuration's key as at create (steps 5 and 6: read, the one-time key retired and the
     retirement proved, else `vm.seed_boot_id` set); when `vm.seed_boot_id` is set (a retirement
     that failed), it is null at once when the status shows a later boot (which retired the key),
     left to the reboot of a release change, and else tried again — null on success, a warning
     (`The VM's one-time host key is still not retired`) on failure. Either is saved at once, with
     the configuration's connection, whatever fails after it. A release change whose own key could not be read is an error
     before anything else runs (`The VM's own host key is not known`): its reboot would retire
     the key the connection accepts.
  2. The release to reach: none when the plan does not change `vm.version` (a VM that runs
     another release than state says — a refresh skipped — is the next refresh's to show), nor
     when `installed.version` is `vm.version`; `rollback` when it is `previous.version` (with
     `allow_rollback`, else an error); else `upgrade --version`.
  3. The **re-run**, `sudo -n <short>-stow install --no-input --json …`, when there is anything to
     give: the changed and added inputs the running release declares (every one without an
     upgrade), the inputs taken out that it declares (`--set KEY=`), the secret inputs on a new
     `secret_inputs_wo_version`, the changed `tls`, and the changed `domains` and `ports` (added
     ones too without an upgrade). When a release change follows, state is saved here, the release
     unchanged.
  4. The **upgrade**: `sudo -n <short>-stow upgrade --version <vm.version> --yes --json`, which
     ends as it schedules the VM's reboot (an ssh cut with no result, exit 255, is taken as that
     reboot beginning); or the **rollback**, `sudo -n <short>-stow rollback --yes --json`, alone (a
     change of inputs, secret inputs, TLS mode, domains or ports with it is an error, `a rollback
     goes alone`). The VM's upgrade takes no input.
  5. The reboot followed: `status --json` every 10 seconds (ssh failing meanwhile) until its
     `boot_id` is another than before and `on_trial` is false (the health gate judged the new
     boot), within the update timeout. A status without `boot_id` is an error (the reboot cannot
     be followed). When the VM then runs another release than `vm.version`, or its `failed_boot`
     is a new one of that release (a boot the gate kept, as going back was refused), it is an
     error naming the gate's reason — `The VM's health gate found <v>'s boot unhealthy: <reason>`,
     then that it went back to the release it runs, or the kept boot's reason — and state saves
     the release the VM runs, so the next plan proposes the change again.
  6. After an upgrade, `describe --json` of the new release, then the re-run again with what it
     adds: the inputs and secret inputs the old release did not declare, and the `domains` and
     `ports` of routes state does not record. An input given a non-empty value the new release
     marks `secret` is refused there, before the re-run (state saves the release the VM runs, and
     not that input). Then `status --json` fills the computed attributes (one that cannot be read
     keeps the status read after the reboot, with the error); `seed_boot_id` is null (the reboot
     retired the one-time key). After the reboot was followed, an error — the new release's
     describe, its re-run — saves the release the VM runs and the inputs applied; a reboot not
     followed in time leaves state as step 3 saved it (the release before the upgrade), the next
     refresh reading the one the VM runs.
- **Import**: `tofu import stowage_install.x 'vm:<host>/<short>-stow'` (`ls /usr/sbin/*-stow`
  on the VM); give `vm.known_hosts_line` the VM's own key. State records `vm.host`,
  `vm.short_name` and `deletion_protection = true`; the first apply runs the update above with
  every configured input, as on Docker.
- **Delete**: refused while `deletion_protection`; else nothing runs: the resource is forgotten.

### Plan-time errors

A change of a first-install-only setting — `docker.work_dir`, `address`, `vm.channel`, `vm.short_name`, the
provider's `registry`, the target block's kind, or the project an `installer` names (an imported
state: its install directory's name) — is a plan error naming the old value, never a replacement:
replacing an install deletes its data.

Also plan errors: `installer` changed to `previous.installer`, or `vm.version` to
`previous.version`, without `allow_rollback` (`a rollback needs allow_rollback`); `vm.known_hosts_line`
left out while `vm.seed_boot_id` is set (`vm.known_hosts_line is still needed`); `tls` taken out (`tls cannot be removed`: a re-run has no flag
for loopback mode); a route taken out of `domains` or `ports` (`domains.<route> cannot be
removed`); a rollback with any other change of what the target applies (`a rollback goes
alone`); a route added to `domains` while `installer` changes (known at plan time) (`a domain added with a release
change`: a route the new release adds takes its domain from its domain input, in `inputs`; one
the running release has takes it in an apply of its own). A plan changing anything the target applies plans `status` unknown; one changing
`installer` also `version`, `revision`, `channel`, `previous` and `short_name`; one changing
`vm.version` also `version`, `revision`, `channel` and `previous`. On a VM, one changing anything
it applies plans those unknown too (the update reads the release after its run), and
`vm.seed_boot_id` unknown while it is set, and `vm.host_key_line` and
`vm.seed_boot_id` unknown while state has no `vm.host_key_line` (the update learns it). An imported
state's first apply also plans `registry` unknown (it records the provider's).

### The machine interface, as read

- A verb (install and re-run, `upgrade`, `rollback`, `uninstall`) runs with `--json`: its
  standard output is one object, `{format, result, release, refused}`; its text goes to standard
  error. Every verb runs after a `describe --json` of the installer it runs (the fetched one, or
  the kept one), so an installer without the machine interface is stopped there: a `describe`
  exiting 1 (cobra's unknown command, as such an installer answers) says that an installer made
  by Stowage before 0.34.0 has none; another exit says nothing of the installer's age.
- The installer's exit status:
  - **0** with the object, `result` `done` or `nothing`, no `refused`: success. State is then what
    `status --json` reads (`nothing` — the install already runs that release — changes nothing).
  - **3** with the object, `result` `nothing` and `refused.reason` set: refused before anything
    changed. The diagnostic's summary is `refused.reason`; its detail is `refused.fix` (advice for
    you: the provider shows it and never runs it), then `<step> was refused by the installer
    (<verb>, exit 3) before anything changed on the target`. That step saves no state (a refused
    upgrade after a re-run keeps the re-run's state, as a failed one does).
  - any other exit: failed, whatever standard output holds (a failure prints no object), with
    the end of its error output (4 KiB). Exit 255 is noted as possibly ssh's own failure.
  - Exit 0 or 3 without exactly that object — none, text, data before or after it, a `result`
    other than `done`/`nothing`, a refusal with exit 0, exit 3 without a refusal or with `result`
    `done`, a `release` without `version` — is a failure naming the breach (`the installer breaks
    the machine interface`), never a refusal or a success.
- **Unknown fields are tolerated** (an installer adds fields without moving `format`); a known
  field of the wrong type, data after the object, a missing `name` or `version`, and a
  `describe` `short_name` that is not one path element (`[a-z0-9][a-z0-9._-]*`) are errors.
- **An unknown `format` is refused**: missing from `describe`, `status --json` or a verb's result
  (an installer older than Stowage 0.34.0), newer than 1 (upgrade the provider), or any other.

### SSH

The system `ssh` runs with `BatchMode=yes`, `StrictHostKeyChecking=yes`, a private 0600
`known_hosts` file (in a 0700 temporary directory, removed after the run) holding the given key
(on a VM, the keys it may serve: see `vm.seed_boot_id`) under `HostKeyAlias=stowage-target`, `GlobalKnownHostsFile=/dev/null`, `KnownHostsCommand=none`,
`CheckHostIP=no`, `UpdateHostKeys=no`, `VerifyHostKeyDNS=no` (no SSHFP record trusted),
`ControlMaster=no` and `ControlPath=none` (no connection another ssh opened is reused),
`ConnectTimeout=30`, and with a key file `IdentitiesOnly=yes`. The user's `~/.ssh/config` still
applies to everything else (a `ProxyJump`, for one). Output is kept up to 16 MiB per stream.

A run cut by its timeout kills the local `ssh` (or, on this machine, the shell), then waits at most
10 seconds for its output to close; so does a program that exited while one it started holds
the output, which keeps its exit status. It does not stop the program on the target: an install or
upgrade cut there goes on, and the installer refuses another run until it ends.

## Resource `stowage_vm_seed`

A new Kairos VM's first-boot material: `vm seed --json` of the release an installer carries, run
once on the machine running OpenTofu. Its one-time SSH host key would change at every plan from a
data source, so it is a managed resource **generated once**: every argument forces a new seed
(and so a new VM, whose resource reads `user_data`). Read and destroy run nothing.

**Create**: `docker run --rm <installer> > <name>.sh` in a private temporary directory removed
afterwards (logged in with the provider's `username`/`password` as on a Docker target), its
`describe --json` (`name` compared with the image's, shapes must include `kairos` when it lists
any), then `sh <name>.sh vm seed --json --platform=<platform> [--flag=value]… [--set K=V]…
[--registry=<provider registry>]`, the SSH key and the provider's `registry_ca` as `--ssh-key` and
`--registry-ca` files on standard input. The installer refuses (exit 3, the diagnostic's summary
its error) a `platform` the registry holds no disk for, naming those it offers, and a secret input
in `inputs`; it fails (exit 1) when the registry cannot be read.

| Argument | | |
|---|---|---|
| `installer` | required | The installer by digest, as `stowage_install`'s. |
| `platform` | required | `--platform`: `truenas`, `proxmox`, `libvirt`, `vmware`, `hyperv`, `azure`, `gcp`, `aws`, `scaleway` or `metal`. |
| `hostname`, `user`, `channel` | optional | `--hostname`, `--user` (the administrator, who logs in over SSH and runs `sudo`), `--channel` (the first install's). |
| `ssh_public_key` | optional | The administrator's public key, e.g. `file("~/.ssh/id_ed25519.pub")` (`--ssh-key`, as a file). Left out, the installer's default key of the machine running OpenTofu. |
| `network`, `address`, `gateway`, `dns` | optional | `--network dhcp\|static`, its static address, gateway and DNS servers. |
| `ipv6`, `address6`, `gateway6` | optional | `--ipv6`, `--address6`, `--gateway6`. |
| `tune_profile` | optional | `--tune-profile`: `default` or `network`. |
| `ssh_allow` | optional list | `--ssh-allow`, one CIDR each (a comma or space is refused). |
| `timezone`, `device` | optional | `--timezone`; `--device` (`metal`). |
| `add_hosts`, `interfaces` | optional maps | `--add-host NAME=IP`, `--interface ROLE=SPEC`. |
| `tls` | optional | `--tls`: `acme`, `dns-cloudflare` or `internal`. `byo` is refused at plan time (`a seed carries no certificate`): seed with another mode, and give the certificate in `stowage_install`'s `given_wo` (its `tls` section), which the VM's first `install --given` takes, moving the front to `byo`. |
| `domains` | optional map | `--domains route=host,…`. |
| `http_port`, `https_port` | optional | `--http-port`, `--https-port` (1–65535). |
| `inputs` | optional map | Non-secret inputs, `--set KEY=VALUE`: in the cloud-config and in state. A secret input goes to `stowage_install`'s `secret_inputs_wo`, given over SSH once the VM answers. |
| `timeouts` | block | `create` (default 10m). |

| Attribute | |
|---|---|
| `id`, `fingerprint` | The one-time host key's SHA256 fingerprint, as ssh prints it. |
| `name`, `short_name` | The project's name; the installer's short name, for `stowage_install`'s `vm.short_name`. |
| `version`, `revision` | The release the seed's disk boots first. |
| `user_data` | **sensitive** — the cloud-config for `platform` (a NoCloud seed's `user-data`, or a cloud's user data): no secret input, but the one-time host key's private key. |
| `known_hosts_line` | The one-time key as a `known_hosts` line under `vm-seed-host-key`, for `stowage_install`'s `vm.known_hosts_line`. |
| `host_key_private` | **sensitive** — the one-time key's OpenSSH private key, which the VM serves until its install and the reboot after it. |
| `disk` | Map, per platform the registry holds a disk for, of `{format, image}`: the disk it boots, by digest. |
| `waiting` | The required secret inputs nothing gives, sorted: the VM's first install waits for them (`secret_inputs_wo`). |

The private key is in state (in `user_data` and `host_key_private`) until it retires: encrypt
state. What the seed does not give a hypervisor yet — a NoCloud `meta-data`, the disk as a file,
the size the OS takes of the disk — is in [TODO.md](TODO.md); the
[TrueNAS example](examples/resources/stowage_vm_seed/resource.tf) runs Stowage's own steps for it.

## Build

Docker is the only requirement.

```bash
make check   # GATE: gofmt, go vet, staticcheck, govulncheck, the hooks' test, go test -race (Dockerfile target checks)
make build   # ./bin/terraform-provider-stowage, only after green checks
make test    # fast loop, never the gate
make docs    # regenerate docs/ with tfplugindocs (needs network)
make tidy    # go.mod / go.sum
make fmt     # gofmt -w
make shell   # toolchain container
make hooks   # commit identity + .githooks
make hooks-test  # the hooks' test (test/hooks.sh) on this machine; make check runs it too
make release-check # goreleaser check + a snapshot release (zips, manifest, checksums; unsigned) in dist/, in Docker
```

An acceptance test, `TestAccDockerLocal`, installs a real installer on this machine's Docker
through OpenTofu: `TF_ACC=1 TF_ACC_TERRAFORM_PATH=<tofu> TF_ACC_PROVIDER_NAMESPACE=norvexlabs
TF_ACC_PROVIDER_HOST=registry.opentofu.org STOWAGE_ACC_INSTALLER=<ref@digest>
STOWAGE_ACC_WORK_DIR=<dir> go test ./internal/provider -run TestAcc`.

CI (`.github/workflows/ci.yml`) runs `make check`, `make build` and `make release-check`
(GoReleaser's `check` and a snapshot release into `dist/` — every zip, the manifest and the
checksums, unsigned and unpublished — in
GoReleaser v2.19.0's image with the Go of `GO_VERSION`) on every push to `main` and every pull
request.

## Release

For maintainers. Only a pushed `v*` tag releases (`.github/workflows/release.yml`); a push to
`main` never does.

**Once, on GitHub:** create the environment `release` (Settings → Environments) with a deployment
rule that admits tags `v*` only (no branch), and put the secrets `GPG_PRIVATE_KEY` (the release
key's armored private key) and `PASSPHRASE` (its passphrase) there — environment secrets, never
repository secrets, so a workflow run on a branch cannot read them.

1. A commit dates the top CHANGELOG.md entry: `## [x.y.z]` → `## [x.y.z] - YYYY-MM-DD`. The
   Makefile's `VERSION` is that entry's version.
2. `git tag vx.y.z` on that commit of `main`, pushed with `main`.
3. The **build** job (read-only token) refuses a tag that is not a version
   (`vX.Y.Z` or `vX.Y.Z-<pre-release>`), is not on `main`, does not name the top entry's version,
   or whose entry is undated; then runs `make check` and `make release-check`.
4. The **publish** job (environment `release`, `contents: write`) refuses a tag that no longer
   names the commit its run was started for (a tag moved to a fix: the newer run publishes), and
   a tag whose release is already published (a draft left by a failed run is replaced). It imports
   the release key from `GPG_PRIVATE_KEY` (refused unless it is the key below, and unless
   `keys/norvexlabs-gpg-public.asc` is its public half), and runs GoReleaser (`.goreleaser.yml`,
   GoReleaser v2.19.0, `GORELEASER_CURRENT_TAG` the pushed tag) with the Go of `GO_VERSION`.
   GoReleaser makes a draft release, its notes the tag's CHANGELOG.md entry, holding what the
   OpenTofu and Terraform registries read:
   - `terraform-provider-stowage_<version>_<os>_<arch>.zip`, each holding
     `terraform-provider-stowage_v<version>`, for freebsd and linux on amd64, 386, arm and arm64,
     windows on amd64, 386 and arm64, and darwin on amd64 and arm64 (13 zips);
   - `terraform-provider-stowage_<version>_manifest.json`, from `terraform-registry-manifest.json`
     (plugin protocol 6.0);
   - `terraform-provider-stowage_<version>_SHA256SUMS` over the zips and the manifest, and
     `terraform-provider-stowage_<version>_SHA256SUMS.sig`, its binary detached GPG signature by
     the release key, its passphrase from `PASSPHRASE`.

   The job then verifies the signature with `keys/norvexlabs-gpg-public.asc`, the manifest's and
   every zip's checksum, checks again that the tag still names its commit, and publishes the draft.
   The release is made by `github-actions` with the workflow's `GITHUB_TOKEN`; nothing is
   published as a person.

The release key: fingerprint `686D3351040F9F4463D9D3AC0C705E9BAA4F4981`, RSA 4096, `Norvex Labs
<norvexlabs@gmail.com>`. Its public key is in this repository,
[`keys/norvexlabs-gpg-public.asc`](keys/norvexlabs-gpg-public.asc), and is the one the registries
are given. To check a release by hand:

```bash
gpg --import keys/norvexlabs-gpg-public.asc
gpg --verify terraform-provider-stowage_<version>_SHA256SUMS.sig terraform-provider-stowage_<version>_SHA256SUMS
sha256sum --check --ignore-missing terraform-provider-stowage_<version>_SHA256SUMS
```

## Repository

| Path | |
|---|---|
| `main.go` | the plugin server |
| `internal/provider` | provider configuration and `stowage_install` |
| `internal/installer` | the machine interface's documents, exit mapping, the Docker target's scripts |
| `internal/target` | `Runner`: this machine, or the system `ssh` with a pinned host key |
| `examples/` | provider and resource examples (read by tfplugindocs) |
| `.goreleaser.yml`, `terraform-registry-manifest.json` | the release's zips, checksums, signature and registry manifest |
| `.github/workflows/` | `ci.yml` (check and build), `release.yml` (a pushed tag's release) |
| `.githooks/` | the commit identity, push and private-words guards (`make hooks`) |
| `test/hooks.sh` | the hooks' test |
| `keys/` | the release key's public half |
| `docs/img/` | the logo, mark, icons and the repository's social preview |

Licence: Apache-2.0.
