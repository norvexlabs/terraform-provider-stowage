#!/usr/bin/env bash
# acc-libvirt.sh — examples/guides/libvirt-vm applied against libvirtd and QEMU in a container
# (test/libvirtd/, domain_type qemu: no KVM there), through tofu (make acc-libvirt). Prints PASS.
#
# Needs a Stowage checkout at STOWAGE_REPO (default ~/stowage) whose `make build` made
# bin/stowage: its suite library runs zot and ships examples/minimal with its kairos shape and its
# qcow2 disk; this repository's `make build`; tofu ≥ 1.11, docker, jq and xorriso on PATH
# (make acc-libvirt runs it in the acceptance runner).
#
# What it runs: the seed (the installer read from zot for this machine, its disk written here),
# the seed ISO, the image, disk and seed volumes and the domain (-target: QEMU without KVM boots the
# guest too slowly for the install to answer) — then the domain checked (running, ACPI and APIC,
# disks by their paths, UEFI, a one-vCPU NIC with no queue count and a 1024 receive ring), a plan
# with no change, and the destroy, which leaves no domain, volume or NVRAM file. The libvirt
# proofs still open (a KVM host with AppArmor enforcing, several vCPUs, the install over SSH) are
# TODO.md's.
set -euo pipefail

STOWAGE_REPO=${STOWAGE_REPO:-$HOME/stowage}
PROV=$(cd "$(dirname "$0")/.." && pwd)
[[ -x $STOWAGE_REPO/bin/stowage && -f $STOWAGE_REPO/test/lib.sh ]] || { echo "FAIL: no Stowage checkout with bin/stowage at STOWAGE_REPO=$STOWAGE_REPO (make build there)" >&2; exit 1; }
[[ -x $PROV/bin/terraform-provider-stowage ]] || { echo "FAIL: no bin/terraform-provider-stowage: make build" >&2; exit 1; }
for t in tofu docker jq xorriso; do
	command -v "$t" >/dev/null || { echo "FAIL: $t is not on PATH" >&2; exit 1; }
done
# shellcheck source=/dev/null
. "$STOWAGE_REPO/test/lib.sh"
# shellcheck source=/dev/null
. "$STOWAGE_REPO/test/pins.sh"
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
LIBVIRTD_BASE=ubuntu:24.04@sha256:fe81560990fae86dc4743202b2cb4d3df98d0bcf949173cecef15ce1127ef852

step "zot, examples/minimal with its kairos shape, shipped on beta with its qcow2 disk"
start_zot
KAIROS=1 new_project "$PG"
"${S[@]}" ship --channel beta --disks 2>"$WORK/ship.log" || { tail -50 "$WORK/ship.log"; fail "ship 1.0.0"; }
INSTALLER=$REG/notes/notes-installer@$(digest_of notes/notes-installer:beta-1.0.0)
[[ $INSTALLER == *@sha256:* ]] || fail "no digest of the installer"

