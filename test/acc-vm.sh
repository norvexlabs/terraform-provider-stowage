#!/usr/bin/env bash
# acc-vm.sh — the VM target and stowage_vm_seed against a real Kairos VM on TrueNAS, through
# `tofu` (make acc-vm). Prints PASS.
#
# Needs:
#   - STOWAGE_VM_ENV=<file>, the file Stowage's VM suites read (make acc-vm resolves it on the host
#     and passes what it sets by name, STOWAGE_VM_ENV_RESOLVED=1; see the head of Stowage's test/vm.sh:
#     TrueNAS's API, the pool, dataset and bridge, the domain the DHCP server registers host names
#     under, an SSH key): given at run time, never written in the tree;
#   - a Stowage checkout at STOWAGE_REPO (default ~/stowage) whose `make build` made bin/stowage, at
#     a release this provider reads (0.35.0 or later): its suite library runs zot, ships
#     examples/minimal and its mirror;
#   - this repository's `make build` (bin/terraform-provider-stowage), installed by tofu init from
#     a filesystem mirror of this run's own;
#   - tofu ≥ 1.11, docker, ssh, jq, oras, zstd and xorriso on PATH, on a linux/amd64 machine TrueNAS
#     reaches: `make acc-vm` runs it in the acceptance runner (test/acc.Dockerfile), which has them.
#
# What it runs, in one OpenTofu root (examples/resources/stowage_vm_seed's, its VM a host named by
# DHCP under the env file's domain):
#   1. examples/minimal with its kairos shape (and a secret input) shipped on beta with its qcow2
#      disk, carried to a TLS mirror of its own CA on the address TrueNAS reaches this host at;
#   2. apply: stowage_vm_seed (truenas, the mirror), the disk pulled and an ISO of its user-data,
#      Stowage's TrueNAS module creating and starting the VM, stowage_install's vm target waiting
#      for its first boot under the one-time key, installing with the secret input over SSH (in
#      neither the seed nor state), reading the VM's own host key and retiring the one-time key
#      (ssh pinned to it alone refused from then on);
#   3. `tofu plan -detailed-exitcode` shows no change; a refresh reads the VM's release;
#   4. an input changed: the re-run alone;
#   5. 1.0.1 shipped and mirrored, vm.version 1.0.1: the upgrade, its reboot followed, the VM on
#      1.0.1 with seed_boot_id null, and only its own key accepted from then on;
#   6. vm.version back to 1.0.0 without allow_rollback: a plan error; with it, the rollback;
#   7. destroy with deletion_protection off: stowage_install forgotten, the module destroyed.
set -euo pipefail

STOWAGE_REPO=${STOWAGE_REPO:-$HOME/stowage}
PROV=$(cd "$(dirname "$0")/.." && pwd)
[[ -x $STOWAGE_REPO/bin/stowage && -f $STOWAGE_REPO/test/vm.sh ]] || { echo "FAIL: no Stowage checkout with bin/stowage at STOWAGE_REPO=$STOWAGE_REPO (make build there)" >&2; exit 1; }
[[ -x $PROV/bin/terraform-provider-stowage ]] || { echo "FAIL: no bin/terraform-provider-stowage: make build" >&2; exit 1; }
for t in tofu docker jq oras zstd xorriso; do
	command -v "$t" >/dev/null || { echo "FAIL: $t is not on PATH" >&2; exit 1; }
done

# shellcheck source=/dev/null
. "$STOWAGE_REPO/test/vm.sh"
if [[ -n ${STOWAGE_VM_ENV_RESOLVED:-} ]]; then
	# make acc-vm resolved STOWAGE_VM_ENV on the host and passed what it sets by name: vm_env
	# checks the environment as it is, a file of nothing to read (/dev/null).
	STOWAGE_VM_ENV=/dev/null
fi
vm_env
# shellcheck source=/dev/null
. "$STOWAGE_REPO/test/lib.sh"
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395

