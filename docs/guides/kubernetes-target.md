---
page_title: "The Kubernetes target - stowage"
subcategory: "Guides"
description: |-
  What each operation of stowage_kubernetes_install runs on a Kubernetes cluster, from the machine running OpenTofu.
---

# The Kubernetes target

Everything runs on the machine running OpenTofu, in a private (0700) directory of the system's
temporary directory (`stowage-k8s-*`, never in the cache), removed when the operation ends, which
holds the kubeconfig (when `host` gives it) and each installer's working directory (its
`<name>.sh`); each installer's program is apart, in the cache (below). Every run is one `sh -c` script, `KUBECONFIG` set to the kubeconfig when one is written or
named (else the environment's), files sent on standard input as a tar into a private directory;
nothing secret is an argument. The installer runs with OpenTofu's environment but for the
variables named as an input the installer declares (the installer reads an exported variable of an
input's name before an answers file or a `--given` file; but those the run needs itself: `PATH`,
`HOME`, `USER`, `LOGNAME`, `SHELL`, `TMPDIR`, `LANG`, `TERM`, `KUBECONFIG`, the proxies' and
`DOCKER_*`, `XDG_*`, `SSL_CERT_*`, `LC_*`) and helm's `HELM_DRIVER`,
`HELM_DRIVER_SQL_CONNECTION_STRING`, `HELM_KUBE*` and `HELM_NAMESPACE` (another release store, or
another cluster than kubectl's), each unset. Each
operation first names the programs missing from `PATH` among `helm` and `kubectl`: no Docker. Every verb gets
`--namespace`, `--release` (when given) and `--kube-context` (with `config_context`).

- **The installer** is read from its registry for the machine running OpenTofu, with no Docker:
  the installer image (an OCI index) by its digest, its entry for this machine's platform
  (`linux/amd64`, `linux/arm64`, `darwin/amd64` or `darwin/arm64`; an index without one is refused,
  naming the platforms it has; an image of one platform, as a Stowage before 0.40.0 published, is
  taken for that platform alone), its one layer's one file, `<short_name>-stow` with the release's
  payload. The mirror's own login goes to the provider `registry`'s host; the provider's
  `username`/`password` to the host of an installer the configuration names (the resource's
  `installer`, the one state records) or of the provider's `registry`; the machine's Docker
  configuration (`~/.docker/config.json` and its credential helpers) reads any other host; the
  provider's `registry_ca` is trusted besides the system's; plain HTTP is refused but on this
  machine's loopback. The entry is checked, before its layer is read, as the [trust
  guide](trust.md) says: against the provider's `public_keys` when given, and, for the new release
  of an upgrade, against the keys the installer of the release running carries (its payload's
  `keys/`; that installer is fetched first, by the digest the release records); a sigstore bundle
  referring to the entry's digest (an OCI referrer, as cosign v3 writes it, read by the referrers
  API or its tag fallback) signed by one of them over that digest
  (`https://sigstore.dev/cosign/sign/v1`), with no transparency log. A referrer that cannot be
  read is a failure to check, never "unsigned". A first install with no `public_keys` trusts the
  digest alone (a plan warning says so). It is kept in the user's cache directory,
  `<cache>/terraform-provider-stowage/installers/<index digest>/<os>-<arch>/<short_name>-stow`
  (`~/.cache` on Linux; 0700 in 0700 directories, written through a temporary file renamed into
  place) with its SHA-256 and its image's digest beside it, checked at each use (and checked
  again against the run's keys: a program kept under none is checked when a trust applies) (a file that does not match is read
  again); the cache keeps the 8 index digests last used and removes the others, and a part
  file a fetch cut off left (`.installer-*.part`, unwritten for an hour) is removed at the next
  fetch into its entry or prune; a cache that cannot be written writes the file into the run's
  directory instead. The run's programs are in the cache too, and nothing else of the run:
  `<cache>/terraform-provider-stowage/installers/runs/stowage-k8s-*` (0700, removed after the run;
  one a day old, which a run cut off left, is removed when the next run starts; without a cache
  directory, the system's temporary directory), and the run executes its own hard link to the
  kept file there — else, on a file system that takes no link, a copy written to a temporary file
  there and renamed into place — so another run's prune of the cache cannot remove it during the
  run. A run the shell could not start as that program was busy (`Text file busy`, exit 126: a
  child another run forks may hold a copy open for writing for an instant) is made again, up to 5
  times over less than a second. The run's working directory gets `<name>.sh`, a script running
  that program with its arguments; then `sh <name>.sh describe --json`
  (its `name` compared; its shapes must include `kubernetes`).
- **Create**: the desired installer's `k8s status --json`. No release (exit 3, its refusal's
  object on standard output saying `absent` — any other refusal is an error) installs: `sh
  <name>.sh k8s install --no-input --json [--set K=V]… [--image-registry <the provider's
  registry>] [--image-pull-login]`, with `--answers` (the secret inputs), `--given`
  (`given_wo`), `--registry-ca`, `--values` (`values`), `--registry-login-file`
  (`registry_login_wo`, in `--image-pull-login`'s place) and `--kit` with `--kit-code-file`
  (`kit_wo`, `kit_code_wo`) on standard input; then `k8s status --json`, read again every 10
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
  recorded); with nothing to give, a release whose latest helm revision is failed, or any with
  `values`, is applied again, `k8s upgrade --no-input --json` given no input (each of
  these runs gets `--values` from `values` when set). A release whose values record no installer is taken over only by an
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
- **Plan, with `plan_dry_run`**: a plan changing `installer` reads the release and chooses the step
  as the update does (the same reads, fetches, checks and errors, in a private directory on this
  machine removed after), then runs the new installer's `k8s upgrade --dry-run --no-input --json`
  given what the upgrade would be (the inputs and secret inputs the new release adds) — the
  installer's checks and its chart pulled, the deploy it would make printed, nothing applied and
  helm not run — or `k8s rollback --dry-run --json`, the installer's checks and helm's own dry run of
  the rollback. A refusal is a plan error, its reason the summary and its fix the detail. A plan
  that also re-runs the running release gets a warning instead (`No dry run of this plan`), told
  before the new installer is fetched when the change is the running release's whatever the new one
  is. While any value of the configuration (but `deletion_protection`, `delete_data` and `timeouts`), the provider's `kubernetes` block or a setting a run reads (the pull account, `registry`, `registry_ca`, the
  mirror's login) is not known, no dry run, with a warning (`No dry run of this plan yet`): OpenTofu
  plans again at apply, with it known, and that plan runs it (a dry run runs again there in any
  case). On an imported state never applied it runs too, and says what the take-over changes (see
  Import).
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
     records; `--registry-ca` too. With nothing to give but `values` changed (or
     `image_pull_login` turned on, or `registry_login_wo_version` changed), the release is applied
     again: `k8s upgrade --no-input --json`. A changed `registry_login_wo_version` gives
     `registry_login_wo` as `--registry-login-file` (on standard input) to that run, or to the
     re-run and the upgrade of the same apply; beside a rollback it is refused (`a rollback goes
     alone`: helm's rollback writes no Secret). A read (`k8s status --json --values`) taking
     `pull_login` false takes `registry_login_wo_version` out of state, so the next plan gives
     the login again.
     Every `k8s upgrade` the update runs (this one, the next step's) gets `--values` from
     `values` when set; a rollback does not, and is followed, with `values`
     set, by the running release's `k8s upgrade --no-input --json --values` (helm's rollback takes
     back the values of the release it reaches). When a release change follows, state is saved here, as on Docker,
     with the release's status read again after the reconfiguration.
  4. The new installer's `k8s upgrade --no-input --json [--set K=V]…` given the inputs and secret
     inputs (`--answers`) the new release adds, with `--registry-ca`; or `k8s rollback --json`
     by the running release's installer, alone (`a rollback goes alone`; an imported release is
     taken over first). The registry is the one the release was installed from: `--image-registry`
     is never given again.
  5. `k8s status --json` by the installer that now runs fills the computed attributes.
  6. The **rotations**, when `secret_rotations` adds a key or changes a number (alone, the steps before
     are skipped): state's installer
     fetched again into the run's private directory and described (its `secrets`, the ways kept
     in the resource's private state for the next plans), every key added or changed checked
     against it, the added recorded, then for each changed key, in key order, `<short>-stow k8s
     secrets rotate KEY --stage`, `…` and `… --finish` with the release's flags (no
     `--registry-ca`: the verb reads no registry), its number saved once it rotated. Steps 1–5 run
     first and are saved. With nothing to rotate, `secret_rotations` set, an
     update after which the ways kept are not the running release's (it changed the release, or
     none were kept) reads its `describe` for those ways alone; a read that fails is a warning.
- **Import**: `tofu import stowage_kubernetes_install.x '<namespace>/<release>'`. State records
  the namespace and the release, `deletion_protection = true`; the first apply, with the
  configuration's cluster and installer, compares the configuration's inputs with those the
  release records (`k8s status --json --values` by the configuration's installer, Stowage 0.38.0
  or later; it tells them when it carries the release installed) as on Docker — matching, no helm
  upgrade runs for them —, runs the update above (the release's project checked from its status)
  and records the provider's `registry`.
- **Delete**: refused while `deletion_protection`; else state's installer fetched and described,
  then `k8s uninstall --yes --no-input --json [--data]`. The installer's refusal `no release …,
  and nothing … made for it there` (gone, and nothing left) is success; one keeping its data's
  Secrets is an error, as any refusal (a chart whose own claims helm would delete refuses an
  uninstall without `delete_data`).
