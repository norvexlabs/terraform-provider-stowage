# TODO

Pending work, prioritised MoSCoW for the **first release**. Its goal is DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider"): the provider drives the three shapes through the
installer's machine interface and decides nothing itself. A finished item is deleted, not ticked.

## Must — the first release

1. **The verbs' `--json` result in Stowage** (blocks every apply past `describe`): Stowage's
   installers do not yet print `{format, result, release, refused}` nor exit 3 on a refusal
   (DESIGN § 6, its next slice). Until then a verb exiting 0 without the object is reported as
   failed. Once it lands, prove create, update (re-run, upgrade, rollback) and delete against a
   real installer (item 6) — the partial state an update saves before a failed upgrade included,
   which OpenTofu accepts with the error.
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

## Could

- A registry CA for the installer image's own fetch (`/etc/docker/certs.d` on the target), today
  the target's Docker daemon's own configuration.
- `stowage_install` data source reading `status --json` without managing the install.
