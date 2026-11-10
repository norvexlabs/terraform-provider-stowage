# TODO

Pending work, prioritised MoSCoW for the **first release**. Its goal is DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider"): the provider drives the three shapes through the
installer's machine interface and decides nothing itself. A finished item is deleted, not ticked.

## Must — the first release

1. **Update: inputs, then the release** (P1). Today `Update` refuses any change the target must
   apply (`Changing … of an install is not implemented yet`). Implement DESIGN § 13's order: the
   inputs first, with the running release's installer (`<short>-stow --set …`, the secret inputs
   by answers file when `secret_inputs_wo_version` changed), state saved; then the release, with
   the new installer fetched by digest and its `upgrade --no-input --json`. A failure between
   leaves the reconfigured release in state, so the next plan shows the upgrade alone. Inputs a
   new release adds go with its upgrade. Remove the input keys from `inputs` that the target no
   longer records only through the installer (a key removed from config: decide `--set KEY=`
   versus leaving it, from the installer's rule for an empty value).
2. **Rollback** (P1): only when the desired `installer` is `previous.installer` and
   `allow_rollback` is set, `rollback --json`; any other older release goes to the installer,
   whose refusal (exit 3) is the diagnostic.
3. **Import** (P1): `tofu import stowage_install.x <host>:<work_dir>/<name>` reads the install
   (`status --json`), the connection given in config.
4. **Plan-time dry run** (P1, opt-in): an upgrade's `--dry-run` over SSH, skipped while the target
   does not exist yet.
5. **Kubernetes target** (P2): `k8s install|upgrade|uninstall --json` run locally (kubeconfig
   and context, helm ≥ 3.14, kubectl, a linux/amd64 runner); the installer fetched locally by
   digest; `k8s upgrade --given -` for a recorded input; no `secrets_wo` (refuse it).
6. **`stowage_vm_seed` and the VM target** (P3): the seed as a managed resource (`vm seed --json`,
   the one-time host key's private key in state until the install), the VM's `install --set`,
   `upgrade --version V --yes` followed by the reboot (a new `boot_id`, the trial over,
   `installed` the target; a fallback or a failed boot an error saving the release that runs).
7. **Docs** (P4): `make docs` (tfplugindocs) into `docs/` from the schema and `examples/`; needs a
   way to give tfplugindocs OpenTofu's schema instead of downloading Terraform.
8. **Acceptance scripts against `tofu`** (P4): sshd + Docker-in-Docker for the Docker target,
   kind for Kubernetes, one TrueNAS VM; run in Docker like the gates, with a memory cap.
9. **First release** (P5): the public repository `norvexlabs/terraform-provider-stowage`, created
   by the maintainer with the org's GitHub App and its ruleset (pushes as the app only, like
   Stowage); a GPG key for the release (`GPG_FINGERPRINT`); a release workflow (GoReleaser
   on a pushed tag); registry submission by hand.

## Should

- **A failed create leaves no state**: an install that failed half-way (exit 1, `installing`) is
  forgotten, so a destroy cannot clean it; the next apply re-runs the install, which finishes it.
  Saving it tainted would make the next apply destroy it (refused by `deletion_protection`).
  Decide with the Update slice.
- **`describe` carries no `short_name`**: Read finds the kept installer as the one `*-stow` in the
  install directory. Ask Stowage to add `short_name` to `describe --json` (DESIGN § 6) and use it.
- **`status --json` `format`**: DESIGN gives `format` to `describe` and the verbs' results, not to
  `status`; this provider checks it when present. Settle with Stowage whether status carries it.
- **The minimum Stowage release** (`installer.MinStowage`, 0.34.0) is the release expected to ship
  the machine interface: confirm at that release.

## Could

- A registry CA for the installer image's own fetch (`/etc/docker/certs.d` on the target), today
  the target's Docker daemon's own configuration.
- `stowage_install` data source reading `status --json` without managing the install.
