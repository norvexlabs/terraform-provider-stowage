# TODO

Pending work, prioritised MoSCoW for the **first release**. Its goal is DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider"): the provider drives the three shapes through the
installer's machine interface and decides nothing itself. A finished item is deleted, not ticked.

## Must — the first release

4. **Run `make acc-vm`** (written, never run): `stowage_vm_seed` and the VM target against a real
   VM on TrueNAS with Stowage ≥ 0.35.0 — the seed (its disk written by `disk_dir`), the module's
   VM, the first install over SSH under the one-time key, the own key read, a re-run, an upgrade
   and its reboot, the rollback, the destroy. Fix what it finds; until then the VM target is
   proven by unit tests alone. `make acc-docker` and `make acc-k8s` run again against Stowage
   0.35.0 too.
7. **First release** (P5), on the maintainer's decision: a GPG key for the release
   (`GPG_FINGERPRINT`); a release workflow (GoReleaser on a pushed tag); registry submission by
   hand.