step "libvirtd and QEMU in a container, its socket bind-mounted out"
LV=$WORK/libvirt
mkdir -p "$LV/run"
docker build -q --build-arg "BASE=$LIBVIRTD_BASE" -t tps-acc-libvirtd "$PROV/test/libvirtd" >/dev/null || fail "the libvirtd image"
# --init: libvirtd daemonizes QEMU, whose parent is then PID 1, which must reap it (a zombie QEMU
# fails the destroy).
lvc=$(docker run -d --init --name "tps-acc-libvirtd-$RUN" --privileged --cgroupns=host --memory 6g -v "$LV/run:/run/libvirt" tps-acc-libvirtd)
CLEANUP+=("docker rm -f $lvc >/dev/null 2>&1")
# libvirtd's runtime files under the bind mount are root's: removed by a container, as root.
CLEANUP+=("docker run --rm --network none -v '$LV:/lv' --entrypoint rm $LIBVIRTD_BASE -rf /lv/run >/dev/null 2>&1")
for _ in $(seq 60); do [[ -e $LV/run/acc-ready ]] && break; sleep 1; done
[[ -e $LV/run/acc-ready ]] || { docker logs "$lvc" | tail -20; fail "libvirtd did not start"; }
# libvirtd names the peer of its socket: this user's uid must be a user inside.
docker exec "$lvc" sh -c "getent passwd $(id -u) >/dev/null || useradd -u $(id -u) -M acc"
virsh() { docker exec "$lvc" virsh -c qemu:///system "$@"; }

step "the root: examples/guides/libvirt-vm, this provider from a filesystem mirror"
ROOT=$WORK/root
mkdir -p "$ROOT" "$WORK/disks" "$WORK/seeds"
cp -a "$PROV/examples/guides/libvirt-vm/." "$ROOT/"
rm -rf "$ROOT/tests"
MIRRORDIR=$WORK/providers/registry.opentofu.org/norvexlabs/stowage/0.3.0/linux_amd64
mkdir -p "$MIRRORDIR"
cp "$PROV/bin/terraform-provider-stowage" "$MIRRORDIR/terraform-provider-stowage_v0.3.0"
cat >"$WORK/tofurc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "$WORK/providers"
    include = ["registry.opentofu.org/norvexlabs/stowage"]
  }
  direct { exclude = ["registry.opentofu.org/norvexlabs/stowage"] }
}
EOF
export TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1 TF_VAR_pull_password=$PULL_TOKEN
ssh-keygen -q -t ed25519 -N '' -f "$WORK/key"
jq -n --arg installer "$INSTALLER" --arg key "$WORK/key" --arg uri "qemu:///system?socket=$LV/run/libvirt-sock" --arg disks "$WORK/disks" --arg seeds "$WORK/seeds" \
	'{installer: $installer, name: "notes-lv", hostname: "notes-lv", ssh_key_file: $key, uri: $uri, domain_type: "qemu",
	addressing: "static", address: "192.168.122.10/24", gateway: "192.168.122.1", dns: ["192.168.122.1"],
	cpus: 1, memory_mib: 2048, tls: "internal", domains: {app: "app.notes.test", api: "api.notes.test"},
	inputs: {ADMIN_EMAIL: "admin@notes.test"}, pull_username: "pull", disk_dir: $disks, seed_dir: $seeds}' >"$ROOT/terraform.tfvars.json"
tf() { (cd "$ROOT" && tofu "$@"); }
CLEANUP=("[[ ! -e $ROOT/terraform.tfstate ]] || (cd $ROOT && tofu destroy -auto-approve -target=libvirt_domain.vm >>$WORK/destroy.log 2>&1)" "${CLEANUP[@]}")
tf init -input=false >"$WORK/init.log" 2>&1 || { cat "$WORK/init.log"; fail "tofu init"; }

step "apply: the seed, its ISO, the volumes and the domain"
tf apply -input=false -auto-approve -target=libvirt_domain.vm >"$WORK/apply.log" 2>&1 || { tail -60 "$WORK/apply.log"; fail "tofu apply"; }
[[ $(virsh domstate notes-lv) == running ]] || fail "the domain does not run"
xml=$(virsh dumpxml notes-lv)
grep -q '<acpi/>' <<<"$xml" && grep -q '<apic/>' <<<"$xml" || fail "the domain has no ACPI or APIC"
grep -q "<disk type='file' device='disk'>" <<<"$xml" && ! grep -q "<disk type='volume'" <<<"$xml" || fail "a disk is not given by its path"
grep -q "<loader .*type='pflash'" <<<"$xml" || fail "the domain does not boot UEFI"
grep -q "<driver rx_queue_size='1024'/>" <<<"$xml" || { grep -n '<driver' <<<"$xml"; fail "the NIC's driver is not a 1024 receive ring alone"; }
rc=0
tf plan -input=false -detailed-exitcode -target=libvirt_domain.vm >"$WORK/plan.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { tail -40 "$WORK/plan.log"; fail "the root drifts after its apply (exit $rc)"; }

step "destroy: no domain, volume or NVRAM file left"
tf destroy -input=false -auto-approve -target=libvirt_domain.vm -target=libvirt_volume.image -target=libvirt_volume.disk -target=libvirt_volume.seed >"$WORK/destroy.log" 2>&1 || { tail -40 "$WORK/destroy.log"; fail "tofu destroy"; }
[[ -z $(virsh list --all --name | tr -d '[:space:]') ]] || fail "the destroy left a domain"
[[ -z $(virsh vol-list default | tail -n +3 | tr -d '[:space:]') ]] || { virsh vol-list default; fail "the destroy left a volume"; }
[[ -z $(docker exec "$lvc" sh -c 'ls -A /var/lib/libvirt/qemu/nvram 2>/dev/null') ]] || fail "the destroy left an NVRAM file"
echo PASS
