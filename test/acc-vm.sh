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
#     a release this provider reads (0.38.0 or later: the plan's dry runs use the VM's --dry-run): its suite library runs zot, ships
#     examples/minimal and its mirror;
#   - this repository's `make build` (bin/terraform-provider-stowage), installed by tofu init from
#     a filesystem mirror of this run's own;
#   - tofu ≥ 1.11, docker, ssh, jq, oras, zstd and xorriso on PATH, on a linux/amd64 machine TrueNAS
#     reaches: `make acc-vm` runs it in the acceptance runner (test/acc.Dockerfile), which has them.
#
# What it runs, in one OpenTofu root (examples/guides/truenas-vm, its VM a host named by DHCP under
# the env file's domain), tofu's PATH holding a docker that fails (the provider runs none):
#   1. examples/minimal with its kairos shape (and a secret input) shipped on beta with its qcow2
#      disk, carried to a TLS mirror of its own CA that asks for a sign-in, on the address TrueNAS
#      reaches this host at;
#   2. apply: stowage_vm_seed (truenas, the mirror read with its read account; the installer read
#      from the registry for this machine, ctl_path), its disk and an ISO of its user-data,
#      truenas/truenas making the VM with Stowage's `vm truenas` steps (ctl_path), and
#      stowage_vm_install waiting for its first boot under the one-time key, giving it its
#      registry login (registry-login) and the secret input over SSH (in neither the seed nor
#      state), installing, reading the VM's own host key and retiring the one-time key;
#   3. `tofu plan -detailed-exitcode` shows no change (the TrueNAS root and the install);
#   4. an input changed: the re-run alone;
#   5. 1.0.1 shipped and mirrored, version 1.0.1: a plan with plan_dry_run runs the VM's
#      upgrade --dry-run and changes nothing; then the upgrade, its reboot followed, the VM on
#      1.0.1 with seed_boot_id null, and only its own key accepted from then on;
#   6. version back to 1.0.0 without allow_rollback: a plan error; with it, a plan with
#      plan_dry_run runs rollback --dry-run, then the rollback;
#   7. the install forgotten (tofu state rm) and imported (<host>/notes-stow), its first apply
#      under the VM's own key (vm_known_hosts_line), then no change planned;
#   8. a destroy of the install refused by deletion_protection, the VM's install untouched;
#   9. destroy with deletion_protection off: stowage_vm_install forgotten, the VM, its devices,
#      zvol and staging dataset destroyed (the stop step first).
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

vm_ctl beta-1.0.0
# Preflight, in seconds: every input the shipped release requires on the kairos shape is one the
# root gives (GIVEN: its inputs, its secret inputs, and the domain inputs its domains fill, by
# route: app → APP_DOMAIN, api → API_DOMAIN). An input with a condition (when) applies under the
# root's TLS mode (internal) when its condition names tls, and is left to the installer otherwise.
GIVEN=" ADMIN_EMAIL ACC_TOKEN APP_DOMAIN API_DOMAIN "
"$CTL" describe --json >"$WORK/describe.json" 2>"$WORK/describe.log" || { cat "$WORK/describe.log"; fail "describe of the shipped release"; }
missing=$(jq -r '.inputs[] | select(.required and (.shapes == [] or (.shapes | index("kairos") != null)) and (.when == "" or (.when | test("tls"))))
	| .key' "$WORK/describe.json" | while read -r k; do [[ $GIVEN == *" $k "* ]] || printf '%s ' "$k"; done)
[[ -z $missing ]] || fail "the root gives none of the inputs the release requires on kairos: $missing(add them to the seed's and the install's inputs, secret inputs or domains, and to GIVEN)"

step "the site's mirror, TLS of its own CA, a sign-in to read, on the address TrueNAS reaches this host at"
export MIRROR_SIGN_IN=1
vm_mirror
mirror "$WORK/r1.oci.tar" --disks qcow2
INSTALLER=$REG/notes/notes-installer@$(digest_of notes/notes-installer:beta-1.0.0)
[[ $INSTALLER == *@sha256:* ]] || fail "no digest of the installer"

step "the OpenTofu root: examples/guides/truenas-vm, this provider from a filesystem mirror"
ROOT=$WORK/root
mkdir -p "$ROOT" "$WORK/disks" "$WORK/seeds"
cp -a "$PROV/examples/guides/truenas-vm/." "$ROOT/"
rm -rf "$ROOT/tests"
# The provider built here, from a filesystem mirror of this run's own (dev_overrides would leave
# tofu init querying the registry for it, which the root's other providers need init for).
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
# No docker on the PATH tofu runs with: the provider reads the installer from the registry and
# runs it natively (a docker run would fail here).
mkdir -p "$WORK/nodocker"
printf '#!/bin/sh\necho "acc-vm: the provider ran docker" >&2\nexit 99\n' >"$WORK/nodocker/docker"
chmod 755 "$WORK/nodocker/docker"
export TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1
TOKEN=$(token)
jq -n --arg installer "$INSTALLER" --arg name "$VMNAME" --arg h "$H" --arg addr "$ADDR" --arg key "$KEY" --argjson insecure "$TN_INSECURE" \
	--arg pool "$POOL" --arg dataset "$DATASET" --arg bridge "$BRIDGE" --arg mirror_ip "$MIRROR_IP" --arg site "$SITE" --arg ca "$MIRROR_CA" \
	--arg disks "$WORK/disks" --arg seeds "$WORK/seeds" \
	'{installer: $installer, name: $name, hostname: $h, vm_address: $addr, user: "ops", ssh_key_file: $key, insecure: $insecure,
	pool: $pool, parent_dataset: $dataset, nic_attach: $bridge, ssh_allow: [($mirror_ip + "/32")], cpus: 2, memory_mib: 4096,
	tls: "internal", domains: {app: "app.notes.test", api: "api.notes.test"}, registry: $site, registry_ca_file: $ca,
	pull_username: "pull", disk_dir: $disks, seed_dir: $seeds}' >"$ROOT/terraform.tfvars.json"
