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

**Status: pre-release (0.1.0, unreleased).** What works: the Docker and Kubernetes targets —
create, read, update (inputs, then the release: upgrade or rollback), import and delete. Not yet:
the plan-time dry run, the VM target, `stowage_vm_seed` — see [TODO.md](TODO.md). The Docker
target is proven against a real installer over SSH (`make acc-docker`), the Kubernetes target on
a kind cluster (`make acc-k8s`).

- Provider address: `registry.opentofu.org/norvexlabs/stowage`, plugin protocol 6.
- Needs **OpenTofu ≥ 1.11** (write-only attributes), and for a remote target the system `ssh`
  binary, OpenSSH ≥ 8.5 (`KnownHostsCommand`), on the machine running OpenTofu. The target needs
  a POSIX `sh`, `tar`, `mktemp` and Docker.
- The Kubernetes target runs the installer on the machine running OpenTofu, which must be
  **linux/amd64** (installers are built for it) with `docker` (to fetch the installer image),
  `helm` ≥ 3.14 (the installer checks the version) and `kubectl` on `PATH`.
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

On a cluster another resource of the root creates, reached as the helm provider reaches it:

```hcl
provider "stowage" {
  kubernetes {
    host                   = aws_eks_cluster.this.endpoint
    cluster_ca_certificate = base64decode(aws_eks_cluster.this.certificate_authority[0].data)
    exec {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = "aws"
      args        = ["eks", "get-token", "--cluster-name", aws_eks_cluster.this.name]
    }
  }
}

resource "stowage_install" "notes_k8s" {
  installer = "registry.example.com/notes/notes-installer@sha256:<64 hex>"
  inputs    = { ADMIN_EMAIL = "ops@example.com" }
  kubernetes {
    namespace = "notes"
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
| `kubernetes` | block | The cluster the Kubernetes targets reach, below. |

On Kubernetes, `registry` is the installer's `--image-registry`: the chart and the images come from
the mirror, for which the installer writes no pull Secret. The cluster's nodes must pull from it on
their own (an anonymous mirror, or the container runtime's credentials on each node): the provider
gives the chart no values file.

**`kubernetes` block** — how the cluster is reached, named as the helm provider's `kubernetes`
block. A provider's configuration is read at every run and never written to state: its
credentials are not in state, and a short-lived token (`exec`) is fetched again each run. With
`host`, a kubeconfig of these values is written for each run (one cluster, user and context named
`stowage`; PEM values as their `*-data`; 0600, in a private 0700 temporary directory removed when
the run ends); with `config_path`, that file; with neither (or no block), kubectl's default:
`KUBECONFIG` (each file made absolute), else `~/.kube/config`. A value not known yet (a cluster
made in the same apply) is checked, and used, when the apply configures the provider again; an
install run while one is still unknown is an error (`The provider's kubernetes block is not known
yet`).

| Attribute | | |
|---|---|---|
| `host` | optional | The API server's URL (`http://` or `https://`, else refused). Refused with `config_path` or `config_context`; it needs a credential: `token`, `client_certificate` with `client_key`, or `exec` (`no credential for host`). |
| `token` | optional, sensitive | A bearer token. |
| `cluster_ca_certificate` | optional | PEM of the API server's certificate authority. |
| `client_certificate`, `client_key` | optional, the key sensitive | PEM of a client certificate and its key, together (one alone is refused). |
| `insecure` | optional | `true` skips the API server certificate's check; refused with `cluster_ca_certificate`. |
| `tls_server_name`, `proxy_url` | optional | The name the server's certificate is checked against; a proxy to the API server. |
| `exec` | block | A credential plugin (`api_version` and `command` required, `args`, `env` sensitive) kubectl and helm run for a token, never interactively (`interactiveMode: Never`). |
| `config_path` | optional | A kubeconfig file on the machine running OpenTofu (a leading `~/` is the home directory; made absolute). Refused with `host`. |
| `config_context` | optional | Its context, given as `--kube-context` (of `config_path`'s file, or of kubectl's default). Refused with `host`. |

