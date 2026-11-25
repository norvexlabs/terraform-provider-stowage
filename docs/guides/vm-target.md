---
page_title: "The VM target - stowage"
subcategory: "Guides"
description: |-
  What each operation of stowage_vm_install runs on a Kairos VM made from stowage_vm_seed, and its SSH host keys.
---

# The VM target

Every run reaches the VM by the system `ssh`, as on Docker: [SSH](docker-target.md#ssh).

`<short>-stow` is the VM's own (its OS bundle's, on its `PATH`): the reads run as the SSH user
(`<short>-stow describe --json`, `<short>-stow status --json`), the changes as root without a
password (`sudo -n <short>-stow install|upgrade|rollback … --json`). Files go on standard input
into a private (0700) temporary directory removed afterwards, as on Docker; nothing secret is an
argument.

- **Create** (every step under `known_hosts_line`, the one-time key):
  1. The first boot, as `vm create` waits for it, tried again every 10 seconds within the create
     timeout (`the VM's first boot had not settled … when the timeout ran out`, with the last
     error, and with `platform` where its boot shows: `The VM's boot shows on …`): `describe --json` (ssh failing, exit 255, while the VM boots; no `<short>-stow` on the
     VM — any failure but ssh's, the VM's shell then not finding it (`command -v`) — is an error on
     `short_name`, `The VM has no
     <short>-stow`, asking for `stowage_vm_seed`'s; another failure is an error at once), then `sudo -n systemctl start <name>.service`, the VM's boot unit, which
     returns once that boot did its part (loaded the release's images, made the first install or
     found it waiting, or wrote another release's OS and rebooted: ssh's exit 255, tried again) —
     so the provider's install never runs beside the boot's, which the install's lock would
     refuse; any other failure of the unit goes on to the install, which finishes what it left or
     says why not —, then `status --json` (tried again while it fails: the VM's daemon may answer
     after sshd), neither on the `recovery` system nor with `reboot_pending` (on the `recovery`
     system with `reset_failed`, Kairos's reset of the first boot failed and stays: an error at
     once, naming its reason), and `describe --json`
     again, the same release (else the OS changed under the boot: read again; ssh failing, tried
     again; another failure, an error at once). Its shapes must include `kairos` when it lists any.
  2. `version` must be that release (`version is not the VM's release`, naming it): a create
     installs the release the VM's OS carries and never upgrades, as an upgrade failing there
     would leave a created object OpenTofu replaces. An input given a non-empty value that
     `describe` marks `secret` is refused. Neither runs anything.
  3. With `registry_login_wo`, `sudo -n <short>-stow registry-login FILE`, the login a file on
     standard input in a private directory removed afterwards (a refusal, exit 3, is an error
     naming its reason, and nothing else runs). Then `sudo -n <short>-stow install --no-input
     --json [--set K=V]… [--tls] [--domains] [--ports] [--acme-ca URL]`, with
     `secret_inputs_wo` as `--answers`, `given_wo` as `--given`, `secrets_wo` as `--secrets`,
     `acme_ca_root` as `--acme-ca-root`, and `kit_wo` and `kit_code_wo` as `--kit` and
     `--kit-code-file` on standard input: the
     first install (from the settings the seed's cloud-config wrote), or the re-run of the one the
     first boot made — the installer decides, and refuses what a re-run does not take (exit 3,
     the diagnostic).
  4. `status --json` until it no longer reports `installing` (a read that fails is tried again);
     an `install_failed`, or no release installed, is an error saving no state (apply again: the
     install runs again and finishes). Then `channel`, when given, must be the channel the VM
     follows (the `channel` row), else an error saving no state.
  5. The one-time key retired by the VM's own verb, `sudo -n <short>-stow host-key --json`
     (Stowage's steps, sshd's reload checked; run again it changes nothing more), whose
     `public_key` is the VM's own key: `host_key_line`, `<host> ssh-ed25519 <base64>` (the
     `host` it was learnt under: a line's host field is never read, the connection naming the
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
     named: `has no host-key verb`, with the `version` change that moves it on) is a warning
     (`The VM's one-time host key was not retired`), and `seed_boot_id` is set: the one-time key is accepted beside the VM's own
     until a later boot or an update whose retry holds. When the verb gave no key (a refusal, a
     failed step, no verb), the key is read alone through the one-time key (`cat` as above), so
     the connection accepts both whatever the verb left; a failure of that too is a warning (`The
     VM's own host key was not read`): the connection keeps accepting `known_hosts_line` alone,
     which the VM's next boot retires. A retry whose verb prints another key than state's
     `host_key_line` (by the key alone) is an error that stops the update (`The VM's own host
     key changed`), naming the line to give and the import.
  6. With `interfaces`, once the install and its key are saved: a status without `boot_id` or
     event journal (whose reboot could not be followed) refuses before any NIC is attached
     (`the VM reports no boot_id or no event journal … nothing was attached`, naming its
     `events_error`); else `sudo -n <short>-stow network
     attach --role R --mac M [--mode] [--address]` for each NIC (sorted by role), then `sudo -n
     <short>-stow reboot` (the connection may end before it answers), followed to the health
     gate's verdict on the boot after it, as an upgrade's reboot: the NICs are named from that
     boot. One that fails is a warning: the install is recorded with `interfaces` as planned (its
     result matches its plan), those roles held pending in the resource's private state, and the
     next plan is an update (`status` known after apply, the warning `NICs not attached yet`
     naming them) that attaches them and reboots the VM, as an update's attach; once attached,
     nothing is pending. A reboot the health gate found unhealthy is not a failed attach: the
     NICs are recorded attached, nothing pending, and the verdict is a warning (`The VM came back
     unhealthy from attaching its NICs`); no apply attaches them again.
- **Read**: `status --json --values` (the values read back: a login the VM no longer holds — `pull_login` false — takes `registry_login_wo_version` out of state, and in the `acme` and `dns-cloudflare` modes `acme_ca` and `acme_ca_root`, its digest, are read, so the next plan gives what changed again); one that finds no `<short>-stow` on the VM (a release that renamed its installer) names the route to take the install over, as an update's read does: the VM's own key in `known_hosts_line` (state's `host_key_line`; with `seed_boot_id` set, reboot the VM first, which retires its one-time key; else the key checked on its console), then `tofu state rm` and `tofu import` under the new name. It counts as missing when the run fails (any exit but ssh's 255: shells give a missing command 127, tcsh 1) and the VM's shell does not find `<short>-stow` either (`sh -c 'command -v <short>-stow'`, whatever the shell's language): a program the installer runs missing is not it. A check that itself fails is said (`could not be checked`). The first boot's wait stops at once on it too. No release installed and no first install running or failed removes
  the resource from state (the next apply installs again). The release the VM runs replaces
  `version` in state, so a VM whose health gate fell back shows its real release and the next
  plan proposes the upgrade again; `inputs` are dropped as on Docker. An imported state (no
  `known_hosts_line` yet) is not read.
  A read ssh fails (exit 255) is an error and the install stays in state, as on Docker, with the
  same summaries (`ssh's login to the VM <host> was refused`: a fix of `user` or
  `private_key_file` saved by `tofu apply -refresh=false`; `The VM <host> serves another host
  key`; `Cannot reach the VM <host> to read the install`) and common ways on. Another host key
  depends on the key a read accepts: while state has no `host_key_line`, state's
  `known_hosts_line` alone, the new key given there and saved by `tofu apply -refresh=false`;
  else the VM's own key state holds (`host_key_line`, the seed's line beside it while
  `seed_boot_id` is set), so a VM made again from its seed is a new install (`tofu state rm`,
  then the next apply installs it under the seed's key), and a VM whose own key changed is given
  it in `known_hosts_line` and imported again (`tofu state rm`, then `tofu import` its ID,
  `<host>/<short>-stow`, the address a read uses).
- **Plan, with `plan_dry_run`**: a plan changing `version` reads `status --json` and chooses the
  step as the update does (its errors — `a rollback needs allow_rollback`, `a rollback goes alone` —
  at plan), then runs `sudo -n <short>-stow upgrade --version <version> --dry-run --json` (given the
  inputs and secret inputs only the new release declares, as the upgrade is: step 4), or
  `rollback --dry-run --json`, over SSH under the connection state accepts: the host daemon runs
  every check the verb runs before its job (the release confirmed, the boot judged, no job running,
  no system written that no boot ran yet, and for an upgrade the checks its boot would refuse) and
  starts nothing — no backup, no OS written, no reboot. A refusal is a plan error, its reason the
  summary and its fix the detail. A plan that also re-runs the running release (inputs, secret
  inputs, TLS, domains or ports) gets a warning instead (`No dry run of this plan`). The VM's OS needs
  Stowage 0.38.0 or later: an older `<short>-stow` has no `--dry-run`, and the change is planned
  without one, with a warning (`No dry run of this plan`), so the upgrade bringing it can be planned. Before it,
  the VM's status is read as the update reads it, waiting as step 1 does, within `plan_dry_run`'s
  10 minutes (OpenTofu plans again at apply, perhaps right after another resource restarted the
  VM). The update's
  own refusals before it runs anything (no install, no `boot_id` or event journal, the channel of an
  imported state) are plan errors too. The dry run covers the upgrade alone: when the plan also gives
  the `domains` or `ports` of routes only the new release declares, a warning says the re-run after
  its reboot is not checked (`The dry run covers the upgrade alone`). When the plan gives inputs
  only the new release declares, `<short>-stow updates --version <version> --json` tells first
  whether an upgrade to it takes them (`takes_inputs`); one that does not — the VM's OS or the
  new release built with Stowage before 0.43.0 (an answer without `takes_inputs`, an
  `<short>-stow` refusing `--answers` as unknown, or one without `--dry-run`) — is dry-run without
  them, with a warning that the re-run after its reboot gives them unchecked (an `updates` that
  fails or refuses is a plan error, `Reading the VM's updates`, with its reason and fix, and no
  dry run runs), and that one the
  new release requires with no default fails the boot into it, the VM going back (`The VM's
  upgrade takes no input`: upgrade first to a release built with Stowage 0.43.0 or later that
  adds no required input). When state has no VM's own key, a warning says the apply reads it first and
  refuses the change when it cannot. While any value of the configuration (but `deletion_protection`, `delete_data` and `timeouts`)
  is not known, no dry run, with a warning (`No dry run of this plan yet`): OpenTofu plans again at
  apply, with it known, and that plan runs it (a dry run runs again there in any case). On an
  imported state never applied it runs too, and says what the take-over changes (see Import).
- **Update**: a change of the provider's own settings alone is saved without touching the VM.
  Otherwise, in this order:
  1. `status --json` (no release installed is an error; no `<short>-stow` on the VM is the Read
     step's rename route) and `describe --json` (the running release). The status is read again
     every 10 seconds, within the update timeout (`plan_dry_run`'s read at plan within its 10
     minutes; `the VM did not answer over SSH, or its boot did not settle, when the timeout … ran
     out; the last attempt: …`, naming the bound, with the platform's console hint), while — a
     power resource of the same apply may be restarting the VM — ssh fails as a VM down or
     booting fails it (no route to host, connection refused, timed out or reset, closed by the
     remote host, network unreachable, host down), the status run fails (exit 1: sshd answers
     before the VM's host daemon early in a boot; with no such ssh failure before it, for 12 reads,
     about two minutes, as one that never answers is no boot), or the VM's boot is not settled
     (`boot_settled` `false`: its install unit or its health gate still running, or their state
     cannot be read — `systemctl status <name> <name>-health` on the VM tells; a status without
     `boot_settled`, from an OS built with an older Stowage, is taken as it answers). So a VM that
     is off, or a `host` or `port` nothing answers on, waits out the timeout. Anything else stops
     at once: another ssh failure (a refused login, another host key, a host name that does not
     resolve), a refusal, no installer, a status this provider cannot read.
     The refusals that need no change come first: a rollback without `allow_rollback`, a secret
     input in `inputs`, a rollback with other changes, an imported state's `channel` other than
     the VM's (`channel is not the VM's`), and a release change on a status without `boot_id`
     (`The VM reports no boot_id`: its reboot could not be followed) or without `events` (`The VM
     reports no event journal`, with its `events_error`: the health gate's verdict could not be
     read). Then the keys,
     before anything else changes on the VM: when state has no `host_key_line` yet (it could
     not be read at create; an imported state), the VM's own key is learnt under the
     configuration's key as at create (step 5: the verb, the one-time key retired and the
     retirement proved, else `seed_boot_id` set); when `seed_boot_id` is set (a retirement
     that failed), it is null at once when the status shows a later boot (which retired the key),
     left to the reboot of a release change, and else tried again (the verb and its proof, the
     key it gives the one state holds) — null on success, a warning
     (`The VM's one-time host key is still not retired`) on failure. Either is saved at once, with
     the configuration's connection, whatever fails after it. A release change whose own key could not be read is an error
     before anything else runs (`The VM's own host key is not known`): its reboot would retire
     the key the connection accepts.
  2. The release to reach: none when the plan does not change `version` (a VM that runs
     another release than state says — a refresh skipped — is the next refresh's to show), nor
     when `installed.version` is `version`; `rollback` when it is `previous.version` (with
     `allow_rollback`, else an error); else `upgrade --version`.
  3. The **re-run**, `sudo -n <short>-stow install --no-input --json …`, when there is anything to
     give: the changed and added inputs the running release declares (every one without an
     upgrade), the inputs taken out that it declares (`--set KEY=`), the secret inputs on a new
     `secret_inputs_wo_version`, the changed `tls`, and the changed `domains` and `ports` (added
     ones too without an upgrade). When a release change follows, state is saved here, the release
     unchanged.
  4. The **upgrade**: `sudo -n <short>-stow upgrade --version <version> --yes --json`, given what
     the plan gives that only the new release declares when the VM's `updates --version <version>
     --json` says the upgrade takes it (`takes_inputs`) — its inputs and secret inputs together in
     an answers file on standard input (`--answers`, a private temporary directory on the VM),
     never on its command line, as the new release may declare secret an input given in
     `inputs` — which the VM's host daemon checks as the boot into it takes them and stages for
     that boot; it ends as it schedules the VM's reboot (an ssh cut with no result, exit 255, is
     taken as that reboot beginning). An upgrade that takes no input (the VM's OS or the new
     release built with Stowage before 0.43.0: no `takes_inputs`, or an `<short>-stow` refusing
     `--answers` as unknown) runs without them (an `updates` that fails or refuses is an error,
     `Reading the VM's updates`, before the upgrade: state stays as step 3 saved it, the release
     unchanged), step 6 giving them after the reboot (one the new
     release requires with no default then fails the boot into it, and the VM goes back). Or the **rollback**, `sudo -n
     <short>-stow rollback --yes --json`, alone (a change of inputs, secret inputs, TLS mode,
     domains or ports with it is an error, `a rollback goes alone`).
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
     `version`, or the last verdict is `kept_unhealthy`, it is an error naming each verdict —
     `The VM's health gate did not keep <v>'s boot (<reason>): the VM went back to the passive
     system.` (or `…, and the VM still runs that boot: its next boot goes to the passive system.`),
     `Kairos booted the passive system: the upgrade's boot failed before the health gate judged
     it` (an upgrade's fallback with no verdict), `The VM's health gate found <v>'s boot
     unhealthy: <reason>.`, with `It kept that boot, as going back is refused: <why>.` for a boot
     on trial — then `After its reboot the VM runs <release> (wanted: <version>).`; state
     saves the release the VM runs, so the next plan proposes the change again — or, when it
     runs `version` unhealthy, says no plan changes that (fix what the gate found), and that
     the next plan proposes the re-run of what the new release adds, when it adds something.
     At debug level (`TF_LOG_PROVIDER=DEBUG`, or `TF_LOG_PROVIDER_STOWAGE` for this
     provider alone), each `status --json`, `events --json`, `upgrade` and `rollback` run on the
     VM is logged as `ran on the VM`: the command, its exit, its standard output (the status,
     which names input keys and never a value, the journal's entries, or the verb's JSON result;
     its first 16 KiB), and the standard error of the reads; a verb's standard error, its run's
     free-text log, is logged by its size alone.
  6. After an upgrade that gave or leaves something it adds, `describe --json` of the new release;
     an input given in `inputs` a non-empty value the new release marks `secret` is an error then
     (state saves the release the VM runs, and not that input; with an OS that took the inputs on
     its upgrade, the install records it already: move it to `secret_inputs_wo`). Then the re-run
     again with what the upgrade did not give: the `domains` and `ports` of routes state does not
     record, and on an OS whose upgrade takes no input the inputs and secret inputs the old release
     did not declare. Then `status --json` fills the computed attributes (one that cannot be read
     keeps the status read after the reboot, with the error); `seed_boot_id` is null (the reboot
     retired the one-time key). After the reboot was followed, an error — the new release's
     describe, its re-run — saves the release the VM runs and the inputs applied; a reboot not
     followed in time leaves state as step 3 saved it (the release before the upgrade), the next
     refresh reading the one the VM runs.
  7. The **rotations**, when `secret_rotations` adds a key or changes a number (alone, the steps before
     are skipped): `<short>-stow
     describe --json` (its `secrets`, the ways kept in the resource's private state for the next
     plans), every key added or changed checked against it, the added recorded, then for each
     changed key, in key order, `sudo -n <short>-stow secrets rotate KEY --yes --no-input`
     (`replace`) or `… --stage`, `…` and `… --finish` (`passes`), its number saved once it rotated.
     The steps before run first and are saved. With nothing to rotate, `secret_rotations` set, an
     update after which the ways kept are not the running release's (it changed the release, or
     none were kept) reads its `describe` for those ways alone; a read that fails is a warning.
  `registry_login_wo_version` changed: `sudo -n <short>-stow registry-login FILE`, before the
  re-run (the login given again; refused with a release change's rollback, `a rollback goes
  alone`). `acme_ca` and `acme_ca_root` changed go to the re-run (`--acme-ca URL|none`,
  `--acme-ca-root FILE|none`). `interfaces` with a role added or changed: the status read as step 1
  reads it (the platform restarted the VM to give it the NIC; an imported state's first apply
  too, its status read with `--values`), a refused login or another host key stopping the wait at
  once with the read's error and its ways on (`ssh's login to the VM … was refused`, `The VM …
  serves another host key`), then the re-run, its state saved with `interfaces` as planned but for the roles to attach (as
  state had them, absent when it had none: an imported VM's roles taken over are recorded, never
  attached again), then `network attach` for each (and each role a create left pending) and the
  reboot followed, as at create — one that fails is an error, the next apply attaching those roles
  again; a reboot the health gate found unhealthy is an error naming the verdict (`Attaching the
  VM's NICs`), the NICs recorded attached, never attached again;
  a change of `interfaces` with one of `version` is a plan error (and, for an imported state,
  whose version a plan cannot compare, an apply error before anything runs).
- **Upgrade trust**: the provider fetches nothing for a VM's upgrade: `upgrade --version V` runs
  on the VM, whose own installer checks the release against the keys of the release it runs
  (Stowage's own rule); the VM's first release is its seed's (`stowage_vm_seed`, checked as the
  [trust guide](trust.md) says).
- **Import**: `tofu import stowage_vm_install.x '<host>/<short>-stow'` (`ls /usr/sbin/*-stow`
  on the VM); give `known_hosts_line` the VM's own key. State records `host`,
  `short_name` and `deletion_protection = true`; the first apply compares the configuration
  with the install's recorded values as on Docker (the VM's `status --json --values`: its OS
  needs Stowage 0.38.0 or later, else every configured input is given), then runs the update
  above. Its `interfaces` are read from the VM first: `sudo -n <short>-stow network --json`, whose
  `roles` name the NICs the VM names now; a role the VM names by the `mac` given is taken over
  whatever its addressing, with no `network attach` and no reboot, and state records
  `interfaces` as configured. When its addressing is not the one given — with an override
  (`network set --role`, the role's `override`), its `mode` is not the one given (left out,
  `none`) or, `static`, its `address` is not; without one (the VM reports no mode), a `static`
  role's `address` is not among its addresses, a `none` role has one (a `dhcp` one with no
  address yet is aligned: its lease is not there yet) — a
  warning (`The VM addresses NICs otherwise than interfaces`) names, for each such role, what the
  VM has and the commands that align it: ``sudo <short>-stow network set --role R --mode M
  [--address A]``, then ``sudo <short>-stow network confirm`` once the VM answers (Stowage's
  window). The others are attached with the addressing given, and the VM reboots. When the VM's
  NICs cannot be read, every interface is attached and the VM reboots (a warning says why). With
  a `version` change, only the roles the apply would attach refuse it (`interfaces change with
  version`). Its plan warns of it (`The first apply reads the VM's NICs`), and with
  `plan_dry_run` reads them: which roles the apply attaches (`The apply attaches NICs and reboots
  the VM`, or the refusal with a `version` change), or none (`The VM names every interface
  given`), and the warning on addressing. A rollback of an imported install (`version` set to `previous.version`) is refused (`a
  rollback of an imported install`: apply first with the version it runs).
- **Delete**: refused while `deletion_protection`; else the resource is forgotten, and nothing changes on the VM. The destroy's plan, before it is approved, asks — with `platform` and the VM's own key known — the VM's installer whether the registry still holds a disk set a VM of its release boots there: `sudo -n <short>-stow vm seed --json --platform P --ssh-key FILE [--registry <state's registry>]` (FILE the VM's own public key, on standard input; the output, a throwaway seed, discarded; two minutes at most, its SSH connection bounded to 10 seconds: `ConnectTimeout=10`). A VM that does not answer within them (stopped, gone) is not checked, a plan warning saying so (`Whether this VM can be made again was not checked`), a refusal is a warning naming its reason (`No disk set to make this VM again`: until the release publishes one, a VM destroyed is not made again), a failure another (`Whether this VM can be made again is not known`); none stops the destroy, whose apply forgets the install, asking nothing, and the VM's own resource's destroy follows.