step "zot, examples/minimal with its kairos shape and a secret input, shipped on beta with its qcow2 disk"
start_zot
KAIROS=1 new_project "$PG"
python3 - "$P/stowage.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
old = '  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true }\n'
assert s.count(old) == 1, old
s = s.replace(old, old + '  - { key: ACC_TOKEN, prompt: "A token", secret: true, required: true }\n')
open(path, "w").write(s)
PY
"${S[@]}" lint
vm_names
"${S[@]}" ship --channel beta --disks 2>"$WORK/ship.log" || { tail -50 "$WORK/ship.log"; fail "ship 1.0.0"; }

step "the site's mirror, TLS of its own CA, on the address TrueNAS reaches this host at"
vm_ctl beta-1.0.0
vm_mirror
mirror "$WORK/r1.oci.tar" --disks qcow2
INSTALLER=$REG/notes/notes-installer@$(digest_of notes/notes-installer:beta-1.0.0)
[[ $INSTALLER == *@sha256:* ]] || fail "no digest of the installer"

step "the OpenTofu root: this provider from a filesystem mirror, the seed, the disk and ISO, Stowage's TrueNAS module, the install"
ROOT=$WORK/root
mkdir -p "$ROOT" "$WORK/disks"
cp "$PROV"/examples/resources/stowage_vm_seed/*.sh "$ROOT/"
# The provider built here, from a filesystem mirror of this run's own (dev_overrides would leave
# tofu init querying the registry for it, which the root's other providers need init for).
MIRRORDIR=$WORK/providers/registry.opentofu.org/norvexlabs/stowage/0.1.0/linux_amd64
mkdir -p "$MIRRORDIR"
cp "$PROV/bin/terraform-provider-stowage" "$MIRRORDIR/terraform-provider-stowage_v0.1.0"
cat >"$WORK/tofurc" <<EOF
provider_installation {
  filesystem_mirror {
    path    = "$WORK/providers"
    include = ["registry.opentofu.org/norvexlabs/stowage"]
  }
  direct { exclude = ["registry.opentofu.org/norvexlabs/stowage"] }
}
EOF
export TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1
export STOWAGE_CTL=$CTL
TOKEN=$(token)
VMNAME=${H//-/}
cat >"$ROOT/main.tf" <<EOF
terraform {
  required_version = ">= 1.11.0"
  required_providers {
    stowage = { source = "registry.opentofu.org/norvexlabs/stowage" }
  }
}

provider "stowage" {
  username    = "${PULL_USER:-pull}"
  password    = var.pull_token
  registry    = "$SITE"
  registry_ca = file("$MIRROR_CA")
}

variable "pull_token" {
  type      = string
  sensitive = true
}
variable "token" {
  type      = string
  sensitive = true
  ephemeral = true
}
variable "release_version" { type = string }
variable "admin" { type = string }
variable "allow_rollback" { default = false }
variable "deletion_protection" { default = true }

resource "stowage_vm_seed" "vm" {
  installer      = "$INSTALLER"
  platform       = "truenas"
  hostname       = "$H"
  user           = "ops"
  ssh_public_key = file("$KEY.pub")
  ssh_allow      = ["$MIRROR_IP/32"]
  tls            = "internal"
  inputs         = { ADMIN_EMAIL = "admin@notes.test" }
  disk_dir       = "$WORK/disks"
}

data "external" "seed" {
  program = ["\${path.module}/seed-iso.sh"]
  query = {
    user_data = stowage_vm_seed.vm.user_data
    meta_data = stowage_vm_seed.vm.meta_data
    hostname  = "$H"
    dir       = "$WORK/seeds"
  }
}

module "vm" {
  source = "$STOWAGE_REPO/internal/install/tofu/truenas"

  name                 = "$VMNAME"
  pool                 = "$POOL"
  parent_dataset       = "$DATASET"
  nic_attach           = "$BRIDGE"
  insecure_skip_verify = ${TRUENAS_INSECURE_SKIP_VERIFY:-false}
  cpus                 = 2
  memory_mib           = 4096
  disk_gib             = stowage_vm_seed.vm.disk_file.disk_gib
  image                = stowage_vm_seed.vm.disk_file.path
  image_sha256         = stowage_vm_seed.vm.disk_file.sha256
  seed                 = data.external.seed.result.path
  seed_sha256          = data.external.seed.result.sha256
}

resource "stowage_install" "vm" {
  inputs                   = { ADMIN_EMAIL = var.admin }
  secret_inputs_wo         = { ACC_TOKEN = var.token }
  secret_inputs_wo_version = 1
  tls                      = "internal"
  allow_rollback           = var.allow_rollback
  deletion_protection      = var.deletion_protection

  vm {
    host             = "$ADDR"
    user             = "ops"
    private_key_file = "$KEY"
    known_hosts_line = stowage_vm_seed.vm.known_hosts_line
    short_name       = stowage_vm_seed.vm.short_name
    version          = var.release_version
  }

  timeouts {
    create = "40m"
    update = "40m"
  }

  depends_on = [module.vm]
}

output "host_key_line" { value = stowage_install.vm.vm.host_key_line }
output "seed_boot_id" { value = stowage_install.vm.vm.seed_boot_id }
output "version" { value = stowage_install.vm.version }
output "fingerprint" { value = stowage_vm_seed.vm.fingerprint }
EOF
export TF_VAR_pull_token=$PULL_TOKEN TF_VAR_token=$TOKEN TF_VAR_admin=admin@notes.test TF_VAR_release_version=1.0.0
tf() { (cd "$ROOT" && tofu "$@"); }
destroyed=
# A run that fails destroys the VM still: the install forgotten (its delete runs nothing), the module destroyed.
CLEANUP=("[[ -n \$destroyed ]] || (cd $ROOT && tofu state rm stowage_install.vm >>$WORK/destroy.log 2>&1; tofu destroy -auto-approve >>$WORK/destroy.log 2>&1)" "${CLEANUP[@]}")
tf init -input=false >"$WORK/init.log" 2>&1 || { cat "$WORK/init.log"; fail "tofu init"; }

step "apply: the seed, the VM, its first install over SSH under the one-time key"
tf apply -input=false -auto-approve >"$WORK/apply1.log" 2>&1 || { tail -80 "$WORK/apply1.log"; fail "the first apply"; }
on() { ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$WORK/known_hosts" -o HostKeyAlias=acc-vm -i "$KEY" -o IdentitiesOnly=yes "ops@$ADDR" -- "$@"; }
own=$(tf output -raw host_key_line)
[[ $own == "$ADDR ssh-ed25519 "* ]] || fail "host_key_line is $own"
printf 'acc-vm %s\n' "${own#* }" >"$WORK/known_hosts"
on sudo notes-stow status --json >"$WORK/status1.json" || fail "status over SSH with the VM's own key"
jq -e '.installed.version == "1.0.0" and (.inputs | index("ACC_TOKEN") != null)' "$WORK/status1.json" >/dev/null || { cat "$WORK/status1.json"; fail "the VM does not run 1.0.0 with ACC_TOKEN recorded"; }
[[ -z $(tf output -raw seed_boot_id 2>/dev/null || true) ]] || fail "seed_boot_id is set: the one-time key was not retired"
# The one-time key is retired at once: ssh pinned to it alone is refused.
seed_line=$(jq -r '.resources[] | select(.type == "stowage_vm_seed") | .instances[0].attributes.known_hosts_line' "$ROOT/terraform.tfstate")
[[ ${seed_line#* } == "ssh-ed25519 "* ]] || fail "the seed's known_hosts_line in state is $seed_line"
printf 'acc-vm %s\n' "${seed_line#* }" >"$WORK/known_hosts.seed"
# seed_refused: ssh pinned to the one-time key alone is refused because the VM serves another key
# (ssh's mismatch warning) — not for any other failure, a timeout or an unusable line, which
# would prove nothing.
seed_refused() {
	local err
	if err=$(ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$WORK/known_hosts.seed" -o HostKeyAlias=acc-vm -i "$KEY" -o IdentitiesOnly=yes "ops@$ADDR" true 2>&1); then
		return 1
	fi
	grep -q 'REMOTE HOST IDENTIFICATION HAS CHANGED' <<<"$err" || fail "ssh pinned to the one-time key failed for another reason: $err"
}
seed_refused || fail "the VM still serves its one-time key after the install"
! grep -qF "$TOKEN" "$ROOT/terraform.tfstate" || fail "the secret input is in state"
iso=$(jq -r '.resources[] | select(.type == "external" and .name == "seed") | .instances[0].attributes.result.path' "$ROOT/terraform.tfstate")
! grep -aqF "$TOKEN" "$iso" || fail "the secret input is in the seed"

step "no change planned; a refresh reads the VM"
rc=0
tf plan -input=false -detailed-exitcode >"$WORK/plan1.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { cat "$WORK/plan1.log"; fail "a plan right after the apply is not empty (exit $rc)"; }

step "an input changed: the re-run alone, no reboot"
boot=$(on cat /proc/sys/kernel/random/boot_id)
TF_VAR_admin=other@notes.test tf apply -input=false -auto-approve >"$WORK/apply2.log" 2>&1 || { tail -60 "$WORK/apply2.log"; fail "the re-run"; }
[[ $(on cat /proc/sys/kernel/random/boot_id) == "$boot" ]] || fail "the re-run rebooted the VM"
on sudo grep -q '^ADMIN_EMAIL=.*other@notes.test' /usr/local/notes/.env || fail "the re-run did not record the input"
export TF_VAR_admin=other@notes.test

step "1.0.1 shipped and mirrored; vm.version 1.0.1: the upgrade, its reboot followed"
printf '1.0.1\n' >"$P/VERSION"
printf '### Changed\n- Release 1.0.1.\n' >"$P/changes/1.0.1.md"
"${S[@]}" ship --channel beta 2>"$WORK/ship-1.0.1.log" || { tail -50 "$WORK/ship-1.0.1.log"; fail "ship 1.0.1"; }
mirror "$WORK/r2.oci.tar" --disks none
TF_VAR_release_version=1.0.1 tf apply -input=false -auto-approve >"$WORK/apply3.log" 2>&1 || { tail -80 "$WORK/apply3.log"; fail "the upgrade"; }
[[ $(tf output -raw version) == 1.0.1 ]] || fail "state's version after the upgrade: $(tf output -raw version)"
[[ -z $(tf output -raw seed_boot_id 2>/dev/null || true) ]] || fail "seed_boot_id is set after the upgrade's reboot"
[[ $(on cat /proc/sys/kernel/random/boot_id) != "$boot" ]] || fail "the upgrade did not reboot the VM"
seed_refused || fail "the VM serves its one-time key again after the upgrade's reboot"
export TF_VAR_release_version=1.0.1

step "vm.version back to 1.0.0: refused without allow_rollback, the rollback with it"
rc=0
TF_VAR_release_version=1.0.0 tf plan -input=false >"$WORK/plan2.log" 2>&1 || rc=$?
if [[ $rc == 0 ]] || ! grep -q 'a rollback needs allow_rollback' "$WORK/plan2.log"; then
	cat "$WORK/plan2.log"
	fail "a rollback without allow_rollback is not a plan error"
fi
TF_VAR_release_version=1.0.0 TF_VAR_allow_rollback=true tf apply -input=false -auto-approve >"$WORK/apply4.log" 2>&1 || { tail -80 "$WORK/apply4.log"; fail "the rollback"; }
[[ $(tf output -raw version) == 1.0.0 ]] || fail "state's version after the rollback: $(tf output -raw version)"
on sudo notes-stow status --json | jq -e '.installed.version == "1.0.0" and .boot == "passive"' >/dev/null || fail "the VM is not on 1.0.0, in its passive system"

step "destroy: the install forgotten, the VM destroyed by its module"
TF_VAR_release_version=1.0.0 TF_VAR_deletion_protection=false tf apply -input=false -auto-approve -target=stowage_install.vm >"$WORK/apply5.log" 2>&1 || { tail -40 "$WORK/apply5.log"; fail "deletion_protection off"; }
TF_VAR_release_version=1.0.0 TF_VAR_deletion_protection=false tf destroy -input=false -auto-approve >"$WORK/destroy.log" 2>&1 || { tail -60 "$WORK/destroy.log"; fail "tofu destroy"; }
# shellcheck disable=SC2034 # read by the cleanup above
destroyed=1

echo PASS
