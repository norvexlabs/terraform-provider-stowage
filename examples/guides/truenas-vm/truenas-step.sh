#!/usr/bin/env bash
# truenas-step.sh STEP — what the TrueNAS provider has no resource for, run by the root's
# local-exec provisioners over SSH to TrueNAS's shell, whose `midclt` calls the middleware (the
# same API the provider uses). Every setting comes from the environment:
#
#   TNAS_SSH_HOST, TNAS_SSH_USER, TNAS_SSH_PORT, TNAS_SSH_KEY   how TrueNAS is reached (its host key
#                                                                must already be known: no other is
#                                                                accepted)
#   import-image     IMAGE, IMAGE_SHA256, STAGING_DIR, ZVOL: upload the disk image (checked against
#                    its SHA-256 here and there), convert it into the zvol (vm.device.convert),
#                    remove the upload. Refuses a zvol that already holds data.
#   seed             SEED_USER_DATA, SEED_META_DATA, WORK_DIR, DEST: a NoCloud seed ISO (volume
#                    cidata) built in a private directory, uploaded to DEST, removed here.
#   start | stop     VM_ID: start the VM unless it runs; power it off when it runs.
#   drop-snapshots   DATASETS: delete every snapshot of each dataset (space-separated).
set -euo pipefail

die() { printf 'truenas-step: %s\n' "$*" >&2; exit 1; }
need() { [[ -n ${!1:-} ]] || die "$1 is not set"; }
for v in TNAS_SSH_HOST TNAS_SSH_USER TNAS_SSH_PORT TNAS_SSH_KEY; do need "$v"; done

# nas_in CMD — CMD in TrueNAS's shell, given this script's standard input; nas CMD — given none
# (ssh would otherwise read what a loop around it reads).
nas_in() {
	ssh -i "$TNAS_SSH_KEY" -p "$TNAS_SSH_PORT" -o IdentitiesOnly=yes -o BatchMode=yes \
		-o StrictHostKeyChecking=yes -o ConnectTimeout=15 "$TNAS_SSH_USER@$TNAS_SSH_HOST" "$@"
}
nas() { nas_in "$@" </dev/null; }
# q ARG… — each argument quoted for the remote shell.
q() { printf ' %q' "$@"; }
# midclt METHOD [ARGS…] — a middleware call on TrueNAS; its JSON on standard output.
midclt() { nas "midclt call$(q "$@")"; }
midjob() { nas "midclt call -job$(q "$@")"; }

sha_of() { sha256sum "$1" | cut -d' ' -f1; }
remote_sha() { nas "if [ -f $(printf %q "$1") ]; then sha256sum $(printf %q "$1") | cut -d' ' -f1; fi"; }

# upload SRC DEST SHA256 — SRC to DEST through a .part file, checked on TrueNAS before it takes
# DEST's name; a DEST already holding those bytes is kept. Mode 0600 (the seed holds the one-time
# host key, the disk the release's pull account): libvirt hands a VM's file to its process as it
# starts it.
upload() {
	local src=$1 dest=$2 sha=$3
	[[ $(remote_sha "$dest") == "$sha" ]] && return 0
	nas_in "umask 077; cat > $(printf %q "$dest.part")" <"$src"
	[[ $(remote_sha "$dest.part") == "$sha" ]] || { nas "rm -f $(printf %q "$dest.part")"; die "the upload of $src to $dest does not match its SHA-256"; }
	nas "chmod 0600 $(printf %q "$dest.part") && mv -f $(printf %q "$dest.part") $(printf %q "$dest")"
}

case "${1:-}" in
import-image)
	for v in IMAGE IMAGE_SHA256 STAGING_DIR ZVOL; do need "$v"; done
	[[ $(sha_of "$IMAGE") == "$IMAGE_SHA256" ]] || die "$IMAGE does not match IMAGE_SHA256"
	# A zvol a VM already wrote to is never overwritten: only a new zvol (a few KiB referenced) is.
	used=$(nas "zfs get -Hp -o value referenced $(printf %q "$ZVOL")")
	[[ $used =~ ^[0-9]+$ ]] || die "could not read how much $ZVOL holds"
	((used < 64 * 1024 * 1024)) || die "$ZVOL already holds data ($used bytes): refusing to write an image over it"
	ext=${IMAGE##*.}
	staged="$STAGING_DIR/disk-${IMAGE_SHA256:0:16}.$ext"
	upload "$IMAGE" "$staged" "$IMAGE_SHA256"
	midjob vm.device.convert "$(jq -cn --arg s "$staged" --arg d "$ZVOL" '{source: $s, destination: $d}')" >/dev/null
	nas "rm -f $(printf %q "$staged")"
	;;
seed)
	for v in SEED_USER_DATA SEED_META_DATA WORK_DIR DEST; do need "$v"; done
	mkdir -p "$WORK_DIR"
	umask 077
	work=$(mktemp -d "$WORK_DIR/.seed.XXXXXX")
	trap 'rm -rf "$work"' EXIT
	mkdir "$work/src"
	printf '%s' "$SEED_USER_DATA" >"$work/src/user-data"
	printf '%s' "$SEED_META_DATA" >"$work/src/meta-data"
	if command -v xorriso >/dev/null; then
		xorriso -no_rc -report_about SORRY -outdev "$work/seed.iso" -volid cidata -joliet on -rockridge on -map "$work/src" / >&2
	elif command -v genisoimage >/dev/null; then
		genisoimage -quiet -output "$work/seed.iso" -volid cidata -joliet -r "$work/src/user-data" "$work/src/meta-data"
	else
		die "neither xorriso nor genisoimage is installed"
	fi
	upload "$work/seed.iso" "$DEST" "$(sha_of "$work/seed.iso")"
	;;
start)
	need VM_ID
	state=$(midclt vm.status "$VM_ID" | jq -r .state)
	[[ $state == RUNNING ]] || midclt vm.start "$VM_ID" >/dev/null
	;;
stop)
	need VM_ID
	state=$(midclt vm.status "$VM_ID" | jq -r .state)
	[[ $state != RUNNING ]] || midjob vm.stop "$VM_ID" '{"force_after_timeout": true}' >/dev/null
	;;
drop-snapshots)
	need DATASETS
	for ds in $DATASETS; do
		midclt pool.snapshot.query "$(jq -cn --arg d "$ds" '[["dataset", "=", $d]]')" '{"select": ["id"]}' |
			jq -r '.[].id' | while read -r snap; do
				midclt pool.snapshot.delete "$snap" >/dev/null
			done
	done
	;;
*)
	sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2
	exit 2
	;;
esac
