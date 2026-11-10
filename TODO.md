# TODO

Pending work, prioritised MoSCoW for the **first release**. Its goal is DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider"): the provider drives the three shapes through the
installer's machine interface and decides nothing itself. A finished item is deleted, not ticked.

## Must — the first release

1. **Prove the Docker target against a real installer** (Stowage ≥ 0.34.0, with item 6): create,
   update (re-run, upgrade, rollback), delete — a refusal (exit 3) and the partial state an
   update saves before a failed or refused upgrade included, which OpenTofu accepts with the
   error. Unit tests drive the results as Stowage's `printResult` prints them.
2. **Plan-time dry run** (P1, opt-in): an upgrade's `--dry-run` over SSH, skipped while the target
   does not exist yet.
3. **Kubernetes target** (P2): `k8s install|upgrade|uninstall --json` run locally (kubeconfig
   and context, helm ≥ 3.14, kubectl, a linux/amd64 runner); the installer fetched locally by
   digest; `k8s upgrade --given -` for a recorded input; no `secrets_wo` (refuse it).
4. **`stowage_vm_seed` and the VM target** (P3): the seed as a managed resource (`vm seed --json`,
   the one-time host key's private key in state until the install), the VM's `install --set`,
   `upgrade --version V --yes` followed by the reboot (a new `boot_id`, the trial over,
   `installed` the target; a fallback or a failed boot an error saving the release that runs).
5. **Docs** (P4): `make docs` (tfplugindocs) into `docs/` from the schema and `examples/`; needs a
   way to give tfplugindocs OpenTofu's schema instead of downloading Terraform.
6. **Acceptance scripts against `tofu`** (P4): sshd + Docker-in-Docker for the Docker target,
   kind for Kubernetes, one TrueNAS VM; run in Docker like the gates, with a memory cap.
7. **First release** (P5): the public repository `norvexlabs/terraform-provider-stowage`, created
   by the maintainer with the org's GitHub App and its ruleset (pushes as the app only, like
   Stowage); a GPG key for the release (`GPG_FINGERPRINT`); a release workflow (GoReleaser
   on a pushed tag); registry submission by hand.

## Should

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
