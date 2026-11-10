# terraform-provider-stowage

The OpenTofu provider for [Stowage](https://github.com/norvexlabs/stowage): it declares a Stowage
product's install beside the VM, cloud or cluster it runs on, in one OpenTofu root. It reads only
the installer's machine interface (`describe --json`, `status --json`, the verbs' `--json` result
and exit status): it decides nothing the installer does not decide, orders no version and keeps no
copy of a rule.

**Status: pre-release (0.1.0, unreleased).** What works: the Docker target — create, read,
update (inputs, then the release: upgrade or rollback), import and delete. Not yet: the plan-time
dry run, the Kubernetes and VM targets, `stowage_vm_seed` — see [TODO.md](TODO.md). Not yet
proven against a real installer (unit tests drive the verbs' results as Stowage prints them).

- Provider address: `registry.opentofu.org/norvexlabs/stowage`, plugin protocol 6.
- Needs **OpenTofu ≥ 1.11** (write-only attributes), and for a remote target the system `ssh`
  binary, OpenSSH ≥ 8.5 (`KnownHostsCommand`), on the machine running OpenTofu. The target needs
  a POSIX `sh`, `tar`, `mktemp` and Docker.
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
| `secrets_wo` | optional, **write-only** | A decrypted `secrets export` of a machine made again (`--secrets`), used at create, on standard input. |
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

**`vm` block** — schema only (`host` required, `port`, `user` and `known_hosts_line` required,
`private_key_file`, `version` required, `channel` first-install only): a create is refused, `The VM
target is not implemented yet`. A destroy (with `deletion_protection` off) forgets the resource:
removing the VM is its hypervisor resource's job.

### Attributes read from the target

| Attribute | |
|---|---|
| `id` | `<work_dir>/<name>`. |
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

### Plan-time errors

A change of a first-install-only setting — `docker.work_dir`, `address`, `vm.channel`, the
provider's `registry`, the target block's kind, or the project an `installer` names (an imported
state: its install directory's name) — is a plan error naming the old value, never a replacement:
replacing an install deletes its data.

Also plan errors: `installer` changed to `previous.installer` without `allow_rollback` (`a
rollback needs allow_rollback`); `tls` taken out (`tls cannot be removed`: a re-run has no flag
for loopback mode); a route taken out of `domains` or `ports` (`domains.<route> cannot be
removed`); a rollback with any other change of what the target applies (`a rollback goes
alone`); a route added to `domains` while `installer` changes (known at plan time) (`a domain added with a release
change`: a route the new release adds takes its domain from its domain input, in `inputs`; one
the running release has takes it in an apply of its own). A plan changing anything the target applies plans `status` unknown; one changing
`installer` also `version`, `revision`, `channel`, `previous` and `short_name`.

### The machine interface, as read

- A verb (install and re-run, `upgrade`, `rollback`, `uninstall`) runs with `--json`: its
  standard output is one object, `{format, result, release, refused}`; its text goes to standard
  error. Every verb runs after a `describe --json` of the installer it runs (the fetched one, or
  the kept one), so an installer without the machine interface is stopped there: a failed
  `describe` says that an installer made by Stowage before 0.34.0 has none.
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
under `HostKeyAlias=stowage-target`, `GlobalKnownHostsFile=/dev/null`, `KnownHostsCommand=none`,
`CheckHostIP=no`, `UpdateHostKeys=no`, `VerifyHostKeyDNS=no` (no SSHFP record trusted),
`ControlMaster=no` and `ControlPath=none` (no connection another ssh opened is reused),
`ConnectTimeout=30`, and with a key file `IdentitiesOnly=yes`. The user's `~/.ssh/config` still
applies to everything else (a `ProxyJump`, for one). Output is kept up to 16 MiB per stream.

A run cut by its timeout kills the local `ssh` (or, on this machine, the shell), then waits at most
10 seconds for its output to close; so does a program that exited while one it started holds
the output, which keeps its exit status. It does not stop the program on the target: an install or
upgrade cut there goes on, and the installer refuses another run until it ends.

## Build

Docker is the only requirement.

```bash
make check   # GATE: gofmt, go vet, staticcheck, govulncheck, go test -race (Dockerfile target checks)
make build   # ./bin/terraform-provider-stowage, only after green checks
make test    # fast loop, never the gate
make docs    # regenerate docs/ with tfplugindocs (needs network)
make tidy    # go.mod / go.sum
make fmt     # gofmt -w
make shell   # toolchain container
make hooks   # commit identity + .githooks
```

An acceptance test, `TestAccDockerLocal`, installs a real installer on this machine's Docker
through OpenTofu: `TF_ACC=1 TF_ACC_TERRAFORM_PATH=<tofu> TF_ACC_PROVIDER_NAMESPACE=norvexlabs
TF_ACC_PROVIDER_HOST=registry.opentofu.org STOWAGE_ACC_INSTALLER=<ref@digest>
STOWAGE_ACC_WORK_DIR=<dir> go test ./internal/provider -run TestAcc`.

Releases: GoReleaser (`.goreleaser.yml`): zips `terraform-provider-stowage_<version>_<os>_<arch>`,
`…_SHA256SUMS` with its detached GPG signature (`GPG_FINGERPRINT`), and the registry manifest
(protocol 6.0).

## Repository

| Path | |
|---|---|
| `main.go` | the plugin server |
| `internal/provider` | provider configuration and `stowage_install` |
| `internal/installer` | the machine interface's documents, exit mapping, the Docker target's scripts |
| `internal/target` | `Runner`: this machine, or the system `ssh` with a pinned host key |
| `examples/` | provider and resource examples (read by tfplugindocs) |

Licence: Apache-2.0.