`token`, `cluster_ca_certificate`, the client certificate, `insecure = true`, `tls_server_name`,
`proxy_url` and `exec` without `host` are refused (`<name> without host`).

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
| `ports` | optional map of numbers | Route → port (1–65535; checked again at apply, for a port computed from another resource), `--ports route=port,…`. A changed port is given to the re-run; a route state does not record goes to the upgrade's `--ports` when the release changes (taken as a route the release adds: set the port of a route the running release has in an apply of its own), else to the re-run; taking one out is a plan error. |
| `address` | optional | The loopback address the routes bind, `--address`. First-install only. |
| `allow_rollback` | default `false` | Lets a change of `installer` to `previous.installer` run the kept installer's `rollback`. Without it, that change is a plan error (and an apply error when `status --json` names it `previous` only at apply). |
| `deletion_protection` | default `true` | While true, a destroy is refused (`deletion_protection is on`). Set it `false` and apply first. |
| `delete_data` | default `false` | A destroy runs `uninstall --data` (the volumes deleted) instead of `uninstall` (kept). |
| `timeouts` | block | `create` (default 30m; it bounds the wait for a first install still `installing`), `read` (5m), `update` (30m), `delete` (15m). A run cut by its timeout is a failure. |

A null value in `inputs`, `secret_inputs_wo`, `domains` or `ports` is refused by its key
(`<attribute>.<key> is null`); a value not known until apply (an ephemeral variable's, one computed
from another resource) is not, its key checked when the configuration is validated.

**`docker` block** — the Docker shape.

| Attribute | | |
|---|---|---|
| `host` | optional | The Docker host, reached by the system `ssh`. Left out, the install is on the machine running OpenTofu, and `user`, `port`, `private_key_file` and `known_hosts_line` are refused. |
| `port` | optional | SSH port (1–65535); left out, 22. |
| `user` | required with `host` | SSH user. A host or user starting with `-` or holding a space is refused. |
| `private_key_file` | optional | The private key's path; left out, ssh's agent and default keys. |
| `known_hosts_line` | required with `host` | The host's `known_hosts` line, `host keytype base64` (the host field may be left out, hashed or a pattern: it is replaced). That key alone is accepted. A marker (`@cert-authority`, `@revoked`), an unknown key type, a key that is not base64 or more than one line is refused. |
| `work_dir` | optional | The directory on the target holding `<name>.sh` and the install directory `<name>/`. Left out, the SSH user's home (`.`; locally, OpenTofu's working directory). First-install only (taking it out is a change too); empty is refused. |

**`kubernetes` block** — the Kubernetes shape, on the cluster the provider's `kubernetes` block
reaches (one cluster per provider configuration: an alias per cluster). Not with the front's
settings: `tls`, `domains`, `ports`, `address` and `secrets_wo` are refused (`<name> is not
Kubernetes's`: the chart's ingress owns TLS and routes, and the release keeps its secrets in the
cluster). On the target, `id` is `<namespace>/<release>`.

| Attribute | | |
|---|---|---|
| `namespace` | required | The release's namespace (`--namespace`), a DNS label (lower-case letters, digits and `-`, at most 63 characters; else refused). First-install only. |
| `release` | optional | The helm release's name (`--release`), a DNS label of at most 53 characters; left out, the project's name, the installer's default. First-install only (the effective name is compared: naming the default changes nothing). |

**`vm` block** — schema only (`host` required, `port`, `user` and `known_hosts_line` required,
`private_key_file`, `version` required, `channel` first-install only): a create is refused, `The VM
target is not implemented yet`. A destroy (with `deletion_protection` off) forgets the resource:
removing the VM is its hypervisor resource's job.

### Attributes read from the target

