# Changelog

The provider's releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

## [0.1.0]

### Added

- `stowage_vm_seed`: a new Kairos VM's first-boot material (`vm seed --json` of an installer, run on
  the machine running OpenTofu), generated once: the cloud-config and the one-time SSH host key
  (both sensitive), its `known_hosts` line and fingerprint, the disk each platform boots, the
  secret inputs the first install waits for, the NoCloud meta-data, each disk's sizes and cloud
  image name, and with `disk_dir` the disk written as a local file. Its first install can wait for
  a secrets export (`await_secrets`) or a `byo` certificate (`tls = "byo"`), given over SSH by
  `stowage_install`. An example makes a TrueNAS VM of it with Stowage's module.
- `stowage_install` on a Kairos VM (`vm` block): its first install over SSH once the VM's first
  boot settled, pinned to the seed's one-time key, the secret inputs and given values on standard
  input, of the release the VM's OS carries (another `vm.version` is refused at create); then the
  VM's own host key read, the one-time key retired, and the VM's own key alone accepted from then
  on. `secrets_wo` goes to the VM's first install, for a seed that awaits it. A change of inputs
  re-runs the install; a new `vm.version` upgrades the VM and follows its reboot until the health
  gate judged it, a fallback or a kept failed boot being an error that saves the release the VM
  runs; `previous.version` with `allow_rollback` rolls it back. Either reboot is followed to the
  health gate's verdict on the boot the VM settles in, read from the VM's event journal (a
  fallback's passive boot judged in turn), and an unhealthy verdict is an error naming the gate's
  reasons; a release change on a VM whose status names no event journal is refused before it
  runs. The VM's status, journal, upgrade and rollback runs are logged at debug level
  (`TF_LOG_PROVIDER=DEBUG`), the verbs' standard output only. Read reports the release the VM
  runs; destroy forgets the install; import takes `vm:<host>/<short>-stow`. `vm.short_name` and
  `vm.channel` are first-install only, a channel other than the VM's refused; a VM without the
  installer state names (a release that renamed it) is told apart from another failure, with the
  route to take the install over.
- A first install still running when the create's timeout ends says so, whatever step the
  deadline cut.
- An imported install's first apply records the provider's `registry` as a planned change, no
  longer an inconsistent result.
- A failed `describe` names an installer older than Stowage 0.34.0 only when it exits 1 (an
  unknown command), on Docker and on the VM; an installer of Stowage 0.34.0 is refused at its
  `describe`, naming Stowage 0.35.0, the release the provider reads.
- `kubernetes.image_pull_login`: the cluster's pull Secret for the mirror written from the
  provider's pull account at every install and upgrade (opt-in).
- `registry_ca_digest`: the registry CA an install keeps; a plan giving the provider another CA
  is a first-install-only error on Docker and the VM.
- A create over an install already in the directory, and an imported install's first apply,
  refuse a configuration whose address, registry or registry CA is not the install's; a read
  records the install's own, so the next plan shows a drift as an error.
- `stowage_install` data source: a Docker install's or a VM's `status --json`, read without
  managing it.
- `registry_username` and `registry_password`: a mirror's own login, for every run that reads the
  mirror (a Docker target's installer, the Kubernetes runs, the seed, a fetch from it), on standard
  input alone.
- `stowage_vm_seed` reads a mirror on another host than the installer's with the machine's own
  Docker login, never with the installer registry's pull account.
- `plan_dry_run`: on Docker, a plan changing the release runs its upgrade's (or rollback's)
  `--dry-run` on the target, so the installer's refusal is a plan error.
- Reference documentation in `docs/`, generated from OpenTofu's own schema of the provider
  (`make docs`), never Terraform's.
- The VM's one-time host key is retired by the VM's own `<short>-stow host-key` (Stowage 0.35.0),
  no longer by a copy of its steps; the retirement is still proved by a connection accepting the
  VM's own key alone.
- A pull account's login keeps the Docker context in use, so the fetch reaches the daemon the
  machine's Docker client is set to.

- `stowage_install` on a Docker host, reached over SSH with its host key pinned or on the machine
  running OpenTofu: create (the installer fetched on the target by digest, its `describe` read,
  the install run unattended), read (`status --json`, the release that runs replacing the one
  configured) and delete (refused while `deletion_protection`; `delete_data` deletes the volumes).
- Secret inputs, a handoff's given values and a secrets export as write-only attributes, sent to
  the target on standard input, never on a command line or in state.
- The installer's refusals as diagnostics: the reason as the summary, the installer's fix as
  advice in the detail (never run); an installer of an unknown machine interface format refused.
- The installers' result of each verb read as Stowage 0.34.0 prints it: one object on standard
  output, exit 0 done or nothing, 3 refused, any other failed; an exit 0 or 3 without that object
  is a failure naming the breach. An installer without the machine interface stops at its
  `describe`, before any verb, destroy included, naming Stowage 0.34.0.
- First-install-only settings changed: a plan-time error, never a replacement.
- Changing a Docker install: changed inputs, secret inputs (on a new `secret_inputs_wo_version`),
  TLS mode, domains and ports reconfigure the running release first; then a new `installer`
  upgrades it, the inputs and ports the new release adds given with the upgrade. A failed upgrade
  keeps the reconfiguration in state, so the next plan shows the upgrade alone.
- Rollback: `installer` set back to `previous.installer` with `allow_rollback` runs the kept
  installer's rollback, alone; without `allow_rollback`, or with another change beside it, it is
  a plan error.
- An `installer` naming the release installed under another digest is refused, naming the
  digest the install records.
- An input taken out of `inputs` is given empty to the installer; taking out the TLS mode, a
  domain or a port is a plan error.
- Import of a Docker install by the installer it keeps: `[<host>:]<work_dir>/<name>/<short>-stow`.
- A create waits, within its timeout, for a first install still in progress.
- Read removes from state an input the install does not record, so the next plan gives it again;
  an input only a newer release declares, which the install keeps after a rollback, stays.
- A port for a route the running release has goes to the re-run even when an upgrade follows; the
  upgrade takes those of the routes the new release adds.
- `work_dir` is an absolute, clean path; left out, the home is resolved at create, so naming it
  later changes nothing.
- A plan changing what the target applies shows the release read back as unknown, as the target
  may run another than state says; an input moved to `secret_inputs_wo` without a new version is
  refused at plan.
- `short_name` attribute; `status --json` must carry its `format`; `update` timeout (default 30m).
- `stowage_install` on Kubernetes: the provider's `kubernetes` block, named as the helm
  provider's (`host` with a token, a client certificate or an `exec` credential plugin, or
  `config_path` and `config_context`), reaches the cluster, never in state; a kubeconfig is written
  for each run and removed after it; the resource's `kubernetes` block names the `namespace` and
  `release`. The installer runs on the machine running OpenTofu (linux/amd64, with docker, helm and
  kubectl): create (`k8s install`), read (`k8s status --json`), changed inputs (`k8s upgrade
  --given` by the running release's installer), a new release (its `k8s upgrade`), rollback
  (`k8s rollback`, with `allow_rollback`), import (`kubernetes:<namespace>/<release>`) and delete
  (`k8s uninstall`, `--data` with `delete_data`). The front's settings and `secrets_wo` are refused
  with it; a change of namespace or release is a plan error. The installer's run never takes an
  input from an exported variable, nor helm's cluster from `HELM_KUBE*` or its store from
  `HELM_DRIVER`; a release helm left failed is applied again, one helm works on refused. An
  installer's bootstrap script is kept by digest in the user's cache: a refresh fetches it once.
- A Docker install on the machine running OpenTofu no longer takes an input from an exported
  variable of its name, over the configured value.
- A create whose timeout ends during a status read says the install is still installing.
- An installer a Kubernetes release records is fetched and run only when it is the project's, by
  digest, from the configured installer's registry (or the provider's mirror). One from elsewhere
  is never run, nor anything over it: a refresh shows it in `status`, and a plan is an error
  naming the fix.
