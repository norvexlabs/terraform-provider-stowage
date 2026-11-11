# TODO

Pending work, prioritised MoSCoW for the **first release**. Its goal is DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider"): the provider drives the three shapes through the
installer's machine interface and decides nothing itself. A finished item is deleted, not ticked.

## Must — the first release

2. **Plan-time dry run** (P1, opt-in): an upgrade's `--dry-run` over SSH, skipped while the target
   does not exist yet.
4. **Run `make acc-vm`** (written, never run): `stowage_vm_seed` and the VM target against a real
   VM on TrueNAS with Stowage ≥ 0.34.0 — the seed, the module's VM, the first install over SSH
   under the one-time key, the own key read, a re-run, an upgrade and its reboot, the rollback,
   the destroy. Fix what it finds; until then the VM target is proven by unit tests alone.
5. **Docs** (P4): `make docs` (tfplugindocs) into `docs/` from the schema and `examples/`; needs a
   way to give tfplugindocs OpenTofu's schema instead of downloading Terraform.
7. **First release** (P5): the public repository `norvexlabs/terraform-provider-stowage`, created
   by the maintainer with the org's GitHub App and its ruleset (pushes as the app only, like
   Stowage); a GPG key for the release (`GPG_FINGERPRINT`); a release workflow (GoReleaser
   on a pushed tag); registry submission by hand.

## Should

- **`make acc-docker` ships with the host's Docker uncapped**: `stowage ship` builds the example's
  images with this machine's builder, whose memory is not capped as the test's containers are.

- **A flaky test**: `TestCreateWaitsForTheFirstInstall` failed once in `make check` on a loaded
  host and passed on the re-run; its 20ms create timeout races the fake's status loop. Scale it
  with an unexported unit, as the test rules ask.
- **A short name changed across releases**: state's `short_name` is the running release's after
  a rollback, and the kept `*-stow` fallback runs only when `<short>-stow` is missing; with both
  files left the stale one runs. Re-derive the short name after a rollback (or ask Stowage to
  report it in `status --json`).
- **A rollback's values**: DESIGN § 13 says a rollback restores the previous values; Stowage's
  README says every other value stays as it is now (only the values `.env` lacks come back).
  State keeps the configured inputs after a rollback; settle with Stowage which holds, and what
  `status` should report.
- **A port for an existing route set in the same apply as an upgrade** goes to the upgrade's
  `--ports` (taken as a route the release adds), which refuses a recorded route. `describe` does
  not list routes: ask Stowage to, and decide by it.

- **`registry_ca` is first-install only** (`describe` `first_install_only`), but state does not
  record it: a changed provider `registry_ca` is silently not applied (a re-run is never given
  it). Record its digest and make a change a plan error, as `registry`.
- **An imported state's `address`** is never compared (status does not report it): one given
  in config that differs from the install's is not caught, as the re-run is never given it.

- **The minimum Stowage release** (`installer.MinStowage`, 0.34.0) is the release expected to ship
  the machine interface: confirm at that release.
- **An installer older than 0.34.0 is not told positively**: it has no `describe` (cobra's
  unknown command, exit 1), so the provider can only say that a failed `describe` may be one.
  Ask Stowage whether an old installer's failure can be told (it cannot be changed after the
  fact) — else this stays a hint in the diagnostic.

- **A pull account's login drops the target's Docker context**: the private `DOCKER_CONFIG` holds
  no `currentContext`, so with `username` the fetch talks to the default daemon (a rootless
  context's user fetches into another one). Carry the context's endpoint over (`DOCKER_HOST`).

- **A create over an existing install records the configuration's first-install-only settings**
  (`address`, the provider's `registry`) though the install keeps its own: state may name values
  never applied. Ask Stowage to report them in `status --json`, and compare.

- **Blocked on Stowage — what the VM target asks of it** (the provider fills none with a rule of
  its own; each is asked of Stowage):
  - **A verb that retires the one-time host key** and prints the VM's own key (e.g. `sudo
    <short>-stow seed-key retire --json` → `{known_hosts_line}`), or `install --json`'s result
    doing it at the install's end, as `vm create` does over its pinned SSH. Until then the
    provider runs the steps README documents (the key's file and drop-in removed, the `.retired`
    mark, sshd reloaded): a copy of that rule, to drop once the verb exists.
  - **`vm seed --secrets-awaited`** (the settings file's `await_secrets: true`): a first boot that
    waits for a secrets export, so the VM target can take `secrets_wo` as DESIGN § 13 says
    (Docker and the VM). Until then it is refused on the VM — a departure from DESIGN § 13.
  - **A seed whose first install waits for TLS material** (`vm seed --tls byo`, the cloud-config
    marking the wait as `vm create`'s given mark does): today the first boot installs in another
    mode and `given_wo`'s certificate moves it to `byo` after.
  - **`meta_data` in `vm seed --json`**: the NoCloud `instance-id` and `local-hostname` Stowage
    writes, for a hypervisor that builds its own seed (libvirt's `libvirt_cloudinit_disk`,
    Proxmox's `cicustom`); the example's `seed-iso.sh` writes them itself.
  - **The disk as a file, and its size**: a verb that fetches `disk[<platform>]` to a file and
    prints its path and SHA-256, and `os_gib` (what Kairos takes of the VM's disk) in `vm seed
    --json`; the example reads them from the documented artifact form (oras, zstd, the
    `dev.stowage.disk.os-mib` annotation).
  - **A cloud's image** in `vm seed --json`: the image ID `ship` registered (`cloud_publish`) per
    cloud platform, which `aws_instance`, `google_compute_instance`… boot.
  - **`vm seed`'s refusal as an object**: exit 3 prints the reason on stderr alone, so the fix
    Stowage chose (the platforms the registry offers) never reaches a program; ask for the verbs'
    `{format, result, refused: {reason, fix}}` on its stdout.

## Could

- **A re-run's plan keeps `version`, `revision`, `channel` and `previous` from state**: an install
  changed outside OpenTofu after the last refresh (`-refresh=false`) makes the apply's result
  inconsistent with the plan. Plan them unknown on every change of the target, at the cost of a
  noisier plan.
- **`status --json` exit 3 is taken as "no install" before a create**: a refusal for another
  reason makes the create a fresh install's, which the installer then refuses.
- **`docker.work_dir` is compared as spelled**: `/home/u` and `.` (the same home) differ.
- **An input moved to `secret_inputs_wo` without a new version** is refused at apply, not at plan:
  move the check to ModifyPlan (the configuration's write-only values are readable there).
- **A run that outlives its program** (a child holding the output) returns what that child wrote
  too, which a `--json` reader then refuses as data after the object.
- **A run cut by its timeout leaves the program on the target running** (no tty, no SIGHUP over
  ssh); stopping it would need the installer's own cancellation.

- A registry CA for the installer image's own fetch (`/etc/docker/certs.d` on the target), today
  the target's Docker daemon's own configuration.
- `stowage_install` data source reading `status --json` without managing the install.
