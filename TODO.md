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
2. **Run `make acc-vm`** (TrueNAS, ~1 h, scheduled by the maintainer): `test/acc-vm.sh` now walks
   the VM's import, its first apply under the VM's own key and a destroy refused by
   `deletion_protection`, written and never run yet.

## Should

None.
