---
page_title: "Installing without the registry - stowage"
subcategory: "Guides"
description: |-
  The provider is not in a registry yet: install it from a checkout into OpenTofu's local mirror, or point OpenTofu at the build.
---

# Installing without the registry

The provider is not published to a registry yet. Build it from a checkout of
`terraform-provider-stowage` (Docker is the only requirement) and give it to OpenTofu in one of
two ways.

## The implied local mirror (`make install-local`)

```bash
make install-local   # build, then copy into ~/.terraform.d/plugins
```

This copies `bin/terraform-provider-stowage` to
`~/.terraform.d/plugins/registry.opentofu.org/norvexlabs/stowage/<version>/linux_amd64/terraform-provider-stowage_v<version>`,
`<version>` being the top entry of `CHANGELOG.md` (`0.1.0`). `PLUGIN_DIR=<dir>` picks another of
OpenTofu's implied mirror directories (`~/.local/share/opentofu/plugins`, say). The build is
linux/amd64.

A root then names the provider as it will once published, with that version:

```hcl
terraform {
  required_providers {
    stowage = {
      source  = "norvexlabs/stowage"
      version = "0.1.0"
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