| Attribute | |
|---|---|
| `id` | `<work_dir>/<name>` on Docker; `<namespace>/<release>` on Kubernetes. |
| `short_name` | The installer's short name (`describe` `short_name`): a Docker install keeps its installer as `<id>/<short_name>-stow`. |
| `version`, `revision` | The release installed (`status --json` `installed`). |
| `channel` | The channel the install follows. |
| `status` | `installed`, `installing` (the first install has not finished; helm's `pending-install` on Kubernetes), `failed: <why>` (`install_failed` on Docker; a latest helm revision neither deployed nor pending on Kubernetes), `pending: <why>` (Kubernetes: helm working on the release), or `not installed`. |
| `previous` | `{version, revision, installer}`, the release `rollback` returns to; null before an upgrade. |
| `registry` | The provider's `registry` at the install; null in an imported state until its first apply. |

### What each operation runs on a Docker target

Every remote command is one `sh -c` script with its values as positional arguments, quoted for
the remote shell; nothing secret is ever an argument. On the machine running OpenTofu (no `host`)
the installer runs with OpenTofu's environment but for the variables named as an input the
installer declares (those it reads before an answers file), each left out of the program's
environment before it starts — but those the run
needs itself, the Kubernetes target's list below; an input of one of those names while the
environment sets it is refused, a key of `given_wo`'s `inputs` included (`<KEY> comes from the
environment`). The scripts' own variables are named `__stowage_*`, so an exported variable of
another name reaches the installer unchanged. Over ssh no environment is carried.

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
  list is removed from state, so the next plan gives it again — when the running release declares
  it: `status` lists the recorded inputs among those the running release declares, so the kept
  installer's `describe --json` is read then (only when a key would go), and a key it does not
  declare (after a rollback, an input only the newer release declares, which the install keeps)
  stays; a `describe` that fails, finds the install gone or carries another release than `status`
  reports installed removes nothing, with a warning. `inputs` absent (the kept
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

### What each operation runs on a Kubernetes target

Everything runs on the machine running OpenTofu, in a private (0700) temporary directory removed
when the operation ends, which holds the kubeconfig (when `host` gives it) and each installer
fetched. Every run is one `sh -c` script, `KUBECONFIG` set to the kubeconfig when one is written or
named (else the environment's), files sent on standard input as a tar into a private directory;
nothing secret is an argument. The installer runs with OpenTofu's environment but for the
variables named as an input the installer declares (the installer reads an exported variable of an
input's name before an answers file or a `--given` file; but those the run needs itself: `PATH`,
`HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `LANG`, `TERM`, `KUBECONFIG`, the proxies' and
`DOCKER_*`, `XDG_*`, `SSL_CERT_*`, `LC_*`) and helm's `HELM_DRIVER`,
`HELM_DRIVER_SQL_CONNECTION_STRING`, `HELM_KUBE*` and `HELM_NAMESPACE` (another release store, or
another cluster than kubectl's), each unset. Each
operation first refuses a machine that is not linux/amd64 and names the programs missing from
`PATH` among `docker`, `helm` and `kubectl`. Every verb gets
`--namespace`, `--release` (when given) and `--kube-context` (with `config_context`).

- **The installer** is fetched as on Docker (`docker run --rm <installer> > <name>.sh`, the pull
  account's login in a Docker configuration of its own, removed afterwards), into the run's
  directory, then `sh <name>.sh describe --json` (its `name` compared; its shapes must include
  `kubernetes`). Each operation fetches the installer it runs, but for one fetched before: the
  bootstrap script an installer prints is kept by its digest (an image by digest prints one, ever)
  in the user's cache directory, `<cache>/terraform-provider-stowage/installers/<digest>/bootstrap.sh`
  (`~/.cache` on Linux; 0600 in 0700 directories, written through a temporary file renamed into
  place) with its SHA-256 beside it (`bootstrap.sh.sha256`), and copied into the run's directory
  from there; a kept script that does not match its checksum is fetched again, and a cache that
  cannot be written is only not kept. The cache keeps the 8 installers last used (each embeds an
  installer binary) and removes the others; delete the directory to fetch again.
- **Create**: the desired installer's `k8s status --json`. No release (exit 3, nothing on
  standard output, and `no release <release> in <namespace>:` on standard error — any other
  refusal is an error) installs: `sh <name>.sh k8s install --no-input --json [--set K=V]…
  [--image-registry <the provider's registry>]`, with `--answers` (the secret inputs), `--given`
  (`given_wo`) and `--registry-ca` on standard input; then `k8s status --json`, read again every 10
  seconds while it reports `installing` (helm's `pending-install`), within the create timeout. A
  release of another project is an error; one recording an installer the provider does not run
  (as at update) is refused, not taken over (`The release records another installer`: `k8s
  uninstall` it, or install in another namespace);
  one running another installer than the desired one is an error naming it (set `installer` to
  it, apply, then change it); one helm uninstalled with its
  history kept is the installer's install to decide. A release running the desired installer is
  taken over with a warning (`A release is already there`): `given_wo` is given as it is (`k8s
  upgrade --given`), then `inputs` and `secret_inputs_wo` the same way (below), which win as at an
  install; the provider's `registry` and `registry_ca` are not given (the release keeps what it
  recorded); with nothing to give, a release whose latest helm revision is failed is applied
  again, `k8s upgrade --no-input --json` given nothing. A release whose values record no installer is taken over only by an
  installer of the release it runs (the same `version` and `revision`, else `The release runs
  another release`); one helm is working on (`installing`, or a latest revision `pending-…` or
  `uninstalling`) is refused (`helm is working on the release`: apply again once it is over; a
  helm killed in its operation leaves it so for good, which `helm rollback` or `helm uninstall`
  ends). An input named as a variable the run keeps (above) while OpenTofu's environment sets it, or
  named `KUBECONFIG` (which the run sets itself), is refused at create and update, a key of
  `given_wo`'s included (its `inputs`, read as YAML) (`<KEY> comes from the environment`, `KUBECONFIG
  is set by the run`: such an input cannot be given by a run on this machine; leave it to its
  default). A
  create whose release is not `deployed` once settled is an error, and saves no state.
- **Read**: state's installer's `k8s status --json`. No release removes the resource from state;
  the release that runs replaces `installer` in state, and an input key it does not record is
  dropped, as on Docker. A release of another project under the name is an error. An installer
  the release records that the provider does not run (as at update, against state's installer
  and the provider's `registry`) is never put in state (a warning, `The release records another
  installer`): state keeps its installer and `status` is `failed: the release records an installer
  the provider does not run: "<installer>"` (the reference quoted, as Go quotes a string;
  followed by ` (and: <status>)` when helm's condition or a failure would have shown). A plan of such a state that changes the release is an
  error naming the fix — the configured installer's `k8s upgrade` run by hand, or `k8s
  uninstall`, then a refresh; or `tofu state rm` — but for an installer the configuration's own
  registry trusts (a configuration moved to another registry: the update then takes its usual
  way) or that is not known yet (the apply checks its registry; another project's, or no
  installer by digest, is refused at plan all the same), and while the provider's `kubernetes` block
  is not known (its cluster made again). A plan
  of the provider's own settings alone (`deletion_protection`, `delete_data`, `allow_rollback`,
  `timeouts`) is not refused, and touches nothing. A destroy of such a state is refused too (`Not
  uninstalled`: nothing is run over the release), unless the installer is trusted now (the
  provider's `registry` set to its registry since). `status` is
  `pending: helm's latest revision (<n>) of the release is
  <status>` while helm works on it (`pending-upgrade`, `pending-rollback`, `uninstalling`), and
  `failed: …` the same way when that revision is neither that nor `deployed` (a release
  uninstalled with its history kept: `not installed`). An imported state,
  never applied, is not read; nor is any while a value of the provider's `kubernetes` block is not
  known yet (its cluster made again in the same plan): state stands until the apply.
- **Update**: a change of the provider's own settings alone touches nothing. Otherwise, in
  DESIGN § 13's order:
  1. `k8s status --json`, by state's installer (an imported state's: the desired one). No release,
     another project's, one helm is working on (`helm is working on the release`, as at create),
     or nothing running (uninstalled with its history kept) is an error. An installer the release
  records that the provider does not run — not this project's by digest
  (`<prefix>/<name>-installer@sha256:<64 hex>`), or from another registry than the configured
  installer's (state's or the configuration's) or the provider's `registry`, registries compared
  as Docker reads them (a prefix without a host is `docker.io`, `index.docker.io` is `docker.io`,
  the host's case does not count) — is never fetched or run (the provider's pull account would go
  to it, and it would run on this machine), and nothing is run over it: the update is refused
  before anything changes (`The release records another installer`, naming the fix). A release
  recording such an installer after a run of the provider's (the reconfiguration, an upgrade)
  has its state saved with the status below, with that error, and nothing more is run (an
  upgrade after the reconfiguration is not); after a create, the same with a warning (an error
  would taint the resource, whose replacement uninstalls the release). A read after the
  reconfiguration that fails, or finds helm working on the release, stops the run there too
  (`the upgrade is not run`), state the reconfiguration's. A read after the apply that fails
  leaves `status` `failed: not read after this apply (refresh to read it)` rather than a drift
  the run may have ended. Each such diagnostic names the fix: when the installer is this
  release's own reference from another registry (a mirror's, an alias), set `installer` to it or
  the provider's `registry` to its registry; else install the configured release by hand (its
  `k8s upgrade`) or `k8s uninstall` it, then refresh; or `tofu state rm`. A
  `previous.installer` from another registry is left out of state, so no rollback goes to it. The
     installer of the release that runs (`installed.installer`; when the release records none,
     state's, else the desired one, which must then carry the release that runs: `the release
     names no installer`, naming it) is fetched and described: the running release's inputs.
  2. The release to reach, as on Docker: none, `k8s rollback` (with `allow_rollback`), or the
     desired installer fetched and described, its `k8s upgrade` to run; the same `version` and
     `revision` under another digest is an error naming the digest the release records.
  3. A **reconfiguration**, when there is anything to give the running release: its installer's
     `k8s upgrade --no-input --json --given <file>` (the same release applied again), the file on
     standard input: `project` and `inputs` — each input changed or added that the running release
     declares (every one without an upgrade), each taken out given `""`, and the secret inputs
     when `secret_inputs_wo_version` changed — which replace the values the release's Secret
     records; `--registry-ca` too. When a release change follows, state is saved here, as on Docker,
     with the release's status read again after the reconfiguration.
  4. The new installer's `k8s upgrade --no-input --json [--set K=V]…` given the inputs and secret
     inputs (`--answers`) the new release adds, with `--registry-ca`; or `k8s rollback --json`
     by the running release's installer, alone (`a rollback goes alone`; an imported release is
     taken over first). The registry is the one the release was installed from: `--image-registry`
     is never given again.
  5. `k8s status --json` by the installer that now runs fills the computed attributes.
- **Import**: `tofu import stowage_install.x 'kubernetes:<namespace>/<release>'`. State records
  the namespace and the release, `deletion_protection = true`; the first apply, with the
  configuration's cluster and installer, runs the update above (the release's project checked
  from its status) and records the provider's `registry`.
- **Delete**: refused while `deletion_protection`; else state's installer fetched and described,
  then `k8s uninstall --yes --no-input --json [--data]`. The installer's refusal `no release …,
  and nothing … made for it there` (gone, and nothing left) is success; one keeping its data's
  Secrets is an error, as any refusal (a chart whose own claims helm would delete refuses an
  uninstall without `delete_data`).

### Plan-time errors

A change of a first-install-only setting — `docker.work_dir`, `kubernetes.namespace`, the helm
release (`kubernetes.release`, else the project's name), `address`, `vm.channel`, the provider's
`registry`, the target block's kind, or the project an `installer` names (an imported Docker
state: its install directory's name; an imported release's is checked at the first apply) — is a plan error naming the old value, never a replacement:
replacing an install deletes its data.

Also plan errors: `installer` changed to `previous.installer` without `allow_rollback` (`a
rollback needs allow_rollback`); `tls` taken out (`tls cannot be removed`: a re-run has no flag
for loopback mode); a route taken out of `domains` or `ports` (`domains.<route> cannot be
removed`); a rollback with any other change of what the target applies (`a rollback goes
alone`); a route added to `domains` while `installer` changes (known at plan time) (`a domain added with a release
change`: a route the new release adds takes its domain from its domain input, in `inputs`; one
the running release has takes it in an apply of its own). A plan changing anything the target applies plans `status` unknown; one changing
`installer` also `version`, `revision`, `channel`, `previous` and `short_name`; an imported state's
first plan also `registry`.

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
make acc-k8s # the Kubernetes target through tofu on a kind cluster, against a real Stowage, in a container; prints PASS
```

**`make acc-docker`** (`STOWAGE_SRC=<a Stowage checkout ≥ 0.34.0, its ./bin/stowage built>`)
runs `test/acc-docker.sh`, the Docker target's acceptance test against a real installer through
OpenTofu: a registry (zot) and a Docker host — sshd beside Docker-in-Docker, its host key pinned
by `known_hosts_line` — share one network namespace; Stowage's `examples/minimal` is shipped there
as 1.0.0, 2.0.0 (an input added) and 3.0.0 (a rollback barrier); OpenTofu (1.13.9, its archive's
checksum checked, kept in `~/.cache/terraform-provider-stowage-tools`) loads the provider built
from this tree (`dev_overrides`). It walks a create (inputs, a secret input, ports, the pull
account), a refresh planning nothing, a re-run, an upgrade adding an input, a rollback (a plan
error without `allow_rollback`), an upgrade across the barrier, a refused upgrade after a re-run
(the re-run's state saved, the reason and the fix shown), a refused rollback, an import and its
first apply, and a destroy refused by `deletion_protection`, then one with `delete_data`; it
prints `PASS`. Everything it starts is named `acc-docker-<pid>` and removed at its exit; its work
directory is under `~/.cache` (`ACC_LOGS=<dir>` keeps the logs). Needs Docker, curl, jq, python3,
ssh, ssh-keygen and network access.

`make acc-k8s` (`test/acc-k8s.sh`) builds the provider and the acceptance runner
(`test/acc.Dockerfile`: OpenTofu 1.12.6, the Docker CLI and the harness's tools, pinned), then runs
the provider through `tofu` (`dev_overrides` in a CLI configuration of its own) in that runner,
capped at 4 GiB, driving the host's Docker through its socket, with Stowage's test harness:
`STOWAGE_REPO` (default `~/stowage`) holds a built `bin/stowage` and its `test/lib.sh` (zot, a kind
cluster `acc-k8s-<pid>` pulling from it, pinned kind, helm and kubectl). The runner mounts
`~/.cache` (the work directory), `~/.docker` (the registry logins, buildx's state), this tree and
Stowage's at their own paths, as the containers the suite starts are given host paths. It ships `examples/minimal` 1.0.0 and 2.0.0 and
walks create, a plan with no change, a changed input, the upgrade, a plan with no change, the
rollback (refused first without `allow_rollback`) and destroy, checking the cluster at each step
and that no run leaves its private directory. It needs Docker, network access and a linux/amd64
host; `PROVIDER_BIN` runs another provider binary.

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
| `internal/installer` | the machine interface's documents, exit mapping, the Docker and Kubernetes targets' scripts |
| `internal/target` | `Runner`: this machine, or the system `ssh` with a pinned host key |
| `examples/` | provider and resource examples (read by tfplugindocs) |
| `.goreleaser.yml`, `terraform-registry-manifest.json` | the release's zips, checksums, signature and registry manifest |
| `.github/workflows/` | `ci.yml` (check and build), `release.yml` (a pushed tag's release) |
| `.githooks/` | the commit identity, push and private-words guards (`make hooks`) |
| `test/hooks.sh` | the hooks' test |
| `test/acc-k8s.sh` | the Kubernetes target's acceptance test (`make acc-k8s`) |
| `keys/` | the release key's public half |
| `docs/img/` | the logo, mark, icons and the repository's social preview |

Licence: Apache-2.0.