# The VM's own registry login, the mirror's read account (it asks for a sign-in), given over SSH,
# never in the seed or in state; the seed reads the mirror's disks with the same account, as the
# provider's mirror login.
cat >"$ROOT/acc.tf" <<'EOF'
variable "registry_login" {
  type      = string
  sensitive = true
  ephemeral = true
}

variable "mirror_password" {
  type      = string
  sensitive = true
}
EOF
python3 - "$ROOT" <<'PY'
import sys
root = sys.argv[1]
def edit(f, old, new):
    s = open(f).read()
    assert s.count(old) == 1, (f, old)
    open(f, "w").write(s.replace(old, new))
edit(root + "/main.tf", "  inputs                   = var.inputs\n  secret_inputs_wo ",
     "  inputs                   = var.inputs\n  registry_login_wo         = var.registry_login\n  registry_login_wo_version = 1\n  secret_inputs_wo ")
edit(root + "/versions.tf", "  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null\n",
     "  registry_ca = var.registry_ca_file != null ? file(var.registry_ca_file) : null\n  registry_username = \"vm\"\n  registry_password = var.mirror_password\n")
PY
export TF_VAR_pull_password=$PULL_TOKEN TF_VAR_secret_inputs="{\"ACC_TOKEN\":\"$TOKEN\"}" TF_VAR_inputs='{"ADMIN_EMAIL":"admin@notes.test"}'
TF_VAR_registry_login=$(cat "$MIRROR_VM_LOGIN")
export TF_VAR_registry_login TF_VAR_mirror_password=$MIRROR_VM_TOKEN TF_VAR_release_version=1.0.0
tf() { (cd "$ROOT" && PATH="$WORK/nodocker:$PATH" tofu "$@"); }
destroyed=
# A run that fails destroys the VM still: the install forgotten (its delete runs nothing), the VM destroyed.
CLEANUP=("[[ -n \$destroyed ]] || (cd $ROOT && tofu state rm stowage_vm_install.vm >>$WORK/destroy.log 2>&1; tofu destroy -auto-approve >>$WORK/destroy.log 2>&1)" "${CLEANUP[@]}")
# A run killed mid-apply leaves apply_logged's raw debug log: removed here.
CLEANUP=("rm -f $WORK/*.raw.log" "${CLEANUP[@]}")
tf init -input=false >"$WORK/init.log" 2>&1 || { cat "$WORK/init.log"; fail "tofu init"; }

