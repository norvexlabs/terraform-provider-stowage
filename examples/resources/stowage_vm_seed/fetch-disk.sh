#!/bin/sh
# fetch-disk.sh — an `external` data source's program: the disk a stowage_vm_seed names, as a file.
# Query (JSON on stdin): image (the seed's disk[<platform>].image, by digest), dir (where the disk
# is kept: a second VM of that disk reuses it), ca_file and registry_config (optional: the mirror's
# CA and a Docker config logged in to it). Output: path, sha256 (the file's own, as the disk
# artifact's layer annotates it) and os_gib (what Kairos takes of the VM's disk, from its
# annotation). Needs oras, zstd and jq. The artifact is Stowage's documented form: one layer, the
# disk compressed with zstd, annotated dev.stowage.uncompressed.digest.
set -eu
q=$(cat)
image=$(printf '%s' "$q" | jq -r .image)
dir=$(printf '%s' "$q" | jq -r .dir)
ca=$(printf '%s' "$q" | jq -r '.ca_file // ""')
cfg=$(printf '%s' "$q" | jq -r '.registry_config // ""')
set --
[ -z "$ca" ] || set -- "$@" --ca-file "$ca"
[ -z "$cfg" ] || set -- "$@" --registry-config "$cfg"
manifest=$(oras manifest fetch "$@" "$image")
sha=$(printf '%s' "$manifest" | jq -r '.layers[0].annotations["dev.stowage.uncompressed.digest"]' | sed 's/^sha256://')
os_mib=$(printf '%s' "$manifest" | jq -r '.annotations["dev.stowage.disk.os-mib"] // "0"')
title=$(printf '%s' "$manifest" | jq -r '.layers[0].annotations["org.opencontainers.image.title"]')
if [ -z "$sha" ] || [ "$sha" = null ]; then
  echo "$image: no dev.stowage.uncompressed.digest on its layer" >&2
  exit 1
fi
out=$dir/$sha/${title%.zst}
if ! [ -f "$out" ] || [ "$(sha256sum "$out" | cut -d' ' -f1)" != "$sha" ]; then
  mkdir -p "$dir/$sha"
  oras pull "$@" -o "$dir/$sha" "$image" >&2
  zstd -d -f --rm "$dir/$sha/$title" -o "$out" >&2
  [ "$(sha256sum "$out" | cut -d' ' -f1)" = "$sha" ] || { echo "$out is not of the digest its artifact records" >&2; exit 1; }
fi
jq -n --arg path "$out" --arg sha "$sha" --arg os "$(( (os_mib + 256 + 1023) / 1024 ))" '{path: $path, sha256: $sha, os_gib: $os}'
