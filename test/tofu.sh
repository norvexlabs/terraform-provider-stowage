# shellcheck shell=bash
# tofu.sh — sourced: tofu_path prints the path of the pinned OpenTofu, downloaded once into the
# cache (its archive's checksum checked) — the one the acceptance tests and make docs run.
TOFU_VERSION=1.13.9
TOFU_SHA=b34b327cc1ef07178207b6f2cd1f5276537d3e5b7be316500f6dac50d4c0a857

tofu_path() {
	local cache=${XDG_CACHE_HOME:-$HOME/.cache} tools tmp
	tools=$cache/terraform-provider-stowage-tools/$TOFU_SHA
	if [[ ! -x $tools/tofu ]]; then
		mkdir -p "$tools"
		tmp=$(mktemp -d "$tools/.dl.XXXXXX")
		curl -fsSL "https://github.com/opentofu/opentofu/releases/download/v$TOFU_VERSION/tofu_${TOFU_VERSION}_linux_amd64.tar.gz" -o "$tmp/tofu.tgz" ||
			{ echo "FAIL: download OpenTofu" >&2; rm -rf "$tmp"; return 1; }
		echo "$TOFU_SHA  $tmp/tofu.tgz" | sha256sum -c --quiet - ||
			{ echo "FAIL: OpenTofu's archive is not $TOFU_SHA" >&2; rm -rf "$tmp"; return 1; }
		if ! { tar -xzf "$tmp/tofu.tgz" -C "$tmp" tofu && mv -f "$tmp/tofu" "$tools/tofu"; }; then
			echo "FAIL: unpacking OpenTofu into $tools" >&2
			rm -rf "$tmp"
			return 1
		fi
		rm -rf "$tmp"
	fi
	printf '%s\n' "$tools/tofu"
}
