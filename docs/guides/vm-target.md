---
page_title: "The VM target - stowage"
subcategory: "Guides"
description: |-
  What each operation of stowage_install runs on a Kairos VM made from stowage_vm_seed, and its SSH host keys.
---

# The VM target

Every run reaches the VM by the system `ssh`, as on Docker: [SSH](docker-target.md#ssh).

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
     with `secret_inputs_wo` as `--answers`, `given_wo` as `--given` and `secrets_wo` as
     `--secrets` on standard input: the
     first install (from the settings the seed's cloud-config wrote), or the re-run of the one the
     first boot made — the installer decides, and refuses what a re-run does not take (exit 3,
     the diagnostic).
  4. `status --json` until it no longer reports `installing` (a read that fails is tried again);
     an `install_failed`, or no release installed, is an error saving no state (apply again: the
     install runs again and finishes). Then `vm.channel`, when given, must be the channel the VM
     follows (the `vm` block's `channel` row), else an error saving no state.
  5. The one-time key retired by the VM's own verb, `sudo -n <short>-stow host-key --json`
     (Stowage's steps, sshd's reload checked; run again it changes nothing more), whose
     `public_key` is the VM's own key: `vm.host_key_line`, `<vm.host> ssh-ed25519 <base64>` (the
     `vm.host` it was learnt under: a line's host field is never read, the connection naming the
     key by its alias; the verb's own host name, and its `known_hosts_line`, are not read
     either). A retirement the verb says took (`"retired": true`) is then checked by a connection
     accepting that key alone answering, reading it again (`cat
     /etc/ssh/ssh_host_ed25519_key.pub`), tried for six polls, a minute (`… when its six polls
     (a minute) ran out`): sshd's reload is a signal its unit does not wait out, so this lets the
     next connection find its listener, and it is the one observation that the VM serves its own
     key alone before state drops the one-time key from every connection. A reload not taken
     (exit 1, `"retired": false` with its reason; the warning carries the installer's message and
     fix: reload sshd on the VM, or reboot it, then apply a change), a check that fails (the
     files retired, sshd not serving the VM's own key alone yet: the next update with a change
     tries again, no reboot needed), a refusal of the verb (exit 3: a recovery or live boot, not
     run as root, the first install not done) or its failure (an installer without the verb is
     named: `has no host-key verb`, with the `vm.version` change that moves it on) is a warning
     (`The VM's one-time host key was not retired`), and `vm.seed_boot_id` is set: the one-time key is accepted beside the VM's own
     until a later boot or an update whose retry holds. When the verb gave no key (a refusal, a
     failed step, no verb), the key is read alone through the one-time key (`cat` as above), so
     the connection accepts both whatever the verb left; a failure of that too is a warning (`The
     VM's own host key was not read`): the connection keeps accepting `known_hosts_line` alone,
     which the VM's next boot retires. A retry whose verb prints another key than state's
     `vm.host_key_line` (by the key alone) is an error that stops the update (`The VM's own host
     key changed`), naming the line to give and the import.
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
     (`The VM reports no boot_id`: its reboot could not be followed) or without `events` (`The VM
     reports no event journal`, with its `events_error`: the health gate's verdict could not be
     read). Then the keys,
     before anything else changes on the VM: when state has no `vm.host_key_line` yet (it could
     not be read at create; an imported state), the VM's own key is learnt under the
     configuration's key as at create (step 5: the verb, the one-time key retired and the
     retirement proved, else `vm.seed_boot_id` set); when `vm.seed_boot_id` is set (a retirement
     that failed), it is null at once when the status shows a later boot (which retired the key),
     left to the reboot of a release change, and else tried again (the verb and its proof, the
     key it gives the one state holds) — null on success, a warning
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
  5. The reboot followed to the health gate's verdict: every 10 seconds (ssh failing
     meanwhile), the event journal after the entry the status before the change named
     (`<short>-stow events --after <events> --json`), until the last boot it judges after the
     change's has a verdict other than `went_back`; then `status --json`, and the wait ends when
     the VM answers from that boot. The gate judges every boot once its boot unit has taken the
     install to the release the OS carries (a rollback's passive boot answers before that, never
     on trial), so the verdict, never the status alone, ends the wait. A `went_back` verdict (an
     upgrade's trial boot not kept) is waited past: the passive system's boot is judged in turn.
     All within the update timeout; when it runs out, the error says what was waited on and names
     the `went_back` verdicts read (one whose reboot failed leaves the VM in that boot, which the
     error says), and state stays as step 3 saved it. When the VM then runs another release than
     `vm.version`, or the last verdict is `kept_unhealthy`, it is an error naming each verdict —
     `The VM's health gate did not keep <v>'s boot (<reason>): the VM went back to the passive
     system.` (or `…, and the VM still runs that boot: its next boot goes to the passive system.`),
     `Kairos booted the passive system: the upgrade's boot failed before the health gate judged
     it` (an upgrade's fallback with no verdict), `The VM's health gate found <v>'s boot
     unhealthy: <reason>.`, with `It kept that boot, as going back is refused: <why>.` for a boot
     on trial — then `After its reboot the VM runs <release> (wanted: <vm.version>).`; state
     saves the release the VM runs, so the next plan proposes the change again — or, when it
     runs `vm.version` unhealthy, says no plan changes that (fix what the gate found), and that
     the next plan proposes the re-run of what the new release adds, when it adds something.
     At debug level (`TF_LOG_PROVIDER=DEBUG`, or `TF_LOG_PROVIDER_STOWAGE` for this
     provider alone), each `status --json`, `events --json`, `upgrade` and `rollback` run on the
     VM is logged as `ran on the VM`: the command, its exit, its standard output (the status,
     which names input keys and never a value, the journal's entries, or the verb's JSON result;
     its first 16 KiB), and the standard error of the reads; a verb's standard error, its run's
     free-text log, is logged by its size alone.
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