step "apply: the seed, the VM, its first install over SSH under the one-time key"
tf apply -input=false -auto-approve >"$WORK/apply1.log" 2>&1 || { tail -80 "$WORK/apply1.log"; fail "the first apply"; }
on() { ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$WORK/known_hosts" -o HostKeyAlias=acc-vm -i "$KEY" -o IdentitiesOnly=yes "ops@$ADDR" -- "$@"; }
# apply_logged <name> <tofu apply arguments…> runs the apply with the provider's debug log on
# (TF_LOG_PROVIDER opens OpenTofu's provider log; the TrueNAS and external providers are turned
# off; one logging otherwise still could), keeps from it this provider's "ran on the VM" entries alone — the command, its exit, its
# standard output, and every indented line after it (a multi-line field's `  | …` lines, and the
# fields hclog writes after one) — in $WORK/<name>.vm.log,
# and removes the raw log, which another provider could have written a credential to.
apply_logged() { tf_logged apply "$@"; }
# plan_logged <name> <tofu plan arguments…>: apply_logged's for a plan.
plan_logged() { tf_logged plan "$@"; }
tf_logged() {
	local verb=$1 name=$2 rc=0
	shift 2
	TF_LOG_PROVIDER=DEBUG TF_LOG_PROVIDER_TRUENAS=OFF TF_LOG_PROVIDER_EXTERNAL=OFF TF_LOG_PATH="$WORK/$name.raw.log" \
		tf "$verb" "$@" >"$WORK/$name.log" 2>&1 || rc=$?
	if [[ -f $WORK/$name.raw.log ]]; then
		awk '/ran on the VM/ { on = 1; print; next } on && /^[[:space:]]/ { print; next } { on = 0 }' "$WORK/$name.raw.log" >"$WORK/$name.vm.log" || true
		rm -f "$WORK/$name.raw.log"
	fi
	return "$rc"
}

# vm_dump [<name>] prints, before the teardown, what the provider ran on the VM in apply <name>
# (apply_logged's entries) and the VM's own account: its status, its event journal and this
# boot's boot unit and health gate.
vm_dump() {
	if [[ -n ${1:-} && -f $WORK/$1.vm.log ]]; then
		printf -- '--- the provider on the VM (%s)\n' "$1" >&2
		tail -400 "$WORK/$1.vm.log" >&2
	fi
	printf -- '--- status --json\n' >&2
	on notes-stow status --json >&2 || true
	printf -- '--- events (the last 100)\n' >&2
	on notes-stow events --json | tail -100 >&2 || true
	local b
	for b in -1 0; do # the boot before (a trial boot that fell back) and this one
		printf -- '--- journal: notes.service, notes-health.service (boot %s)\n' "$b" >&2
		on sudo -n journalctl -b "$b" --no-pager -u notes.service -u notes-health.service | tail -120 >&2 || true
	done
}
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
! grep -qF "$MIRROR_VM_TOKEN" "$ROOT/terraform.tfstate" || fail "the VM's registry login is in state"
on notes-stow status --json --values | jq -e '.values.pull_login == true' >/dev/null || { on notes-stow status --json --values >&2; fail "the VM holds no registry login"; }
[[ $(jq -r '.resources[] | select(.type == "stowage_vm_seed") | .instances[0].attributes.ctl_path' "$ROOT/terraform.tfstate") == /* ]] || fail "the seed has no ctl_path"
! grep -aqF "$TOKEN" "$iso" || fail "the secret input is in the seed"

step "no change planned; a refresh reads the VM"
rc=0
tf plan -input=false -detailed-exitcode >"$WORK/plan1.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { cat "$WORK/plan1.log"; fail "a plan right after the apply is not empty (exit $rc)"; }

step "an input changed: the re-run alone, no reboot"
boot=$(on cat /proc/sys/kernel/random/boot_id)
TF_VAR_inputs='{"ADMIN_EMAIL":"other@notes.test"}' tf apply -input=false -auto-approve >"$WORK/apply2.log" 2>&1 || { tail -60 "$WORK/apply2.log"; fail "the re-run"; }
[[ $(on cat /proc/sys/kernel/random/boot_id) == "$boot" ]] || fail "the re-run rebooted the VM"
on sudo grep -q '^ADMIN_EMAIL=.*other@notes.test' /usr/local/notes/.env || fail "the re-run did not record the input"
export TF_VAR_inputs='{"ADMIN_EMAIL":"other@notes.test"}'

step "1.0.1 shipped and mirrored; vm.version 1.0.1: the upgrade, its reboot followed"
printf '1.0.1\n' >"$P/VERSION"
printf '### Changed\n- Release 1.0.1.\n' >"$P/changes/1.0.1.md"
"${S[@]}" ship --channel beta 2>"$WORK/ship-1.0.1.log" || { tail -50 "$WORK/ship-1.0.1.log"; fail "ship 1.0.1"; }
mirror "$WORK/r2.oci.tar" --disks none
# plan_dry_run: the VM's upgrade --dry-run passes at plan; nothing reboots, nothing changes: the
# last job's state, the release, the boot and the event journal's last entry as before the plan.
vm_now() { on notes-stow status --json | jq -c '{job, installed, boot_id, events}'; }
before=$(vm_now)
rc=0
TF_VAR_release_version=1.0.1 TF_VAR_plan_dry_run=true plan_logged plan-dry-upgrade -detailed-exitcode -input=false || rc=$?
[[ $rc == 2 ]] && grep -q 'notes-stow upgrade --version 1.0.1 --dry-run --json' "$WORK/plan-dry-upgrade.vm.log" ||
	{ cat "$WORK/plan-dry-upgrade.log" "$WORK/plan-dry-upgrade.vm.log"; fail "the upgrade's plan with plan_dry_run exited $rc, or ran no dry run"; }
[[ $(on cat /proc/sys/kernel/random/boot_id) == "$boot" ]] || fail "the plan's dry run rebooted the VM"
[[ $(vm_now) == "$before" ]] || fail "the plan's dry run changed the VM: $before, then $(vm_now)"
TF_VAR_release_version=1.0.1 apply_logged apply3 -input=false -auto-approve || { tail -80 "$WORK/apply3.log"; vm_dump apply3; fail "the upgrade"; }
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
# plan_dry_run: the VM's rollback --dry-run passes at plan; nothing reboots, nothing changes.
before=$(vm_now)
rc=0
TF_VAR_release_version=1.0.0 TF_VAR_allow_rollback=true TF_VAR_plan_dry_run=true plan_logged plan-dry-rollback -detailed-exitcode -input=false || rc=$?
[[ $rc == 2 ]] && grep -q 'notes-stow rollback --dry-run --json' "$WORK/plan-dry-rollback.vm.log" ||
	{ cat "$WORK/plan-dry-rollback.log" "$WORK/plan-dry-rollback.vm.log"; fail "the rollback's plan with plan_dry_run exited $rc, or ran no dry run"; }
[[ $(vm_now) == "$before" ]] || fail "the plan's dry run of the rollback changed the VM: $before, then $(vm_now)"
TF_VAR_release_version=1.0.0 TF_VAR_allow_rollback=true apply_logged apply4 -input=false -auto-approve || { tail -80 "$WORK/apply4.log"; vm_dump apply4; fail "the rollback"; }
[[ $(tf output -raw version) == 1.0.0 ]] || { vm_dump apply4; fail "state's version after the rollback: $(tf output -raw version)"; }
on notes-stow status --json | jq -e '.installed.version == "1.0.0" and .boot == "passive"' >/dev/null || { vm_dump apply4; fail "the VM is not on 1.0.0, in its passive system"; }

step "import: the install forgotten, imported by its ID, its first apply under the VM's own key, then no change planned"
export TF_VAR_release_version=1.0.0
tf state rm stowage_vm_install.vm >"$WORK/rm.log" 2>&1 || { cat "$WORK/rm.log"; fail "tofu state rm"; }
tf import -input=false stowage_vm_install.vm "$ADDR/notes-stow" >"$WORK/import.log" 2>&1 || { cat "$WORK/import.log"; fail "tofu import"; }
export TF_VAR_vm_known_hosts_line=$own
boot=$(on cat /proc/sys/kernel/random/boot_id)
tf apply -input=false -auto-approve >"$WORK/apply-import.log" 2>&1 || { tail -80 "$WORK/apply-import.log"; vm_dump; fail "the import's first apply"; }
[[ $(tf output -raw version) == 1.0.0 && $(tf output -raw host_key_line) == "$own" ]] ||
	fail "state after the import's apply: version $(tf output -raw version), host_key_line $(tf output -raw host_key_line)"
[[ $(on cat /proc/sys/kernel/random/boot_id) == "$boot" ]] || fail "the import's apply rebooted the VM"
! grep -qF "$TOKEN" "$ROOT/terraform.tfstate" || fail "the secret input is in state after the import's apply"
rc=0
tf plan -input=false -detailed-exitcode >"$WORK/plan3.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { cat "$WORK/plan3.log"; fail "a plan after the import's apply is not empty (exit $rc)"; }

step "a destroy of the install refused by deletion_protection: the VM's install untouched"
rc=0
tf destroy -input=false -auto-approve -target=stowage_vm_install.vm >"$WORK/destroy-refused.log" 2>&1 || rc=$?
[[ $rc != 0 ]] && grep -q 'deletion_protection is on' "$WORK/destroy-refused.log" || { cat "$WORK/destroy-refused.log"; fail "the destroy was not refused by deletion_protection"; }
[[ $(tf state list) == *stowage_vm_install.vm* ]] || fail "the install left state though its destroy was refused"
on notes-stow status --json | jq -e '.installed.version == "1.0.0"' >/dev/null || { vm_dump; fail "the VM does not run 1.0.0 after the refused destroy"; }

step "destroy: the install forgotten, the VM destroyed by its root"
TF_VAR_release_version=1.0.0 TF_VAR_deletion_protection=false tf apply -input=false -auto-approve -target=stowage_vm_install.vm >"$WORK/apply5.log" 2>&1 || { tail -40 "$WORK/apply5.log"; fail "deletion_protection off"; }
TF_VAR_release_version=1.0.0 TF_VAR_deletion_protection=false tf destroy -input=false -auto-approve >"$WORK/destroy.log" 2>&1 || { tail -60 "$WORK/destroy.log"; fail "tofu destroy"; }
# shellcheck disable=SC2034 # read by the cleanup above
destroyed=1

echo PASS
