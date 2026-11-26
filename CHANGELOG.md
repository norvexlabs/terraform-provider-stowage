# Changelog

The provider's releases. Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions
follow [SemVer](https://semver.org/).

## [0.9.0] - 2026-11-27

For Stowage 0.50.0 or later with `ssh_allow` on `stowage_vm_install`.

### Added

- `stowage_vm_install` takes `ssh_allow`, the sources SSH accepts on the VM, changed in place after
  its first boot (the seed's `ssh_allow` is the first boot's alone): the apply sets them last, after
  any reboot, then confirms them over a new SSH connection, so a list that leaves out the address
  OpenTofu runs from fails the apply and the VM undoes it within its window. A refresh reads them
  back, so a change made on the VM shows in the plan. Each source must be a CIDR written masked,
  an IPv4 network written as IPv4 (an IPv4-mapped one is refused, naming its IPv4 form). Left out,
  the provider never reads nor changes them.

## [0.8.0] - 2026-11-26

For Stowage 0.49.0 or later with `restore`: only an export (`export: true`) makes an install made
again; a secrets file without it seeds a plain install.

### Changed

- **BREAKING**: `restore` needs a release whose installer is built with Stowage 0.49.0 or later.
  A create with `restore` whose `secrets_wo` reads as a secrets file without `export: true` is
  refused at plan (`secrets_wo is no secrets export`): a new install's secrets make a plain
  install, which awaits no data, and its backup settings, set before the restore, would send its
  backups to the old destination. An export an installer built with Stowage 0.48.0 or earlier
  wrote lacks the mark: add the line `export: true` to the decrypted file.
- `secrets_wo` also takes a new install's secrets file (no `export: true`), as the installer does
  from Stowage 0.49.0. A VM create on an installed VM given such a file is refused (`The VM holds
  an install of its own`), naming the way on: remove `secrets_wo` if an earlier apply's install
  took it.

## [0.7.0] - 2026-11-26

For Stowage 0.48.0 or later with `restore` (from 0.48.0 every install made by `--secrets` awaits its
data, which a restore requires) and 0.47.0 or later with `backup.keep_local`.

### Added

- A lost machine made again in one apply, on `stowage_docker_install` and `stowage_vm_install`:
  `backup` declares the install's backup settings (a destination with the write-only
  `backup_rclone_config_wo`, or `keep_local`; recipients, retention, schedule, profile, lock),
  set at create and whenever they change, and read back by every refresh, so a change made on
  the target shows in the plan; `restore` (with the write-only `restore_identity_wo`) loads a
  backup into the install a create makes from `secrets_wo`, then runs its provision hook. A
  restore is taken in that create alone: one that fails leaves the install created, with a
  warning, and the next apply runs it again; a restore loads only while the install awaits its
  data (`status --json`, read right before it), never over an install's own data.

### Changed

- A VM create given `secrets_wo` reads the VM's `status --json` first: an install an earlier apply
  made from the export, which awaits its data, is not given it again (the installer would refuse
  it), and an install of the VM's own is refused before anything runs, naming the way on.
