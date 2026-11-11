# Changelog

The provider's releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

## [0.1.0]

### Added

- `stowage_vm_seed`: a new Kairos VM's first-boot material (`vm seed --json` of an installer, run on
  the machine running OpenTofu), generated once: the cloud-config and the one-time SSH host key
  (both sensitive), its `known_hosts` line and fingerprint, the disk each platform boots, the
  secret inputs the first install waits for. A seed with the `byo` TLS mode is refused at plan
  time, naming what to do instead. An example makes a TrueNAS VM of it with Stowage's module.
- `stowage_install` on a Kairos VM (`vm` block): its first install over SSH once the VM's first
  boot settled, pinned to the seed's one-time key, the secret inputs and given values on standard
  input, of the release the VM's OS carries (another `vm.version` is refused at create); then the
  VM's own host key read, the one-time key retired, and the VM's own key alone accepted from then
  on. `secrets_wo` is refused on a VM: a seed cannot make its first boot wait. A change of inputs
  re-runs the install; a new `vm.version` upgrades the VM and follows its reboot until the health
  gate judged it, a fallback or a kept failed boot being an error that saves the release the VM
  runs; `previous.version` with `allow_rollback` rolls it back. Read reports the release the VM
  runs; destroy forgets the install; import takes `vm:<host>/<short>-stow`. `vm.short_name` and
  `vm.channel` are first-install only, a channel other than the VM's refused; a VM without the
  installer state names (a release that renamed it) is told apart from another failure, with the
  route to take the install over.
- A first install still running when the create's timeout ends says so, whatever step the
  deadline cut.
- An imported install's first apply records the provider's `registry` as a planned change, no
  longer an inconsistent result.
- A failed `describe` names an installer older than Stowage 0.34.0 only when it exits 1 (an
  unknown command), on Docker and on the VM.

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
- Read removes from state an input the install does not record, so the next plan gives it again.
- `short_name` attribute; `status --json` must carry its `format`; `update` timeout (default 30m).
- The Kubernetes and VM targets' schema (not yet installed).
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
- Releases in the OpenTofu registry's layout: a zip per platform, the plugin protocol manifest, and
  checksums signed with the Norvex Labs release key `686D3351040F9F4463D9D3AC0C705E9BAA4F4981`,
  whose public key the repository publishes.
