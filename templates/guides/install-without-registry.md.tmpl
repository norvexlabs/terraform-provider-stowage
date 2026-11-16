---
page_title: "Installing without the registry - stowage"
subcategory: "Guides"
description: |-
  The provider is not in a registry yet: install a release's signed zip, or a checkout's build, into OpenTofu's local mirror, or point OpenTofu at the build.
---

# Installing without the registry

The provider is not published to a registry yet. Its releases are on GitHub, signed; or build it
from a checkout of `terraform-provider-stowage` (Docker is the only requirement). Either is given
to OpenTofu through its implied local mirror; a checkout's build also through `dev_overrides`.

## A release's zip, in the implied local mirror

Each [release](https://github.com/norvexlabs/terraform-provider-stowage/releases) holds
`terraform-provider-stowage_<version>_<os>_<arch>.zip` for linux, darwin, windows and freebsd,
`terraform-provider-stowage_<version>_SHA256SUMS` and its GPG signature by the release key
(fingerprint `686D3351040F9F4463D9D3AC0C705E9BAA4F4981`, its public half the repository's
`keys/norvexlabs-gpg-public.asc`). Check that the key file holds that key alone, the signature
against it alone (`gpgv`, never the keys of your own keyring), and the zip's checksum, then copy
the zip unopened into the mirror's directory for the provider (`platform` as the release's zips name it:
`linux_amd64`, `darwin_arm64`, `windows_amd64`, …; on macOS, `shasum -a 256 --check` for
`sha256sum --check`). Each step runs only once the one before succeeded, and a step that fails
says why: a download, the key file holding one key that is not the release key, the signature,
the checksum. The block is for a POSIX shell: it pastes into bash or zsh (it has no comment, so
zsh without `interactive_comments` too); on Windows, run it in Git Bash or WSL, or take its steps
by hand:

```bash
v=0.1.0 platform=linux_amd64
base=https://github.com/norvexlabs/terraform-provider-stowage/releases/download/v$v
curl -fLO "$base/terraform-provider-stowage_${v}_${platform}.zip" &&
curl -fLO "$base/terraform-provider-stowage_${v}_SHA256SUMS" &&
curl -fLO "$base/terraform-provider-stowage_${v}_SHA256SUMS.sig" &&
curl -fLO "https://raw.githubusercontent.com/norvexlabs/terraform-provider-stowage/v$v/keys/norvexlabs-gpg-public.asc" &&
{ [ "$(gpg --show-keys --with-colons norvexlabs-gpg-public.asc | awk -F: '$1=="pub"{n++} $1=="fpr"&&!f{f=$10} END{print n, f}')" = \
    "1 686D3351040F9F4463D9D3AC0C705E9BAA4F4981" ] || { echo "norvexlabs-gpg-public.asc is not the release key alone" >&2; false; }; } &&
gpg --dearmor <norvexlabs-gpg-public.asc >release-key.gpg &&
gpgv --keyring ./release-key.gpg "terraform-provider-stowage_${v}_SHA256SUMS.sig" "terraform-provider-stowage_${v}_SHA256SUMS" &&
grep "_${platform}.zip\$" "terraform-provider-stowage_${v}_SHA256SUMS" | sha256sum --check &&
mkdir -p ~/.terraform.d/plugins/registry.opentofu.org/norvexlabs/stowage &&
cp "terraform-provider-stowage_${v}_${platform}.zip" ~/.terraform.d/plugins/registry.opentofu.org/norvexlabs/stowage/ &&
echo "installed $v ($platform)" || echo "NOT INSTALLED: the step that failed said why, above" >&2
```

On Windows the directory is `%APPDATA%\terraform.d\plugins\registry.opentofu.org\norvexlabs\stowage`.
A root requiring `norvexlabs/stowage` at that version (`version = "~> 0.1"` takes any 0.x) then
installs it with `tofu init`, which records the zip's hash in `.terraform.lock.hcl`.

## The implied local mirror (`make install-local`)

Build, then copy into `~/.terraform.d/plugins`:

```bash
make install-local
```

This copies `bin/terraform-provider-stowage` to
`~/.terraform.d/plugins/registry.opentofu.org/norvexlabs/stowage/<version>/linux_amd64/terraform-provider-stowage_v<version>`,
`<version>` being the top entry of `CHANGELOG.md` (on `main`, the next release's). `PLUGIN_DIR=<dir>` picks another of
OpenTofu's implied mirror directories (`~/.local/share/opentofu/plugins`, say). The build is
linux/amd64; a release's zips hold every platform's.

A root then names the provider as it will once published, at a version that takes it:

```hcl
terraform {
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "~> 0.1"
    }
  }
}
```

`tofu init` installs it from the mirror and records its hash in `.terraform.lock.hcl`. After a
rebuild the hash changes: run `tofu init -upgrade` in each root. On another machine, copy the
same directory tree, or serve it as a `network_mirror`.

The roots in `examples/quickstart/` (`docker`, `kubernetes`, `vm`) are written for this: `make
install-local`, then `tofu init` and `tofu validate` in one.

## `dev_overrides`, for development

In `~/.tofurc` (or a file `TF_CLI_CONFIG_FILE` names):

```hcl
provider_installation {
  dev_overrides { "registry.opentofu.org/norvexlabs/stowage" = "/path/to/terraform-provider-stowage/bin" }
  direct {}
}
```

OpenTofu then runs `bin/terraform-provider-stowage` as built, with no `tofu init` for it and no
lock entry, and warns on every run. The acceptance tests run so.
