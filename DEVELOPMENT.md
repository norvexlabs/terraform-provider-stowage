# Developing terraform-provider-stowage

For contributors: building, testing, the acceptance tests, the release procedure and the
repository's map. Using the provider is [README.md](README.md) and the generated reference in
[docs/](docs/index.md); the rules and commit gates are [CLAUDE.md](CLAUDE.md).

## Build

Docker is the only requirement.

```bash
make check   # GATE: gofmt, go vet, staticcheck, govulncheck, the hooks' test, go test -race (Dockerfile target checks), then make docs-check
make docs-check # docs/ regenerated in Docker (target docs-check: the pinned tofu, its checksum checked, and tfplugindocs) into a copy: any difference from docs/ fails
make build   # ./bin/terraform-provider-stowage, only after green checks
make install-local # the build in OpenTofu's implied local mirror (PLUGIN_DIR, default ~/.terraform.d/plugins): see docs/guides/install-without-registry.md
make test    # fast loop, never the gate
make docs    # docs/ by tfplugindocs from OpenTofu's schema of the built provider (test/schema.sh; the pinned tofu, downloaded once)
make tidy    # go.mod / go.sum
make fmt     # gofmt -w
make shell   # toolchain container
make hooks   # commit identity + .githooks
make hooks-test  # the hooks' test (test/hooks.sh) on this machine; make check runs it too
make release-check # goreleaser check + a snapshot release (zips, manifest, checksums; unsigned) in dist/, in Docker
make acc-k8s # the Kubernetes target through tofu on a kind cluster, against a real Stowage, in a container; prints PASS
make acc-vm  # the VM target and stowage_vm_seed on a real TrueNAS VM (STOWAGE_VM_ENV=<file>), in the same container; prints PASS
```

**`make acc-docker`** (`STOWAGE_SRC=<a Stowage checkout ≥ 0.35.0, its ./bin/stowage built>`)
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
prints `PASS`. `stowage ship` builds the example's images with a buildx builder of its own
(`docker-container` driver, capped at 4 GiB, `BUILDX_BUILDER`). Everything it starts is named
`acc-docker-<pid>` and removed at its exit; its work
directory is under `~/.cache` (`ACC_LOGS=<dir>` keeps the logs). Needs Docker, curl, jq, python3,
ssh, ssh-keygen and network access.

`make acc-k8s` (`test/acc-k8s.sh`) builds the provider and the acceptance runner
(`test/acc.Dockerfile`: OpenTofu of `test/tofu.sh`'s pin, the Docker CLI, ssh, xorriso, oras,
zstd, jq and the harness's tools, pinned), then runs
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

`make acc-vm` (`STOWAGE_VM_ENV=<file>`, the file Stowage's VM suites read, given at run time and
never written in the tree; `STOWAGE_REPO`, a Stowage checkout ≥ 0.35.0 with `bin/stowage` built)
runs `test/acc-vm.sh` in the same runner, capped at 4 GiB, with the host's network (TrueNAS reaches
the suite's mirror on this host's address): the env file and the directory of the SSH key it names
(`DEV_VM_SSH_KEY`, its `.pub` beside it) are mounted read-only at their own paths. It ships
`examples/minimal` with its kairos shape and a secret input, carries it to a TLS mirror, and walks
`examples/resources/stowage_vm_seed`'s root: the seed (its disk written by `disk_dir`, its ISO by
xorriso), Stowage's TrueNAS module, the first install over SSH under the one-time key, the VM's own
key and the one-time key's retirement, a plan with no change, a re-run, an upgrade and its reboot,
the rollback (refused first without `allow_rollback`), and the destroy; it prints `PASS`. The host
needs Docker, network access to TrueNAS and a linux/amd64 machine; every other tool is the
runner's.

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

Held: a release is cut only when the maintainer decides to publish (held since 2026-11-11). The
procedure, for then:


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
| `internal/provider` | provider configuration, `stowage_install` (Docker, Kubernetes, VM) and `stowage_vm_seed` |
| `internal/installer` | the machine interface's documents, exit mapping, the targets' scripts |
| `internal/testutil` | the tests' shared helpers |
| `internal/target` | `Runner`: this machine, or the system `ssh` with a pinned host key |
| `examples/` | provider and resource examples (read by tfplugindocs) |
| `.goreleaser.yml`, `terraform-registry-manifest.json` | the release's zips, checksums, signature and registry manifest |
| `.github/workflows/` | `ci.yml` (check and build), `release.yml` (a pushed tag's release) |
| `.githooks/` | the commit identity, push and private-words guards (`make hooks`) |
| `test/hooks.sh` | the hooks' test |
| `test/acc-docker.sh`, `test/acc-k8s.sh`, `test/acc-vm.sh` | the targets' acceptance tests (`make acc-docker`, `acc-k8s`, `acc-vm`); `test/acc.Dockerfile` their runner |
| `test/tofu.sh`, `test/schema.sh` | the pinned OpenTofu; the provider's schema by it (`make docs`) |
| `keys/` | the release key's public half |
| `docs/` | the reference documentation `make docs` generates; `docs/img/` the logo, mark, icons and the repository's social preview |

Licence: Apache-2.0.
