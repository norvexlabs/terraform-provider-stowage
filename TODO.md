# TODO

Pending work, prioritised MoSCoW. The bar is a **production-ready beta**: DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider") on the three shapes, each proven by its acceptance test,
installable and verifiable by a user. A finished item is deleted, not ticked.

## Must — the beta

1. **The OpenTofu registry** (by hand, the maintainer, as the org): submit
   `norvexlabs/terraform-provider-stowage` and the release key to the OpenTofu registry, so `tofu
   init` finds `norvexlabs/stowage` without a local mirror; then README's and the install guide's
   "not in a registry yet" go. 0.1.0 is released on GitHub, signed; the registry has no
   `norvexlabs/stowage` yet.
2. **Acceptance coverage of import and destroy safety off Docker**: `make acc-docker` walks an
   import and its first apply, `deletion_protection`'s refusal, a secret input and a refused
   upgrade; `make acc-k8s` walks none of them, and `make acc-vm` no import
   (`vm:<host>/<short>-stow`). Add them to `test/acc-k8s.sh` and `test/acc-vm.sh` and run both
   (acc-vm needs TrueNAS, ~1 h): until then those paths are proven by unit tests alone.

## Should

1. **A plan-time dry run on Kubernetes**: `plan_dry_run` is Docker's alone (refused elsewhere);
   Stowage's `k8s upgrade --dry-run` and `k8s rollback --dry-run` exist, run locally like every
   Kubernetes verb.
2. **A plan-time dry run on the VM** — blocked on Stowage: the VM's `<short>-stow upgrade` and
   `rollback` have no `--dry-run` (Stowage 0.37.0), which DESIGN § 13's "an upgrade's dry run over
   SSH" needs. Raise it with Stowage.
