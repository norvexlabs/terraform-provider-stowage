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
  <a href="https://github.com/norvexlabs/stowage"><img alt="Stowage ≥ 0.35.0" src="https://img.shields.io/badge/Stowage-%E2%89%A5%200.35.0-02BF87"></a>
</p>

# terraform-provider-stowage

The OpenTofu provider for [Stowage](https://github.com/norvexlabs/stowage): it declares a Stowage
product's install beside the VM, cloud or cluster it runs on, in one OpenTofu root. It reads only
the installer's machine interface (`describe --json`, `status --json`, the verbs' `--json` result
and exit status): it decides nothing the installer does not decide, orders no version and keeps no
copy of a rule.

**Status: pre-release (0.1.0, unpublished).** What works: the Docker, Kubernetes and VM targets —
create, read, update (inputs, then the release: upgrade or rollback), import and delete, with an
opt-in plan-time dry run on Docker —, `stowage_vm_seed`, the first-boot material of a Kairos VM a
hypervisor's or cloud's resource makes, and the `stowage_install` data source. The Docker target
is proven against a real installer over SSH (`make acc-docker`), the Kubernetes target on a kind
cluster (`make acc-k8s`); the VM target by unit tests, its acceptance test (`make acc-vm`) pending
(see [TODO.md](TODO.md)). Not in a registry: [install it locally](#install).

- Provider address: `registry.opentofu.org/norvexlabs/stowage` (`norvexlabs/stowage`), plugin
  protocol 6.
- Needs **OpenTofu ≥ 1.11** (write-only attributes), and for a remote target the system `ssh`
  binary, OpenSSH ≥ 8.5 (`KnownHostsCommand`), on the machine running OpenTofu. The target needs
  a POSIX `sh`, `tar`, `mktemp` and Docker; a VM, those of its OS bundle and `sudo` without a
  password for its SSH user (the bundle's admin group).
- The Kubernetes target and `stowage_vm_seed` run the installer on the machine running OpenTofu,
  which must be **linux/amd64** (installers are built for it) with `docker` (to fetch the
  installer image); the Kubernetes target also needs `helm` ≥ 3.14 (the installer checks the
  version) and `kubectl` on `PATH`.
- Reads installers made by **Stowage 0.35.0 or later** (machine interface format 1).

## Install

The provider is not published yet. From a checkout (Docker is the only requirement):

```bash
make install-local   # build, then copy into OpenTofu's local mirror, ~/.terraform.d/plugins
```

A root then requires it as it will once published, and `tofu init` installs it from the mirror
(after a rebuild, `tofu init -upgrade`):

```hcl
terraform {
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "0.1.0"
    }
  }
}
```

For development, `dev_overrides` runs `bin/` as built, with no `tofu init` for it. Both, and
another machine's mirror: [installing without the registry](docs/guides/install-without-registry.md).

## Quick start

One root per target in [`examples/quickstart/`](examples/quickstart/), each `tofu init` and `tofu
validate` clean once the provider is installed locally:

- [**Docker**](examples/quickstart/docker/main.tf): a host reached over SSH, its key pinned by
  `known_hosts_line`; the installer runs there.
- [**Kubernetes**](examples/quickstart/kubernetes/main.tf): the provider's `kubernetes` block
  reaches the cluster (as the helm provider's); the installer runs on the machine running OpenTofu.
- [**VM**](examples/quickstart/vm/main.tf): `stowage_vm_seed` makes the first-boot material once,
  your hypervisor's resource the VM, and `stowage_install` installs and keeps it over SSH. A full
  TrueNAS root: [`examples/resources/stowage_vm_seed`](examples/resources/stowage_vm_seed/resource.tf).

```hcl
provider "stowage" {}

resource "stowage_install" "app" {
  installer = "registry.example.com/app/app-installer@sha256:<64 hex>" # as `stowage ship` prints it
  inputs    = { ADMIN_EMAIL = "ops@example.com" }

  docker {
    host             = "app.example.com"
    user             = "ops"
    known_hosts_line = "app.example.com ssh-ed25519 AAAA…"
    work_dir         = "/srv"
  }
}
```

A change of `inputs` re-runs the release that runs; a new `installer` upgrades it (back to
`previous.installer` with `allow_rollback`, a rollback); a destroy is refused while
`deletion_protection` (the default) and keeps the data unless `delete_data`.

## Reference

Generated from the schema, the templates and the examples (`make docs`) into [docs/](docs/index.md):

| | |
|---|---|
| [Provider](docs/index.md) | the pull account, `registry`, `registry_ca`, the `kubernetes` block |
| [`stowage_install`](docs/resources/install.md) | every argument and attribute, the plan-time errors, import IDs |
| [`stowage_vm_seed`](docs/resources/vm_seed.md) | a Kairos VM's first-boot material, generated once |
| [`stowage_install` data source](docs/data-sources/install.md) | an install's status, read without managing it |

## How it works

The provider reads only the installer's machine interface — `describe --json`, `status --json`,
the verbs' `--json` result and exit status (0 done, 3 refused with a reason and a fix, any other
failed) — and decides nothing the installer does not decide: [the machine interface, as
read](docs/guides/machine-interface.md). What runs where:

| | Docker | Kubernetes | VM |
|---|---|---|---|
| runs on | the host, over SSH (or this machine) | this machine, against the cluster | the VM, over SSH |
| installer | fetched there by digest (`docker run --rm <installer>`) | fetched here by digest | the VM's OS bundle's `<short>-stow` |
| create | `sh <name>.sh --no-input --json` | `k8s install --json` | its first boot, then `sudo -n <short>-stow install --json` |
| read | `<short>-stow status --json` | `k8s status --json` | `<short>-stow status --json` |
| inputs | the running release's re-run | `k8s upgrade --given` | `install` (re-run) |
| release | `upgrade` / `rollback` | `k8s upgrade` / `k8s rollback` | `upgrade --version V`, its reboot followed / `rollback` |
| delete | `uninstall [--data]` | `k8s uninstall [--data]` | forgets it (the VM is its own resource's) |

Secrets (`secret_inputs_wo`, `given_wo`, `secrets_wo`) are write-only: on standard input, never on
a command line or in state. Every SSH connection accepts the pinned key alone. Each target in
full: [Docker](docs/guides/docker-target.md), [Kubernetes](docs/guides/kubernetes-target.md),
[VM](docs/guides/vm-target.md).

## Building and testing

```bash
make check          # the gate: gofmt, vet, staticcheck, govulncheck, the hooks' test, go test -race
make build          # ./bin/terraform-provider-stowage, after green checks
make install-local  # the build in OpenTofu's local mirror
make docs           # docs/ from OpenTofu's schema of the build (tfplugindocs)
make acc-docker     # acceptance: Docker over SSH against a real installer (STOWAGE_SRC=<Stowage checkout>)
make acc-k8s        # acceptance: a kind cluster
make acc-vm         # acceptance: a TrueNAS VM (STOWAGE_VM_ENV=<file>)
```

Everything runs in Docker. The acceptance tests, the release procedure (held until the maintainer
decides to publish) and the repository's map: [DEVELOPMENT.md](DEVELOPMENT.md).

## Licence

Apache-2.0. [CHANGELOG.md](CHANGELOG.md) · [TODO.md](TODO.md) · [Stowage](https://github.com/norvexlabs/stowage)
