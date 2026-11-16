#!/bin/sh
# seed-iso.sh — an `external` data source's program: a NoCloud seed ISO of a stowage_vm_seed's
# user_data and meta_data, for a hypervisor that boots it from a CD-ROM. Query (JSON on stdin):
# user_data, meta_data, hostname, dir. Output: path and sha256. The ISO and its directory are
# 0600/0700: the user-data holds the one-time host key's private key. Needs xorriso and jq.
# The ISO is the same bytes for the same user-data and meta-data — every date fixed (the files',
# the volume's, its UUID), owners and modes fixed — so a plan reading it again changes nothing.
set -eu
q=$(cat)
host=$(printf '%s' "$q" | jq -r .hostname)
dir=$(printf '%s' "$q" | jq -r .dir)
umask 077
mkdir -p "$dir"
work=$(mktemp -d "$dir/.seed.XXXXXX")
trap 'rm -rf "$work"' EXIT
mkdir "$work/src"
printf '%s' "$q" | jq -j .user_data >"$work/src/user-data"
printf '%s' "$q" | jq -j .meta_data >"$work/src/meta-data"
# Every date fixed, the ctimes the owner and mode changes stamp included (alter_date_r c, after
# them), the volume's dates and UUID too: the same inputs make the same bytes.
SOURCE_DATE_EPOCH=0 xorriso -no_rc -report_about SORRY -outdev "$work/seed.iso" \
	-volid cidata -joliet on -rockridge on -map "$work/src" / \
	-chown_r 0 / -- -chgrp_r 0 / -- -chmod_r a=r / -- -chmod 0555 / -- \
	-alter_date_r b =0 / -- -alter_date_r c =0 / -- \
	-volume_date all_file_dates =0 -volume_date uuid 1970010100000000 -volume_date c =0 -volume_date m =0 >&2
sha=$(sha256sum "$work/seed.iso" | cut -d' ' -f1)
mv -f "$work/seed.iso" "$dir/$host-seed-$sha.iso"
jq -n --arg path "$dir/$host-seed-$sha.iso" --arg sha "$sha" '{path: $path, sha256: $sha}'
