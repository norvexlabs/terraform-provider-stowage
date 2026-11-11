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
TOFU=$(tofu_path)
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
python3 -I - "$WORK/schema.json" <<'PY'
import json, sys
doc = json.load(open(sys.argv[1]))
schemas = doc.get("provider_schemas", {})
addr = "registry.opentofu.org/norvexlabs/stowage"
if addr not in schemas:
    sys.exit(f"FAIL: tofu's schema has no {addr} (got {sorted(schemas)})")
doc["provider_schemas"] = {"stowage": schemas[addr]}
json.dump(doc, sys.stdout)
PY
