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

**Status: beta** (0.2.0 released on GitHub, signed; 0.3.0 in progress; not in a registry yet). The
schema and behaviour may still change before 1.0, each change named in [CHANGELOG.md](CHANGELOG.md).
What works: one resource type per shape — `stowage_docker_install`, `stowage_kubernetes_install`,
`stowage_vm_install`: create, read, update (inputs, then the release: upgrade or rollback), import
and delete, with an opt-in plan-time dry run on each (the VM's needs Stowage 0.38.0 on its OS) —,
`stowage_vm_seed`, the first-boot material of a Kairos VM a hypervisor's or cloud's resource makes,
and a data source per shape. A complete root per platform makes the VM itself, with the platform's
own provider ([the platform guides](docs/guides/platforms.md)). Each target is proven by its
acceptance test against a real installer (`make acc-docker`, `acc-k8s`, `acc-vm`: [DEVELOPMENT.md](DEVELOPMENT.md));
what waits for a proof is in [TODO.md](TODO.md). Not in a registry yet: [install it locally](#install).

- Provider address: `registry.opentofu.org/norvexlabs/stowage` (`norvexlabs/stowage`), plugin
  protocol 6.
- Needs **OpenTofu ≥ 1.11** (write-only attributes), and for a remote target the system `ssh`
  binary, OpenSSH ≥ 8.5 (`KnownHostsCommand`), on the machine running OpenTofu. The target needs
  a POSIX `sh`, `tar`, `mktemp` and Docker; a VM, those of its OS bundle and `sudo` without a
  password for its SSH user (the bundle's admin group).
- The Kubernetes target and `stowage_vm_seed` run the installer on the machine running OpenTofu,
  natively and with **no Docker**: the installer image's entry for that machine's platform
  (linux/amd64, linux/arm64, darwin/amd64 or darwin/arm64, from Stowage 0.40.0; before, linux/amd64
  alone), read from the registry and checked as [trust](docs/guides/trust.md) says (the provider's
  `public_keys`, the keys of the release installed). The Kubernetes target
  also needs `helm` ≥ 3.14 (the installer checks the version) and `kubectl` on `PATH`.
- Reads installers made by **Stowage 0.35.0 or later** (machine interface format 1); the 0.40.0
  values (`pull_login`, `acme_ca_root`, the seed's `size` and `firewall`) from Stowage 0.40.0.

## Install

The provider is not in a registry yet. Each [GitHub
release](https://github.com/norvexlabs/terraform-provider-stowage/releases) holds a signed zip per
platform: check it and copy it into OpenTofu's local mirror,
`~/.terraform.d/plugins/registry.opentofu.org/norvexlabs/stowage/` ([the
steps](docs/guides/install-without-registry.md#a-releases-zip-in-the-implied-local-mirror)). Or
build it from a checkout (Docker is the only requirement; linux/amd64), then copy it into
OpenTofu's local mirror, `~/.terraform.d/plugins`:

```bash
make install-local
```

A root then requires it as it will once published, and `tofu init` installs it from the mirror
(after a rebuild, `tofu init -upgrade`):

```hcl
terraform {
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.3"
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
  `known_hosts_line`; the installer runs there. A whole root, the host made too:
  [`examples/guides/docker-aws`](examples/guides/docker-aws/).
- [**Kubernetes**](examples/quickstart/kubernetes/main.tf): the provider's `kubernetes` block
  reaches the cluster (as the helm provider's); the installer runs on the machine running OpenTofu.
  A whole root, the cluster and its prerequisites made too: [`examples/guides/kubernetes-kind`](examples/guides/kubernetes-kind/).
- [**VM**](examples/quickstart/vm/main.tf): `stowage_vm_seed` makes the first-boot material once,
  your hypervisor's resource the VM, and `stowage_vm_install` installs and keeps it over SSH. A
  whole root per platform: [`examples/guides/<platform>-vm`](examples/guides/) (TrueNAS, Proxmox
  VE, libvirt, VMware, AWS, Azure, Google Cloud, Scaleway).

```hcl
provider "stowage" {}

resource "stowage_docker_install" "app" {
  installer        = "registry.example.com/app/app-installer@sha256:<64 hex>" # as `stowage ship` prints it
  inputs           = { ADMIN_EMAIL = "ops@example.com" }
  host             = "app.example.com"
  user             = "ops"
  known_hosts_line = "app.example.com ssh-ed25519 AAAA…"
  work_dir         = "/srv"
}
```

A change of `inputs` re-runs the release that runs; a new `installer` upgrades it (back to
`previous.installer` with `allow_rollback`, a rollback); a destroy is refused while
`deletion_protection` (the default) and keeps the data unless `delete_data`.

## Reference

Generated from the schema, the templates and the examples (`make docs`) into [docs/](docs/index.md):

| | |
|---|---|
| [Provider](docs/index.md) | the pull account, `registry`, `registry_ca`, `public_keys`, the `kubernetes` block |
| [`stowage_docker_install`](docs/resources/docker_install.md) | an install on a Docker host: every argument and attribute, the plan-time errors, its import ID |
| [`stowage_kubernetes_install`](docs/resources/kubernetes_install.md) | a helm release on a cluster |
| [`stowage_vm_install`](docs/resources/vm_install.md) | the install on a Kairos VM: its keys, `interfaces`, `platform` |
| [`stowage_vm_seed`](docs/resources/vm_seed.md) | a Kairos VM's first-boot material, generated once: `ctl_path`, `size`, `firewall` |
| [`stowage_kit`](docs/resources/kit.md) and the [ephemeral `stowage_kit`](docs/ephemeral-resources/kit.md) | a handoff kit issued by an install (`kit create --json`), kept in the provider's process and handed to `kit_wo` in the same apply: never in a file of the root, state or plan |
| [`stowage_kit_take`](docs/resources/kit_take.md) | a kit taken by an install that exists (its re-run's `--kit`) |
| [`stowage_kit_check`](docs/resources/kit_check.md) | the issuer's `kit check`, tried until it answers: `proven` |
| Data sources: [Docker](docs/data-sources/docker_install.md), [Kubernetes](docs/data-sources/kubernetes_install.md), [VM](docs/data-sources/vm_install.md) | an install's status, read without managing it |
| Guides: [a VM per platform](docs/guides/platforms.md), [without the internet](docs/guides/offline.md), [trust](docs/guides/trust.md), [handoff kits](docs/guides/handoff-kits.md) | complete roots per platform, with import IDs; a root on premises; what an installer is checked against; a kit issued, taken, taken back and proven in one apply (`examples/guides/handoff-chain`) |

## How it works

The provider reads only the installer's machine interface — `describe --json`, `status --json`,
the verbs' `--json` result and exit status (0 done, 3 refused with a reason and a fix, any other
failed) — and decides nothing the installer does not decide: [the machine interface, as
read](docs/guides/machine-interface.md). What runs where:

| | Docker | Kubernetes | VM |
|---|---|---|---|
| runs on | the host, over SSH (or this machine) | this machine, against the cluster | the VM, over SSH |
| installer | checked here ([trust](docs/guides/trust.md)), then fetched there by digest (`docker run --rm <installer>`) | read here from the registry, this machine's entry, checked ([trust](docs/guides/trust.md)), run natively | the VM's OS bundle's `<short>-stow`, whose `upgrade --version` checks a release itself |
| create | `sh <name>.sh --no-input --json` | `k8s install --json` | its first boot, then `sudo -n <short>-stow install --json` |
| read | `<short>-stow status --json` | `k8s status --json` | `<short>-stow status --json` |
| inputs | the running release's re-run | `k8s upgrade --given` | `install` (re-run) |
| release | `upgrade` / `rollback` | `k8s upgrade` / `k8s rollback` | `upgrade --version V`, its reboot followed / `rollback` |
| delete | `uninstall [--data]` | `k8s uninstall [--data]` | forgets it (the VM is its own resource's) |

Secrets (`secret_inputs_wo`, `given_wo`, `secrets_wo`, `registry_login_wo`, `kit_code_wo`) are write-only: on standard input, never on
a command line or in state. Every SSH connection accepts the pinned key alone. Each target in
full: [Docker](docs/guides/docker-target.md), [Kubernetes](docs/guides/kubernetes-target.md),
[VM](docs/guides/vm-target.md); a VM per platform: [guide](docs/guides/platforms.md).

## Building and testing

| Command | |
|---|---|
| `make check` | the gate: gofmt, vet, staticcheck, govulncheck, the hooks' test, go test -race, docs/ in step |
| `make build` | `./bin/terraform-provider-stowage`, after green checks |
| `make install-local` | the build in OpenTofu's local mirror |
| `make docs` | docs/ from OpenTofu's schema of the build (tfplugindocs) |
| `make acc-docker` | acceptance: Docker over SSH against a real installer (`STOWAGE_REPO=<Stowage checkout>`, default `~/stowage`) |
| `make acc-k8s` | acceptance: a kind cluster |
| `make acc-vm` | acceptance: a TrueNAS VM made by `examples/guides/truenas-vm` (`STOWAGE_VM_ENV=<file>`) |
| `make acc-libvirt` | acceptance: `examples/guides/libvirt-vm`'s domain against libvirtd under QEMU, in a container |
| `make examples-test` | `tofu validate` and `tofu test` (plan, mocked providers) of every example root |

Everything runs in Docker. The acceptance tests, the release procedure and the repository's map: [DEVELOPMENT.md](DEVELOPMENT.md).

## Licence

Apache-2.0. [CHANGELOG.md](CHANGELOG.md) · [TODO.md](TODO.md) · [Stowage](https://github.com/norvexlabs/stowage)
