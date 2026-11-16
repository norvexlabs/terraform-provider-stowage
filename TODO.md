# TODO

Pending work, prioritised MoSCoW. The bar is a **production-ready beta**: DESIGN § 13 of Stowage
(`docs/DESIGN.md`, "OpenTofu provider") on the three shapes, each proven by its acceptance test,
installable and verifiable by a user. A finding (a bug, a wrong behaviour, a security hole, a
review's leftover, a weak test) is never written here: it is fixed in the slice that finds it. A
finished item is deleted, not ticked.

## Must — the beta

1. **The OpenTofu registry** (by hand, the maintainer, as the org): submit
   `norvexlabs/terraform-provider-stowage` and the release key to the OpenTofu registry, so `tofu
   init` finds `norvexlabs/stowage` without a local mirror; then README's and the install guide's
   "not in a registry yet" go. 0.1.0 is released on GitHub, signed; the registry has no
   `norvexlabs/stowage` yet.
2. **The acceptance walks against Stowage 0.40.0's release** (the maintainer, once 0.40.0 is
   published): `make acc-docker`, `acc-k8s` and `acc-vm` against the released Stowage (each passed
   against its unreleased 0.40.0 tree), and `make acc-libvirt` (written, not run yet).
3. **The platform examples applied for real** (each needs an account or a host, outside this
   repository): Proxmox VE, VMware vSphere, AWS, Azure, Google Cloud and Scaleway (`examples/guides/
   <platform>-vm`), the Docker root on EC2 (`docker-aws`) and the Kubernetes root (`kubernetes-kind`):
   each stays a preview until then. libvirt: on a KVM host with AppArmor enforcing, with several
   vCPUs, and a failed start's domain; the install over SSH (`acc-libvirt` stops at the domain,
   QEMU without KVM being too slow to answer).
4. **The darwin installers on a Mac**: `stowage_vm_seed` and the Kubernetes runs from macOS (the
   darwin entries of the installer index; Stowage proves them by structure alone until one runs on
   a Mac).
5. **A NIC added after the seed, proven on TrueNAS**: `extra_nics` → `truenas_vm_power` restart →
   `interfaces` (`network attach`, the reboot followed), needs a release naming a secondary role
   (`host.interfaces`); the acceptance project declares none.

## Should

1. **Published modules per platform**: `terraform-<target>-stowage-vm`, one repository per platform
   (the OpenTofu registry's naming), extracted from `examples/guides/<platform>-vm` once each is
   proven, each submitted through the registry's form.
2. **TrueNAS without the stopgap**: once `truenas/truenas` writes a file to the host, converts an
   image into a zvol and stops a VM before deleting its devices, the TrueNAS root drops Stowage's
   `vm truenas` steps (they are Stowage's TODO too).
