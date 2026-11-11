#!/usr/bin/env bash
# schema.sh — `tofu providers schema -json` of the provider built in bin/, through dev_overrides,
# on standard output: the schema make docs gives tfplugindocs, so it never downloads Terraform.
# The provider's schema is keyed by its short name, `stowage`: tfplugindocs looks a schema up by
# that name or by a registry.terraform.io/hashicorp address, never OpenTofu's.
set -euo pipefail
REPO=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=test/tofu.sh
. "$REPO/test/tofu.sh"
BIN=${PROVIDER_BIN:-$REPO/bin/terraform-provider-stowage}
[[ -x $BIN ]] || { echo "FAIL: $BIN is missing: run make build" >&2; exit 1; }
TOFU=${TOFU:-$(tofu_path)} # TOFU: a tofu of the pinned version given (the Docker stage of make docs-check)
WORK=$(mktemp -d "${XDG_CACHE_HOME:-$HOME/.cache}/tps-schema.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/plugins" "$WORK/root"
cp "$BIN" "$WORK/plugins/terraform-provider-stowage"
cat >"$WORK/tofurc" <<HCL
provider_installation {
  dev_overrides { "registry.opentofu.org/norvexlabs/stowage" = "$WORK/plugins" }
  direct {}
}
HCL
cat >"$WORK/root/main.tf" <<'HCL'
terraform {
  required_providers {
    stowage = { source = "norvexlabs/stowage" }
  }
}
HCL
cd "$WORK/root"
TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1 "$TOFU" providers schema -json >"$WORK/schema.json"
# tofu prints the schema as one line: its provider_schemas key, renamed.
key='"provider_schemas":{"registry.opentofu.org/norvexlabs/stowage":'
grep -qF "$key" "$WORK/schema.json" || { echo "FAIL: tofu's schema has no registry.opentofu.org/norvexlabs/stowage" >&2; exit 1; }
sed 's|"provider_schemas":{"registry.opentofu.org/norvexlabs/stowage":|"provider_schemas":{"stowage":|' "$WORK/schema.json"
