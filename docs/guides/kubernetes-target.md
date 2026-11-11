---
page_title: "The Kubernetes target - stowage"
subcategory: "Guides"
description: |-
  What each operation of stowage_install runs on a Kubernetes cluster, from the machine running OpenTofu.
---

# The Kubernetes target

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
- **Create**: the desired installer's `k8s status --json`. No release (exit 3, its refusal's
  object on standard output saying `absent` — any other refusal is an error) installs: `sh
  <name>.sh k8s install --no-input --json [--set K=V]… [--image-registry <the provider's
  registry>] [--image-pull-login]`, with `--answers` (the secret inputs), `--given`
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
