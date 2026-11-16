#!/usr/bin/env bash
# examples.sh — `tofu validate` and `tofu test` (each test's runs at command = plan, under mocked
# providers) of every root in examples/guides/ and examples/quickstart/ (make examples-test). Each
# root is copied into a work directory under ~/.cache (the example's own files and lock file stay
# as they are), this repository's bin/terraform-provider-stowage installed from a filesystem
# mirror of the run's own as norvexlabs/stowage 0.3.0, the other providers from the registry at
# the versions each root pins (network access). EXAMPLES=<dir …> narrows the roots; PROVIDER_BIN
# runs another provider binary. Prints PASS.
set -euo pipefail

PROV=$(cd "$(dirname "$0")/.." && pwd)
# shellcheck source=test/tofu.sh
. "$PROV/test/tofu.sh"
TOFU=$(tofu_path)
BIN=${PROVIDER_BIN:-$PROV/bin/terraform-provider-stowage}
[[ -x $BIN ]] || { echo "FAIL: no $BIN: make build" >&2; exit 1; }
WORK=$(mktemp -d "${XDG_CACHE_HOME:-$HOME/.cache}/tps-examples.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/mirror/registry.opentofu.org/norvexlabs/stowage/0.3.0/linux_amd64"
cp "$BIN" "$WORK/mirror/registry.opentofu.org/norvexlabs/stowage/0.3.0/linux_amd64/terraform-provider-stowage_v0.3.0"
cat >"$WORK/tofurc" <<EOT
provider_installation {
  filesystem_mirror {
    path    = "$WORK/mirror"
    include = ["registry.opentofu.org/norvexlabs/stowage"]
  }
  direct { exclude = ["registry.opentofu.org/norvexlabs/stowage"] }
}
plugin_cache_dir = "${XDG_CACHE_HOME:-$HOME/.cache}/tps-plugin-cache"
EOT
mkdir -p "${XDG_CACHE_HOME:-$HOME/.cache}/tps-plugin-cache"
export TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1

roots=${EXAMPLES:-$(cd "$PROV" && ls -d examples/guides/*/ examples/guides/*/image/ examples/quickstart/*/ 2>/dev/null)}
failed=
for r in $roots; do
	r=${r%/}
	dst=$WORK/root/$r
	mkdir -p "$dst"
	cp -a "$PROV/$r/." "$dst/"
	printf '== %s\n' "$r"
	if ! "$TOFU" -chdir="$dst" init -input=false -no-color >"$dst.init.log" 2>&1; then
		cat "$dst.init.log"
		failed+=" $r(init)"
		continue
	fi
	"$TOFU" -chdir="$dst" validate -no-color || { failed+=" $r(validate)"; continue; }
	if [[ -d $dst/tests ]]; then
		(cd "$dst" && "$TOFU" test -no-color) || failed+=" $r(test)"
	fi
done
[[ -z $failed ]] || { echo "FAIL:$failed" >&2; exit 1; }
echo PASS