- **BREAKING — a Docker create retried with its kit lost no longer waits out a first install
  marked as running**: an install there (Docker's status answers only once one is written)
  needs no kit, so the create runs its re-run as for any install there — the installer refuses
  one beside a run still going, and an apply once it ends goes on. Before, it was refused
  (`The first install is still running`), and a mark a timeout left needed the kit attributes
  taken out.

### Security

- The signature verifier's sigstore libraries are raised to the release that dropped the
  unmaintained OpenPGP package flagged by GO-2026-5932, and `golang.org/x/crypto` to 0.59.0.

## [0.6.0] - 2026-11-25

For Stowage 0.46.0 or later on the installs that rotate a secret (`describe --json`'s `secrets`).

### Added

- `secret_rotations` on every install resource: a number per generated secret (`{ SESSION_SECRET = 1 }`),
  as `random_password`'s `keepers`. Changing a key's number rotates that secret in the apply, after
  the update's other steps: in one pass for a secret its release marks `replace: true` (Docker and
  the VM), in its three passes for one marked `rotation: true` (every target). A key's first number
  rotates nothing. A key the release cannot rotate on the target is refused at plan once the
  provider has read the release's ways (at the create, or a rotation), and by the apply, before any
  rotation, otherwise; a rotation whose passes stop part-way resumes at the failed pass in the
  next apply.

## [0.5.0] - 2026-11-24

For Stowage 0.44.0 or later on the installs that issue a kit (`kit create --json --request` on
every shape, `describe --json`'s `handoffs`).

### Added

- Handoff kits, the whole cycle in one apply with no verb run by hand: `stowage_kit` issues a kit
  on an install (the request, a secret answer included, never on a command line), the ephemeral
  `stowage_kit` hands it, in the same apply, to the new `kit_wo` of an install resource (its first
  install) or to `stowage_kit_take` (an install that exists takes it by its re-run, as a kit going
  back to the install that issued the first one), and `stowage_kit_check` proves it, tried until the
  issuer's check answers. The kit and its code are kept in the provider's process alone: never in a
  file of the root, the state, a plan or a log (the runs that issue and take one pass it through a
  private temporary directory, removed when they end). A guide and a whole root, `examples/guides/handoff-chain`.
- A refresh of an install reads the value it records for each input the configuration gives: one
  whose record a kit (or a run by hand) changed since the refresh before shows in the next plan,
  which gives the configuration's back, where it stayed apart unseen.

### Changed

- **Breaking**: `kit_file` is gone from the install resources. A kit is given by `kit_wo` (write-only)
  with `kit_code_wo`, and `kit_wo_version`, which says a kit is expected: a kit made by hand is
  `kit_wo = file("x.kit")`, with `kit_wo_version` set to any value naming it.

## [0.4.0] - 2026-11-23

### Added

- A VM upgrade gives the new release the inputs and secret inputs only it declares, with the
  upgrade itself, so the boot into it takes them (Stowage 0.43.0 or later on the VM's OS): a
  release adding a required input with no default is reached in one apply, where its boot used to
  refuse and the VM went back. The plan's dry run is given them too. On an older OS the upgrade
  runs without them and the re-run after its reboot gives them, as before, with a plan warning
  naming the way on.

### Fixed

- An update of a VM that a resource of the same apply restarts (a TrueNAS VM's memory changed by
  its power resource) waits, within the update timeout, while ssh fails as a VM down or booting
  fails it, its host daemon is not up yet or its boot is not settled, where it failed at once
  (`No route to host`, or the boot still bringing the install up); so does the plan's dry run of a
  release change, within its bound. A VM that is off, or a host or port nothing answers on, now
  waits out the timeout; a host that does not resolve, a refused login or another host key still
  fail at once.

## [0.3.0] - 2026-11-16

For Stowage 0.40.0, which leaves VM creation to OpenTofu: the provider makes no VM, and every
platform has a complete root among its examples. Proven against Stowage 0.40.0's tree: the Docker
and Kubernetes targets (`make acc-docker`, `make acc-k8s`, no Docker on the machine running
OpenTofu), the libvirt root (`make acc-libvirt`) and the TrueNAS root applied for real (`make acc-vm`).

### Changed

- **`stowage_install` is gone**: one resource type per shape, `stowage_docker_install`,
  `stowage_kubernetes_install` and `stowage_vm_install`, each with its target's settings at the top
  (no `docker`, `kubernetes` or `vm` block), and the data source split the same way. There is no
  compatibility before 1.0: for each install, `tofu state rm stowage_install.<name>`, then `tofu
  import` it into its new type — Docker `[<host>:]<work_dir>/<name>/<short>-stow`, Kubernetes
  `<namespace>/<release>` (no `kubernetes:` prefix), VM `<host>/<short>-stow` (no `vm:` prefix) —
  and apply once. A VM's `vm.version`, `vm.channel` and `vm.short_name` are `version`, `channel`
  and `short_name`; its type has no `installer`, `address` nor `delete_data`; a Kubernetes install's
  has none of the front's settings nor `secrets_wo`.
- **No Docker on the machine running OpenTofu**: `stowage_vm_seed` and the Kubernetes runs read
  the installer image from its registry — its entry for the machine's own platform (linux/amd64,
  linux/arm64, darwin/amd64, darwin/arm64; one it has none for is refused, naming those it has) —
  streamed into the cache by the image's digest, and run natively. A Kubernetes
  run needs `helm` and `kubectl` alone. The Docker target still runs `docker run` on its host.
- Every example's root is the platform's own: `examples/guides/<platform>-vm` for TrueNAS
  (`truenas/truenas` 1.9.1 with Stowage's `vm truenas` steps), Proxmox VE, libvirt, VMware, AWS,
  Azure, Google Cloud and Scaleway, each with its import IDs for a VM Stowage 0.39.0 or earlier
  made; the TrueNAS guide's SSH step script and the community TrueNAS provider are gone.

### Added

- `stowage_vm_seed`'s `ctl_path` (the release's installer for this machine, which a root's steps
  run), `size` (the VM's least and recommended CPUs, memory and app space, which the examples
  refuse under and warn under), `firewall` (what a cloud's firewall lets through) and
  `await_given`.
- `registry_login_wo` with `registry_login_wo_version` on the three install types: the install's
  own registry login, write-only — Docker's and Kubernetes's `--registry-login-file`, a VM's
  `registry-login` over SSH — sent again when the version changes, and taken out of state by a
  refresh that finds the install holds none.
- `acme_ca` and `acme_ca_root` on Docker and the VM (`--acme-ca`, `--acme-ca-root`), read back by a
  refresh.
- `kit_file` with write-only `kit_code_wo` on the three install types: a handoff kit given to the
  first install (`--kit`).
- `stowage_vm_install`'s `interfaces`: a NIC the platform attached after the seed named and
  addressed by `network attach` over SSH, the VM rebooted and its boot followed; and `platform`,
  with which a wait that runs out names the platform's console and a destroy's plan warns, before
  it is approved, when the registry holds no disk set to make the VM again.
- A Docker example (an EC2 host made by cloud-init, its SSH key pinned from the first connection)
  and a Kubernetes example (a kind cluster, ingress-nginx by `helm_release`); a guide to running a
  root without the internet (`tofu providers mirror`, Stowage's `mirror export` and `import`).
- `pull_login` on the three data sources; a Kubernetes data source.
- The provider's `public_keys`: every installer it fetches must carry a signature by one of them
  (a cosign signature over the image's digest, checked as Stowage checks one), and its release
  keys of its own. An upgrade over a Docker or Kubernetes install is checked against the keys of
  the release installed, read from its installer by the digest the install records, as Stowage's
  own `upgrade --version` checks one — on Docker, read from the installer the install keeps when
  the registry cannot give them; a first install without `public_keys` trusts the digest alone,
  which its plan warns of, as it warns that an upgrade over a release carrying no keys is checked
  by the digest alone too. On Docker the check runs before the target pulls anything.

### Fixed

- A VM's first boot that failed in Kairos's recovery system (`reset_failed`) fails the create at
  once instead of waiting out the timeout.
- The pull account goes only to the registry of an installer the configuration names, or of the
  provider's `registry`: never to a host a target or a release reported.
- A create whose NIC attach fails, or that cannot reach the VM under its keys for it, records the
  install as planned, its interfaces pending: the next plan is an update that attaches them (it
  was an error after apply, which planned the install's replacement).
- An imported VM's first apply attaches only the NICs the VM does not name already by their MAC,
  read from it, and never reattaches or reboots for one it has: a role addressed otherwise than
  `interfaces` gives is a warning naming the `network set --role` that aligns it, and state records
  the configuration. A release change beside NICs to attach is refused before anything runs, at
  the plan's dry run too; one beside NICs the VM already names is not.
- The update attaching NICs left pending plans the VM's keys unknown: it may learn its own key and
  retire the one-time key (it was an inconsistent result).
- A create that re-runs an install already there gives it the kit only while nothing is installed
  yet (a first install left unfinished), as Stowage's re-run takes `--kit`.
- A Kubernetes run or a seed runs its own hard link to the cached installer (a copy, renamed into
  place, on a file system that takes no link), in a private directory inside the cache (removed
  after the run, or a day later when a run was cut off), which another run's prune of the cache
  cannot remove under it; a run whose program was busy (`text file busy`: a program forked
  meanwhile holding a copy open for writing) is made again, a few times within a second; part
  files a fetch cut off left are removed.
- A Kubernetes run's kubeconfig and working directory are in the system's temporary directory,
  never in the cache: only the installer's program is there.
- An upgrade over a release installed that carries no key says so in a warning (the upgrade checked
  by `public_keys`, or by its digest alone). Keys of the release installed that cannot be read
  refuse the upgrade before anything is fetched or run on the target, naming `public_keys` among
  the fixes; with `public_keys`, the new installer is checked against them alone, said in a warning.
- A create over an install already there names the kit in its warning only when `kit_file` is
  given (given while nothing is installed yet).
- An imported VM's first apply whose NIC attach fails records the roles it took over, so the next
  apply attaches only the others; it waits for the VM's SSH first, as an update attaching NICs
  does; a `dhcp` role with no address yet is not taken for one addressed otherwise.
- NICs attached whose reboot the health gate found unhealthy are recorded attached (a warning at
  create, an error at an update), and never attached again.
- The wait for a VM's SSH before attaching NICs stops at once on a refused login or another host
  key, with the read's ways on, and its timeout names the bound it ran out (the plan's dry run's
  10 minutes at plan).
- A VM whose status reports no `boot_id` or no event journal refuses the NIC attach before any
  NIC is attached (it attached them, then refused the reboot).
- On Docker, an upgrade over an install that records no installer is refused, without
  `public_keys`, before the new installer is fetched or run on the target; with them, the same
  release is taken over as before.
- `registry_login_wo_version` without `registry_login_wo` is refused at validation.
- A VM's destroy no longer waits on a VM that does not answer: its disk-set check, at the
  destroy's plan, connects within 10 seconds, and says it was not checked otherwise.
- The hypervisor roots (TrueNAS, Proxmox VE, libvirt, VMware) give every NIC the release names the
  MAC `stowage_vm_seed`'s `nics` gives it, the one NIC of a root without `nic_roles` too (the
  platform's, or the root's own, when the seed gives its one role none: the primary's alone), and
  refuse at plan a role in `nic_roles` the seed gives no MAC and two NICs sharing a MAC. Their
  `nic_macs` (role → MAC) gives a role its MAC, to the seed and the NIC alike, and is the refusal's
  fix.
- The TrueNAS root's destroy step names `tofu apply -refresh-only` when the installer is not on
  the machine.

## [0.2.0] - 2026-11-15

The provider is in **beta**: the Docker, Kubernetes and VM targets are proven by their acceptance
tests (Docker and the VM against Stowage 0.35.0, Kubernetes against 0.35.0 and 0.37.0; the VM's
import, and the plan-time dry runs on Kubernetes and the VM, are proven by unit tests until the
acceptance walks run against Stowage 0.38.0); its schema and behaviour may still change before 1.0,
each change named here.

### Added

- `plan_dry_run` on Kubernetes and the VM: a plan changing the release runs the new installer's `k8s
  upgrade --dry-run` (or `k8s rollback --dry-run`), or the VM's `upgrade --version V --dry-run` (or
  `rollback --dry-run`) over SSH, so a refusal is a plan error; the VM's OS needs Stowage 0.38.0 or
  later.
- Taking over an imported install: its first apply reads what the install records (`status --json
  --values`, Stowage 0.38.0 or later) and re-runs it with what the configuration gives otherwise
  alone — a configuration that matches re-runs nothing; a secret input the install records is
  kept, one it does not is sent. A TLS mode the install serves that the configuration leaves out,
  or the reverse, is refused. With `plan_dry_run`, the plan of an imported state runs too and says
  which keys differ (`Taking over the install: …`); a rollback of an imported install is refused at
  plan as at apply.
- `kubernetes.values`: a chart values file (YAML) given to helm after the installer's own at the
  install, every upgrade and every re-run (`k8s install|upgrade --values`); a change alone applies
  the release again, a change beside a rollback is refused, a rollback gives them again after it
  (helm's takes back the values of the release it reaches), and leaving it out keeps the release's
  values (a key given `null` drops one).
- `stowage_vm_seed.nics`: the NICs the VM is made with, by role — the MAC its cloud-config names
  each by (given or generated), its network and the primary — for the VM's resource to give each
  NIC its MAC (Stowage 0.38.0 or later; null, with a warning, from an earlier installer).
- A guide, with its root in `examples/guides/truenas-vm`: a Kairos VM made on TrueNAS SCALE by the
  upstream TrueNAS provider and a small SSH step script (the disk's upload and conversion, the
  NoCloud seed ISO, the power), no module of Stowage's.

### Changed

- `delete_data = true` with a `vm` block is refused at validation (`delete_data is not the VM's`): a
  VM's destroy forgets the install and deletes nothing, its data going with the VM.
- A read of a Docker host or VM that ssh fails (exit 255) is an error naming what ssh met and the
  way on: `ssh's login to … was refused` (a fix of the user or key file saved by `tofu apply
  -refresh=false`, which saves a change of the connection alone), `… serves another host key` (on
  Docker, and on a VM whose own key is not recorded yet, the new key the same way; else on a VM
  made again a new install, on one whose own key changed the new key and a new import), else
  `Cannot reach … to read the install` (plan again once it answers); each names `tofu state rm`
  for a target known to be gone and `-refresh=false` for one plan. The install stays in state, as
  a host down for a while is not told from one gone.

### Fixed

- The Cloudflare token of the `dns-cloudflare` TLS mode (`STOWAGE_CLOUDFLARE_API_TOKEN`) counts as a
  declared secret input (Stowage 0.38.0's `describe` lists it): given in `inputs` it is refused, as
  it would land in state; given in `secret_inputs_wo` it is no longer sent again with every
  upgrade; exported in OpenTofu's environment it is kept out of a local installer's run.
- An imported install's first apply, the provider given `registry_ca`, no longer fails with an
  inconsistent result on `registry_ca_digest`: the plan shows it known after apply, and the apply
  records the registry CA the install keeps.

### Documentation

- Installing a GitHub release's signed zip into OpenTofu's local mirror, on any platform it is
  built for, its signature checked against the release key alone (`gpgv`, the key file first
  checked to hold that key and no other); the quick starts take any 0.x release (`~> 0.1`).

## [0.1.0] - 2026-11-12

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
