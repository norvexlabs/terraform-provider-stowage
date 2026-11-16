---
page_title: "Without the internet - stowage"
subcategory: "Guides"
description: |-
  Running a root on premises without internet: the providers from a filesystem mirror, the installer and the disks from an exported Stowage registry.
---

# Without the internet

A root on premises reads nothing from the internet once three things are on its side: the
OpenTofu providers it requires, the project's registry content (the installer image, the release's
images, its disks), and the tools the runs call.

**The providers**, from a machine with the internet, into a directory you carry over:

```bash
tofu providers mirror -platform=linux_amd64 -platform=darwin_arm64 ./providers   # in the root: every provider it requires, at its lock file's versions
```

`norvexlabs/stowage` is not in a registry yet: put its release zip (or `make install-local`'s
build) into the same tree, `providers/registry.opentofu.org/norvexlabs/stowage/<version>/<os>_<arch>/`
([installing without the registry](install-without-registry.md)). On premises, a CLI configuration
reads them from there alone:

```hcl
provider_installation {
  filesystem_mirror {
    path = "/opt/tofu/providers"
  }
}
```

`tofu init` then checks each provider against the root's `.terraform.lock.hcl` (`-platform` above
must cover the machine's, for its checksums to be there).

**The registry**, exported by Stowage and imported into a registry of your site (Stowage's
[mirror](https://github.com/norvexlabs/stowage/blob/main/docs/mirror.md)): `<short>-stow mirror
export --output r.tar --disks <format>` where the project's registry is reachable, then `mirror
import r.tar --to <site>/<project> --registry-ca <CA>` on premises. The export carries every entry
of the installer image (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64), their signatures,
the release's images and its disks. Then the provider reads the site's registry:

- `installer` names the site's copy, `<site>/<project>/<name>-installer@sha256:…` (the same
  digest: an export keeps it);
- the provider's `registry` is the site's prefix, `registry_ca` its CA, `registry_username` and
  `registry_password` its read account: `stowage_vm_seed` and the Kubernetes runs read the
  installer (the entry for this machine, checked as the [trust guide](trust.md) says) and the disks there,
  a Docker target and a VM pull from there, and `stowage_vm_install`'s `registry_login_wo` gives a
  VM its own login to it;
- a cloud's image is registered from the disk the seed wrote (`disk_dir`) by the example's
  `image/` root, whose own provider must reach the cloud.

**The tools**: `ssh` (OpenSSH ≥ 8.5) for every remote target; `helm` (3.14 or later) and `kubectl`
for Kubernetes; `xorriso` and `jq` for a hypervisor's seed ISO. No Docker on the machine running
OpenTofu.
