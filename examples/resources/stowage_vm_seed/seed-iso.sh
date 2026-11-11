#!/bin/sh
# seed-iso.sh — an `external` data source's program: a NoCloud seed ISO of a stowage_vm_seed's
# user_data and meta_data, for a hypervisor that boots it from a CD-ROM. Query (JSON on stdin):
# user_data, meta_data, hostname, dir. Output: path and sha256. The ISO and its directory are
# 0600/0700: the user-data holds the one-time host key's private key. Needs xorriso and jq.
set -eu
q=$(cat)
host=$(printf '%s' "$q" | jq -r .hostname)
dir=$(printf '%s' "$q" | jq -r .dir)
umask 077
mkdir -p "$dir"
work=$(mktemp -d "$dir/.seed.XXXXXX")
trap 'rm -rf "$work"' EXIT
printf '%s' "$q" | jq -j .user_data >"$work/user-data"
printf '%s' "$q" | jq -j .meta_data >"$work/meta-data"
xorriso -as mkisofs -quiet -o "$work/seed.iso" -V cidata -J -R "$work/user-data" "$work/meta-data" >&2
sha=$(sha256sum "$work/seed.iso" | cut -d' ' -f1)
mv -f "$work/seed.iso" "$dir/$host-seed-$sha.iso"
jq -n --arg path "$dir/$host-seed-$sha.iso" --arg sha "$sha" '{path: $path, sha256: $sha}'