- A create over an install already in its directory (a create that failed or timed out, or an
  install a destroy kept with its data) runs the installer again on it, without the settings only
  a first install takes, with a warning; another project's install there is refused.
- SSH trusts no DNS record and reuses no shared connection: the pinned host key alone.
- A run cut by its timeout returns, even when a program it started keeps its output open; a
  program that exited keeps its exit status then.
- Taking `docker.work_dir` out of the configuration is a plan error, as changing it is.
- An input a release declares secret is refused in `inputs` on an update too, the release an
  upgrade goes to included; one moved to
  `secret_inputs_wo` is no longer given empty, and needs a new `secret_inputs_wo_version`.
- A missing install directory is recognised behind an SSH banner.
- A value not known until apply — a write-only secret input from an ephemeral variable, an input,
  domain or port computed from another resource — passes validation; its key is still checked.
  A null value there is refused by its key; a port is checked again at apply, once known.
- An imported install's first apply records the provider's `registry` (the plan leaves it to the
  apply), where OpenTofu refused the result as inconsistent.
- Releases in the OpenTofu registry's layout: a zip per platform, the plugin protocol manifest, and
  checksums signed with the Norvex Labs release key `686D3351040F9F4463D9D3AC0C705E9BAA4F4981`,
  whose public key the repository publishes.
