#!/usr/bin/env bash
# Acceptance test of the Docker target against a real Stowage installer, through OpenTofu.
#
#   STOWAGE_REPO=<a Stowage checkout, its ./bin/stowage built> make acc-docker
#
# A real registry (zot), Stowage's examples/minimal shipped to it three times (1.0.0; 2.0.0, which
# adds an input and a migration 1.0.0 reads; 3.0.0, a rollback barrier), and a Docker host reached
# over SSH (sshd beside Docker-in-Docker, its host key pinned). The provider, built from this tree,
# is loaded by a dev_overrides OpenTofu configuration. Walks: create → a refresh plans nothing → a
# re-run (an input changed) → secret_rotations (a first number recorded, a bump replacing
# SESSION_SECRET, given replace: true, in one pass; a key that does not rotate refused at plan) →
# an upgrade adding an input → a rollback (allow_rollback; refused at
# plan without it) → an upgrade across the barrier → a refused upgrade after a re-run (the
# re-run's state saved, the refusal's reason and fix shown) → a refused rollback (the barrier) →
# import → a destroy refused by deletion_protection, then one deleting the data → the handoff
# chain of examples/guides/handoff-chain on the same host (A's kit to B's first install, B's back
# to A by its re-run, both proven, in one apply; a plan after it plans nothing; a kit given back
# with another value taken, then shown by A's refresh and given back; destroyed). Prints PASS.
#
# Needs Docker, curl, jq, python3, ssh, ssh-keygen and network access (images, OpenTofu).
set -euo pipefail

REPO=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
: "${STOWAGE_REPO:?set STOWAGE_REPO to a Stowage checkout (>= 0.38.0) whose ./bin/stowage is built}"
STOWAGE=$STOWAGE_REPO/bin/stowage
[[ -x $STOWAGE ]] || { echo "FAIL: $STOWAGE is missing: run make build in $STOWAGE_REPO" >&2; exit 1; }
PROVIDER_BIN=${PROVIDER_BIN:-$REPO/bin/terraform-provider-stowage}
[[ -x $PROVIDER_BIN ]] || { echo "FAIL: $PROVIDER_BIN is missing: run make build" >&2; exit 1; }

# Pinned by digest (zot, htpasswd and cosign as Stowage's own suites pin them).
ZOT=ghcr.io/project-zot/zot-linux-amd64:v2.1.22@sha256:46f688dc26315a35a247e1784368829e66a67bf91de94c7d8d1645044fac1d5b
HTPASSWD=httpd:2.4-alpine@sha256:3440c39d8d6f54fa9ad2549e5a60c19ddd435faadc29c1ad28aa795f71888889
COSIGN=gcr.io/projectsigstore/cosign:v3.0.2@sha256:b29487e48205d875c324c79583e2806d9d269c0fa299e0861bbec023d8430c8b
DIND=docker:29.9.3-dind@sha256:08a98015fce594af7c382883bea080ea206de74402a95da9dc52b6428c8cd121
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
RCLONE=rclone/rclone:1.76.0@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466
AGE_URL=https://github.com/FiloSottile/age/releases/download/v1.3.2/age-v1.3.2-linux-amd64.tar.gz
AGE_SHA=cbe24006683f8eb669266162894b9a522a1af52f2665fbc63a4bb032ed26ac10

CACHE=${XDG_CACHE_HOME:-$HOME/.cache}
mkdir -p "$CACHE"
WORK=$(mktemp -d "$CACHE/tps-acc-docker.XXXXXX") # on disk: image builds and exports are large
N=acc-docker-$$                                    # every container, network and volume made
export XDG_CACHE_HOME=$WORK/cache

cleanup() {
	docker rm -f "$N-target" "$N-zot" "$N-dav" "$N-net" >/dev/null 2>&1 || true
	docker buildx rm "$N-builder" >/dev/null 2>&1 || true
	docker volume rm -f "$N-docker" >/dev/null 2>&1 || true
	# The images ship built and pushed, kept by tag on this host's Docker.
	docker images --format '{{.Repository}}:{{.Tag}}' --filter "reference=$REG/notes/*" 2>/dev/null | xargs -r docker rmi >/dev/null 2>&1 || true
	if [[ -n ${ACC_LOGS:-} ]]; then mkdir -p "$ACC_LOGS" && cp "$WORK"/*.log "$ACC_LOGS"/ 2>/dev/null || true; fi
	rm -rf "$WORK"
}
REG=
trap cleanup EXIT

step() { printf '\n== %s  [%dm%02ds]\n' "$*" $((SECONDS / 60)) $((SECONDS % 60)); }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
token() { head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }

# shellcheck source=test/tofu.sh
. "$REPO/test/tofu.sh"
step "OpenTofu $TOFU_VERSION, the provider"
TOFU=$(XDG_CACHE_HOME=$CACHE tofu_path) || fail "OpenTofu $TOFU_VERSION" # the cache's, not the run's own
mkdir -p "$WORK/plugins" "$WORK/root"
cp "$PROVIDER_BIN" "$WORK/plugins/terraform-provider-stowage"
cat >"$WORK/tofurc" <<HCL
provider_installation {
  dev_overrides { "registry.opentofu.org/norvexlabs/stowage" = "$WORK/plugins" }
  direct {}
}
HCL
export TF_CLI_CONFIG_FILE=$WORK/tofurc TF_IN_AUTOMATION=1
tf() { (cd "$WORK/root" && "$TOFU" "$@" -no-color); }

step "the network the registry and the Docker host share, zot"
ZP=$(free_port) SP=$(free_port)
while [[ $SP == "$ZP" ]]; do SP=$(free_port); done
REG=127.0.0.1:$ZP
# One network namespace holds zot and the Docker host: the host's Docker reaches the registry on
# its loopback, at the address ship published to (a loopback registry is plain HTTP to Docker and
# to the installer alike), and this machine reaches both on the ports it publishes.
docker run -d --name "$N-net" --memory 64m -p "$REG:$ZP" -p "127.0.0.1:$SP:$SP" --entrypoint sleep "$DIND" infinity >/dev/null
PULL_TOKEN=$(token)
export STOWAGE_PUSH_TOKEN NOTES_PULL_TOKEN=$PULL_TOKEN
STOWAGE_PUSH_TOKEN=$(token)
mkdir -p "$WORK/zot/data"
printf '%s\n' "$PULL_TOKEN" "$STOWAGE_PUSH_TOKEN" |
	docker run --rm -i --entrypoint sh "$HTPASSWD" -ec \
		'read -r a; read -r b; printf "%s\n" "$a" | htpasswd -niB pull; printf "%s\n" "$b" | htpasswd -niB publisher' |
	grep . >"$WORK/zot/htpasswd"
cat >"$WORK/zot/config.json" <<JSON
{
  "storage": { "rootDirectory": "/etc/zot/data" },
  "http": {
    "address": "0.0.0.0", "port": "$ZP", "compat": ["docker2s2"],
    "auth": { "htpasswd": { "path": "/etc/zot/htpasswd" } },
    "accessControl": { "repositories": { "notes/**": {
      "policies": [
        { "users": ["pull"], "actions": ["read"] },
        { "users": ["publisher"], "actions": ["read", "create", "update", "delete"] }
      ],
      "defaultPolicy": [] } } }
  },
  "log": { "level": "warn" }
}
JSON
docker run -d --name "$N-zot" --memory 512m --network "container:$N-net" --user "$(id -u):$(id -g)" -v "$WORK/zot:/etc/zot" "$ZOT" serve /etc/zot/config.json >/dev/null
for _ in $(seq 1 50); do
	code=$(curl -s -o /dev/null -w '%{http_code}' "http://$REG/v2/" || true)
	[[ $code == 401 ]] && break
	sleep 0.2
done
[[ $code == 401 ]] || { docker logs "$N-zot" >&2; fail "zot did not come up (last status $code)"; }

step "examples/minimal, shipped: 1.0.0, 2.0.0 (an input added), 3.0.0 (a rollback barrier)"
P=$WORK/notes
cp -r "$STOWAGE_REPO/examples/minimal" "$P"
export STOWAGE_COSIGN=$WORK/cosign COSIGN_PASSWORD
COSIGN_PASSWORD=$(token)
cat >"$STOWAGE_COSIGN" <<SH
#!/bin/sh
exec docker run --rm --network host --user $(id -u):$(id -g) -e HOME=/tmp -e DOCKER_CONFIG -e COSIGN_PASSWORD -v "$WORK:$WORK" -w "$WORK" $COSIGN "\$@"
SH
chmod +x "$STOWAGE_COSIGN"
mkdir -p "$P/keys"
"$STOWAGE_COSIGN" generate-key-pair --output-key-prefix "$P/keys/release" >/dev/null 2>&1
[[ -s $P/keys/release.pub ]] || fail "cosign generate-key-pair"
python3 - "$P/stowage.yaml" "$PG" <<'PY'
import sys
path, pg = sys.argv[1:]
s = open(path).read()
lines = [l for l in s.splitlines() if not l.lstrip().startswith("postgres: { ref:")]
assert len(lines) == len(s.splitlines()) - 1
lines.insert(lines.index("images:") + 1, f'  postgres: {{ ref: "{pg}" }}')
s = "\n".join(lines) + "\n"
old = '  - { key: ADMIN_EMAIL, prompt: "Administrator email", required: true }\n'
assert s.count(old) == 1, old
s = s.replace(old, old + '  - { key: E2E_TOKEN, prompt: "A token", secret: true, optional: true }\n')
old = '  - { key: SESSION_SECRET,    gen: hex32 }\n'
assert s.count(old) == 1, old
s = s.replace(old, '  - { key: SESSION_SECRET,    gen: hex32, replace: true }\n')
open(path, "w").write(s)
PY
# ship builds the example's images with buildx: a builder of its own, capped as the test's
# containers are (BUILDX_BUILDER, which docker buildx reads), removed at the exit.
docker buildx create --name "$N-builder" --driver docker-container --driver-opt memory=4g --driver-opt memory-swap=4g >/dev/null
export BUILDX_BUILDER=$N-builder
S=("$STOWAGE" -C "$P" --test-registry "$REG/notes" --test-push publisher:STOWAGE_PUSH_TOKEN --test-pull pull:NOTES_PULL_TOKEN
	--test-key "$P/keys/release.key" --test-public-key keys/release.pub --shapes docker)
"${S[@]}" lock --update >/dev/null
"${S[@]}" lint >"$WORK/lint.log" 2>&1 || { cat "$WORK/lint.log"; fail "lint"; }
# digest_of <repo:tag>: the digest the registry holds for a tag, read with the pull account.
digest_of() {
	curl -sfI -u "pull:$PULL_TOKEN" -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json' \
		"http://$REG/v2/${1%:*}/manifests/${1##*:}" | tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: //p'
}
# release <version> <schema.json>: the example at that version, shipped on stable; sets INSTALLER.
release() {
	if [[ $1 != 1.0.0 ]]; then
		printf '%s\n' "$1" >"$P/VERSION"
		printf '### Changed\n- Release %s.\n' "$1" >"$P/changes/$1.md"
		printf '%s\n' "$2" >"$P/api/schema.json"
	fi
	"${S[@]}" ship --channel stable >"$WORK/ship-$1.log" 2>&1 || { cat "$WORK/ship-$1.log"; fail "ship $1"; }
	local d
	d=$(digest_of "notes/notes-installer:$1")
	[[ $d == sha256:* ]] || fail "no installer of $1 in the registry"
	INSTALLER=$REG/notes/notes-installer@$d
}
[[ $(cat "$P/VERSION") == 1.0.0 ]] || fail "examples/minimal is not at 1.0.0: $(cat "$P/VERSION")"
release 1.0.0
I1=$INSTALLER
python3 - "$P/stowage.yaml" <<'PY'
import sys
path = sys.argv[1]
s = open(path).read()
old = '  - { key: E2E_TOKEN, prompt: "A token", secret: true, optional: true }\n'
assert s.count(old) == 1, old
s = s.replace(old, old + '  - { key: E2E_NOTE, prompt: "A note", optional: true }\n')
open(path, "w").write(s)
PY
release 2.0.0 '{"version": 2, "barriers": {}, "reads_newer": true}'
I2=$INSTALLER
release 3.0.0 '{"version": 3, "barriers": {"3": "notes moved to a new table"}, "reads_newer": true}'
I3=$INSTALLER

step "the Docker host: sshd beside Docker-in-Docker, its host key pinned"
mkdir -p "$WORK/target"
ssh-keygen -q -t ed25519 -N '' -C acc-host -f "$WORK/target/host_key"
ssh-keygen -q -t ed25519 -N '' -C acc-user -f "$WORK/user_key"
cp "$WORK/user_key.pub" "$WORK/target/authorized_keys"
cat >"$WORK/target/start.sh" <<SH
#!/bin/sh
set -e
apk add --no-cache openssh-server >/dev/null
adduser -D -s /bin/sh ops
passwd -u ops >/dev/null 2>&1 || sed -i 's/^ops:!/ops:*/' /etc/shadow
addgroup ops docker
install -d -m 700 -o ops -g ops /home/ops/.ssh
install -m 600 -o ops -g ops /acc/authorized_keys /home/ops/.ssh/authorized_keys
install -m 600 /acc/host_key /etc/ssh/ssh_host_ed25519_key
/usr/sbin/sshd -p $SP -h /etc/ssh/ssh_host_ed25519_key -o PasswordAuthentication=no -o PermitRootLogin=no
exec dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock
SH
chmod 755 "$WORK/target/start.sh"
docker run -d --name "$N-target" --privileged --memory 4g --network "container:$N-net" -e DOCKER_TLS_CERTDIR= \
	-v "$N-docker:/var/lib/docker" -v "$WORK/target:/acc:ro" --entrypoint /acc/start.sh "$DIND" >/dev/null
on_target() { docker exec "$N-target" "$@"; }
KNOWN="127.0.0.1 $(cut -d' ' -f1-2 "$WORK/target/host_key.pub")"
printf '[127.0.0.1]:%s %s\n' "$SP" "${KNOWN#127.0.0.1 }" >"$WORK/known_hosts"
# Up once ops reaches the daemon over SSH, the pinned key checked.
for _ in $(seq 1 60); do
	ssh -o BatchMode=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile="$WORK/known_hosts" -o ConnectTimeout=5 \
		-i "$WORK/user_key" -p "$SP" ops@127.0.0.1 docker version --format '{{.Server.Version}}' >"$WORK/ssh.log" 2>&1 && break
	sleep 1
done
grep -qE '^[0-9]+[.]' "$WORK/ssh.log" || { cat "$WORK/ssh.log"; docker logs "$N-target" >&2; fail "ssh to the Docker host's daemon"; }

# config: the root's main.tf. Every argument from the environment of the call: INST, ADMIN, NOTE
# (empty: absent), ROT (secret_rotations' entries, empty: absent), ROLLBACK, PROTECT, DATA.
E2E_TOKEN=$(token)
export TF_VAR_e2e_token=$E2E_TOKEN TF_VAR_pull_token=$PULL_TOKEN
config() {
	local note=
	[[ -z ${NOTE:-} ]] || note="E2E_NOTE = \"$NOTE\""
	local rot=
	[[ -z ${ROT:-} ]] || rot="secret_rotations = { $ROT }"
	local backup= restore=
	[[ -z ${BACKUP:-} ]] || backup="backup = { destination = \"dav:backups\", recipients = [\"$AGE_RECIPIENT\"] }
  backup_rclone_config_wo         = file(\"$WORK/dav.conf\")
  backup_rclone_config_wo_version = 1"
	[[ -z ${RESTORE:-} ]] || restore="secrets_wo          = file(\"$WORK/secrets.yaml\")
  restore             = { object = \"$RESTORE\", unrecorded = true }
  restore_identity_wo = file(\"$WORK/age.key\")"
	cat >"$WORK/root/main.tf" <<HCL
terraform {
  required_providers {
    stowage = { source = "registry.opentofu.org/norvexlabs/stowage" }
  }
}
variable "e2e_token" {
  type      = string
  sensitive = true
  ephemeral = true
}
variable "pull_token" {
  type      = string
  sensitive = true
}
provider "stowage" {
  username    = "pull"
  password    = var.pull_token
  public_keys = [file("$P/keys/release.pub")]
}
resource "stowage_docker_install" "notes" {
  installer = "$INST"
  inputs = {
    ADMIN_EMAIL = "$ADMIN"
    $note
  }
  secret_inputs_wo         = { E2E_TOKEN = var.e2e_token }
  secret_inputs_wo_version = 1
  $rot
  $backup
  $restore
  ports                    = { app = 18080, api = 18000 }
  allow_rollback           = ${ROLLBACK:-false}
  plan_dry_run             = ${DRY:-false}
  deletion_protection      = ${PROTECT:-true}
  delete_data              = ${DATA:-false}
  host                     = "127.0.0.1"
  port                     = $SP
  user                     = "ops"
  private_key_file         = "$WORK/user_key"
  known_hosts_line         = "$KNOWN"
  work_dir                 = "/home/ops"
}
HCL
}
# apply / plan_nothing: an apply that succeeds; a plan that plans nothing (exit 0).
apply() { tf apply -auto-approve -input=false >"$WORK/$1.log" 2>&1 || { cat "$WORK/$1.log"; fail "apply: $1"; }; }
plan_nothing() {
	local rc=0
	tf plan -detailed-exitcode -input=false >"$WORK/$1.log" 2>&1 || rc=$?
	[[ $rc == 0 ]] || { cat "$WORK/$1.log"; fail "$1: the plan is not empty (exit $rc)"; }
}
# apply_fails <log> <ERE…>: an apply that fails, its output matching each ERE.
apply_fails() {
	local log=$WORK/$1.log re
	shift
	if tf apply -auto-approve -input=false >"$log" 2>&1; then cat "$log"; fail "$(basename "$log" .log): the apply succeeded"; fi
	for re in "$@"; do tr -s ' \n' ' ' <"$log" | grep -E -- "$re" >/dev/null || { cat "$log"; fail "$(basename "$log" .log): no /$re/ in the error"; }; done
}
# plan_fails <log> <ERE…>: a plan that fails, its output matching each ERE.
plan_fails() {
	local log=$WORK/$1.log re
	shift
	if tf plan -input=false >"$log" 2>&1; then cat "$log"; fail "$(basename "$log" .log): the plan succeeded"; fi
	for re in "$@"; do tr -s ' \n' ' ' <"$log" | grep -E -- "$re" >/dev/null || { cat "$log"; fail "$(basename "$log" .log): no /$re/ in the error"; }; done
}
# state <jq filter>: true when the filter holds on the resource's state.
state() {
	tf show -json >"$WORK/show.json" 2>"$WORK/show.log" || { cat "$WORK/show.log"; fail "tofu show"; }
	jq -e ".values.root_module.resources[] | select(.address == \"stowage_docker_install.notes\") | .values | $1" "$WORK/show.json" >/dev/null ||
		{ jq '.values.root_module.resources[].values' "$WORK/show.json"; fail "the state does not satisfy: $1"; }
}
# env_of <KEY>: the value the install's .env on the Docker host records.
env_of() { on_target sed -n "s/^$1='\{0,1\}\([^']*\)'\{0,1\}\$/\1/p" /home/ops/notes/.env; }
# env_is <KEY> <value>: the install's .env on the Docker host records that value.
env_is() {
	local v
	v=$(env_of "$1")
	[[ $v == "$2" ]] || fail "the install records $1=$v, not $2"
}
# no_secrets <when>: neither the secret input nor the pull account's token is in the state or any
# other file of the root.
no_secrets() {
	[[ -s $WORK/root/terraform.tfstate ]] || fail "no state at $WORK/root/terraform.tfstate ($1)"
	! grep -rqF -- "$E2E_TOKEN" "$WORK/root" || fail "the secret input is in the root's files ($1)"
	! grep -rqF -- "$PULL_TOKEN" "$WORK/root" || fail "the pull account's token is in the root's files ($1)"
}
ready() { on_target wget -qO- "http://127.0.0.1:18000/healthz/ready" >/dev/null || fail "the api does not answer on the Docker host ($*)"; }

step "create: 1.0.0, an input, a secret input, ports"
INST=$I1 ADMIN=admin@notes.test config
apply create
state '.status == "installed" and .version == "1.0.0" and .revision == 1 and .previous == null and .short_name == "notes" and .channel == "stable" and .id == "/home/ops/notes"'
env_is ADMIN_EMAIL admin@notes.test
env_is E2E_TOKEN "$E2E_TOKEN"
ready "after the create"
no_secrets "after the create"

step "a refresh plans nothing"
plan_nothing refresh

step "a re-run: an input changed"
INST=$I1 ADMIN=ops@notes.test config
apply rerun
env_is ADMIN_EMAIL ops@notes.test
state '.version == "1.0.0" and .inputs.ADMIN_EMAIL == "ops@notes.test"'
plan_nothing after-rerun

step "secret_rotations: a first number recorded, a bump replacing SESSION_SECRET in one pass, a key that does not rotate refused at plan"
session=$(env_of SESSION_SECRET)
[[ -n $session ]] || fail "the install records no SESSION_SECRET"
INST=$I1 ADMIN=ops@notes.test ROT="SESSION_SECRET = 1" config
apply rotations-first
env_is SESSION_SECRET "$session"
state '.secret_rotations.SESSION_SECRET == 1'
INST=$I1 ADMIN=ops@notes.test ROT="SESSION_SECRET = 2" config
apply rotations-bump
[[ $(env_of SESSION_SECRET) != "$session" && -n $(env_of SESSION_SECRET) ]] || fail "SESSION_SECRET was not replaced"
state '.secret_rotations.SESSION_SECRET == 2'
ready "after the rotation"
plan_nothing after-rotation
INST=$I1 ADMIN=ops@notes.test ROT="SESSION_SECRET = 2, POSTGRES_PASSWORD = 1" config
plan_fails rotations-refused 'POSTGRES_PASSWORD does not rotate on docker'

step "an upgrade to 2.0.0, adding the input it declares"
INST=$I2 ADMIN=ops@notes.test NOTE=hello config
apply upgrade2
state ".version == \"2.0.0\" and .installer == \"$I2\" and .previous.version == \"1.0.0\" and .previous.installer == \"$I1\" and .inputs.E2E_NOTE == \"hello\""
env_is E2E_NOTE hello
ready "after the upgrade to 2.0.0"
plan_nothing after-upgrade2

step "a rollback to 1.0.0: a plan error without allow_rollback, then the kept installer's rollback"
INST=$I1 ADMIN=ops@notes.test NOTE=hello config
rc=0; tf plan -input=false >"$WORK/rollback-plan.log" 2>&1 || rc=$?
[[ $rc != 0 ]] && grep -q 'allow_rollback' "$WORK/rollback-plan.log" || { cat "$WORK/rollback-plan.log"; fail "a rollback planned without allow_rollback"; }
INST=$I1 ADMIN=ops@notes.test NOTE=hello ROLLBACK=true config
apply rollback
state ".version == \"1.0.0\" and .installer == \"$I1\""
ready "after the rollback"
# The install keeps E2E_NOTE, which 1.0.0 does not declare (status lists it no more): state keeps it.
env_is E2E_NOTE hello
state '.inputs.E2E_NOTE == "hello"'
plan_nothing after-rollback

step "an upgrade to 3.0.0 (a rollback barrier)"
INST=$I3 ADMIN=ops@notes.test NOTE=hello ROLLBACK=true config
apply upgrade3
state ".version == \"3.0.0\" and .installer == \"$I3\" and .previous.version == \"1.0.0\""
plan_nothing after-upgrade3

step "an older release by upgrade, after a re-run: the re-run's state saved, the refusal shown with its fix"
INST=$I2 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true config
apply_fails refused-upgrade 'runs notes 3[.]0[.]0' 'refused by the installer [(]upgrade, exit 3[)] before anything changed'
env_is ADMIN_EMAIL refused@notes.test
state ".version == \"3.0.0\" and .installer == \"$I3\" and .inputs.ADMIN_EMAIL == \"refused@notes.test\""
INST=$I3 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true config
plan_nothing after-refused-upgrade

step "plan_dry_run: the rollback across the barrier refused at plan, the barrier named"
INST=$I1 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true DRY=true config
plan_fails dry-rollback 'notes moved to a new table' "The plan's dry run of the rollback"
state ".version == \"3.0.0\" and .installer == \"$I3\""

step "a rollback across the barrier: refused, the barrier named"
INST=$I1 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true config
apply_fails refused-rollback 'notes moved to a new table' 'refused by the installer [(]rollback, exit 3[)]'
state ".version == \"3.0.0\" and .installer == \"$I3\""
INST=$I3 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true config
plan_nothing after-refused-rollback

step "import: the state forgotten, the install imported by its kept installer, its first apply"
tf state rm stowage_docker_install.notes >"$WORK/state-rm.log" 2>&1 || { cat "$WORK/state-rm.log"; fail "tofu state rm"; }
tf import -input=false stowage_docker_install.notes 127.0.0.1:/home/ops/notes/notes-stow >"$WORK/import.log" 2>&1 || { cat "$WORK/import.log"; fail "tofu import"; }
state '.id == "/home/ops/notes" and .short_name == "notes" and .deletion_protection == true'
apply after-import
state ".version == \"3.0.0\" and .installer == \"$I3\" and .status == \"installed\" and .inputs.ADMIN_EMAIL == \"refused@notes.test\""
env_is E2E_TOKEN "$E2E_TOKEN"
no_secrets "after the import's apply"
plan_nothing after-import-plan

# A machine made again: the backup settings declared and read back, a backup and the secrets
# export taken on the host, the install destroyed with its data, then made again in one apply
# (secrets_wo, backup, restore), its data back.
step "backup settings declared: set, read back, a change on the host shown and undone"
tool_dir=$CACHE/terraform-provider-stowage-tools/age-${AGE_SHA:0:12}
if [[ ! -x $tool_dir/age/age ]]; then
	mkdir -p "$tool_dir"
	curl -fsSL "$AGE_URL" -o "$WORK/age.tgz" || fail "downloading age"
	echo "$AGE_SHA  $WORK/age.tgz" | sha256sum -c - >/dev/null || fail "age's archive does not match its checksum"
	tar -xzf "$WORK/age.tgz" -C "$tool_dir"
fi
AGE=$tool_dir/age/age
"$tool_dir/age/age-keygen" -o "$WORK/age.key" 2>/dev/null || fail "age-keygen"
AGE_RECIPIENT=$("$tool_dir/age/age-keygen" -y "$WORK/age.key")
DAVPORT=$(free_port)
mkdir -p "$WORK/dav"
docker run -d --name "$N-dav" --user "$(id -u):$(id -g)" --network "container:$N-net" -v "$WORK/dav:/data" "$RCLONE" \
	serve webdav /data --addr "127.0.0.1:$DAVPORT" >/dev/null
printf '[dav]\ntype = webdav\nurl = http://127.0.0.1:%s\nvendor = other\n' "$DAVPORT" >"$WORK/dav.conf"
# The logs are read whole before they are searched: grep -q ending at its match could stop
# docker logs with SIGPIPE, failing the pipeline under pipefail.
dav_up() { local logs; logs=$(docker logs "$N-dav" 2>&1) && grep -q 'WebDav Server started' <<<"$logs"; }
for _ in $(seq 1 50); do dav_up && break; sleep 0.2; done
dav_up || { docker logs "$N-dav"; fail "the WebDAV server did not start"; }
as_ops() { docker exec -u ops "$N-target" /home/ops/notes/notes-stow "$@" --dir /home/ops/notes; }
INST=$I3 ADMIN=refused@notes.test NOTE=hello BACKUP=1 config
apply backup-config
state '.backup.destination == "dav:backups" and .backup.recipients == ["'"$AGE_RECIPIENT"'"] and .backup.schedule != null and .backup_rclone_config_wo == null'
as_ops backup config --json | jq -e '.configured and .destination == "dav:backups" and .rclone_config_from == "operator"' >/dev/null || fail "the install does not hold the settings"
plan_nothing backup-refresh
as_ops backup config --remove >/dev/null || fail "backup config --remove on the host"
rc=0; tf plan -detailed-exitcode -input=false >"$WORK/backup-drift.log" 2>&1 || rc=$?
[[ $rc == 2 ]] && grep -q 'destination' "$WORK/backup-drift.log" || { cat "$WORK/backup-drift.log"; fail "settings removed on the host do not show in the plan (exit $rc)"; }
apply backup-again
as_ops backup config --json | jq -e '.configured and .rclone_config_from == "operator"' >/dev/null || fail "the apply did not set the settings and the rclone config again"
plan_nothing backup-again-plan

step "a machine made again: a backup and the secrets export, the install destroyed with its data, then one apply (secrets_wo, backup, restore)"
pg_target() { on_target docker ps -q --filter "label=com.docker.compose.project=$(on_target sed -n "s/^COMPOSE_PROJECT_NAME=['\"]\{0,1\}\([^'\"]*\).*/\1/p" /home/ops/notes/.env)" --filter label=com.docker.compose.service=postgres; }
on_target docker exec "$(pg_target)" psql -U notes -d notes -c 'CREATE TABLE acc_backup (n int); INSERT INTO acc_backup VALUES (7)' >/dev/null || fail "a row to back up"
as_ops backup run >"$WORK/backup-run.log" 2>&1 || { cat "$WORK/backup-run.log"; fail "backup run"; }
object=$(as_ops backup list | awk '$1 ~ /^notes-.*[.]pgdump[.]age$/ {print $1}' | tail -n1)
[[ -n $object && -s $WORK/dav/backups/$object ]] || { as_ops backup list; fail "no backup in the destination"; }
as_ops secrets export --recipient "$AGE_RECIPIENT" --output - >"$WORK/secrets.age" 2>"$WORK/secrets-export.log" || { cat "$WORK/secrets-export.log"; fail "secrets export"; }
"$AGE" -d -i "$WORK/age.key" -o "$WORK/secrets.yaml" "$WORK/secrets.age" || fail "the secrets export does not decrypt"
INST=$I3 ADMIN=refused@notes.test NOTE=hello BACKUP=1 PROTECT=false DATA=true config
apply made-again-unprotect
tf destroy -auto-approve -input=false >"$WORK/made-again-destroy.log" 2>&1 || { cat "$WORK/made-again-destroy.log"; fail "tofu destroy before the machine is made again"; }
! on_target test -e /home/ops/notes || fail "the install is left after the destroy"
INST=$I3 ADMIN=refused@notes.test NOTE=hello BACKUP=1 RESTORE=$object config
apply made-again
[[ $(on_target docker exec "$(pg_target)" psql -U notes -d notes -tAc 'SELECT n FROM acc_backup') == 7 ]] || fail "the restore did not bring the backup's data back"
docker exec -u ops "$N-target" /home/ops/notes/notes-stow status --dir /home/ops/notes --json | jq -e '.restore_awaited | not' >/dev/null || fail "the install still awaits a restore"
state '.backup.destination == "dav:backups" and .restore.object == "'"$object"'" and .restore_identity_wo == null and .secrets_wo == null'
! grep -rqF -- "$(grep -o 'AGE-SECRET-KEY-[A-Z0-9]*' "$WORK/age.key")" "$WORK/root" || fail "the age identity is in the root's files"
ready "after the machine is made again"
plan_nothing made-again-plan
env_is E2E_TOKEN "$E2E_TOKEN"

step "destroy: refused by deletion_protection, then with the data"
if tf destroy -auto-approve -input=false >"$WORK/destroy-refused.log" 2>&1; then cat "$WORK/destroy-refused.log"; fail "a destroy ran with deletion_protection on"; fi
grep -q 'deletion_protection is on' "$WORK/destroy-refused.log" || { cat "$WORK/destroy-refused.log"; fail "the destroy's refusal does not name deletion_protection"; }
on_target test -d /home/ops/notes || fail "the refused destroy removed the install"
project=$(on_target sed -n "s/^COMPOSE_PROJECT_NAME=['\"]\{0,1\}\([^'\"]*\).*/\1/p" /home/ops/notes/.env)
[[ -n $project ]] || fail "the install records no compose project"
INST=$I3 ADMIN=refused@notes.test NOTE=hello ROLLBACK=true PROTECT=false DATA=true config
apply unprotect
tf destroy -auto-approve -input=false >"$WORK/destroy.log" 2>&1 || { cat "$WORK/destroy.log"; fail "tofu destroy"; }
! on_target test -e /home/ops/notes || fail "the install directory is left after the destroy"
[[ -z $(on_target docker ps -aq --filter "label=com.docker.compose.project=$project") ]] || fail "containers of the install are left"
[[ -z $(on_target docker volume ls -q --filter "label=com.docker.compose.project=$project") ]] || fail "volumes of the install are left (delete_data)"
[[ $(tf state list 2>/dev/null) == "" ]] || fail "the state still holds the resource"

# The handoff chain: examples/guides/handoff-chain as it is, on the same host. Install A issues a
# kit B's first install takes; B issues one back, which A takes by its re-run; each proves its kit
# was taken. One apply, no verb by hand; a plan after it plans nothing; the kits nowhere on disk.
step "handoff chain: A's kit to B's first install, B's back to A, both proven, in one apply"
mkdir -p "$WORK/chain"
cp "$REPO/examples/guides/handoff-chain/"*.tf "$WORK/chain/"
on_target su ops -c 'mkdir -p /home/ops/a /home/ops/b'
export TF_VAR_installer=$I3 TF_VAR_pull_username=pull TF_VAR_pull_password=$PULL_TOKEN TF_VAR_host=127.0.0.1 TF_VAR_port=$SP \
	TF_VAR_user=ops TF_VAR_private_key_file=$WORK/user_key TF_VAR_known_hosts_line=$KNOWN \
	TF_VAR_work_dirs='{ a = "/home/ops/a", b = "/home/ops/b" }' \
	TF_VAR_ports='{ a = { app = 18080, api = 18000 }, b = { app = 18081, api = 18001 } }' \
	TF_VAR_site_a_admin=a@notes.test TF_VAR_site_b_admin=b@notes.test
TF_VAR_release_public_key=$(cat "$P/keys/release.pub")
export TF_VAR_release_public_key
ctf() { (cd "$WORK/chain" && "$TOFU" "$@" -no-color); }
capply() { ctf apply -auto-approve -input=false >"$WORK/$1.log" 2>&1 || { cat "$WORK/$1.log"; fail "apply: $1"; }; }
cplan() {
	local rc=0
	ctf plan -detailed-exitcode -input=false >"$WORK/$1.log" 2>&1 || rc=$?
	[[ $rc == "$2" ]] || { cat "$WORK/$1.log"; fail "$1: the plan exited $rc, not $2"; }
}
env_at() {
	local v
	v=$(on_target sed -n "s/^$2='\{0,1\}\([^']*\)'\{0,1\}\$/\1/p" "/home/ops/$1/notes/.env")
	[[ $v == "$3" ]] || fail "the install in /home/ops/$1 records $2=$v, not $3"
}
# no_kit <when>: no kit (its armor), and so no code beside it, in any file of the root.
no_kit() { ! grep -rqF -- 'BEGIN AGE ENCRYPTED' "$WORK/chain" || fail "a kit is in the root's files ($1)"; }
capply chain
env_at a ADMIN_EMAIL a@notes.test
env_at b ADMIN_EMAIL b@notes.test
ctf output -json proven >"$WORK/proven.json" 2>&1 || { cat "$WORK/proven.json"; fail "tofu output proven"; }
jq -e '.a_to_b == true and .b_to_a == true' "$WORK/proven.json" >/dev/null || { cat "$WORK/proven.json"; fail "a kit is not proven"; }
no_kit "after the chain's apply"
cplan chain-again 0

step "handoff chain: B gives A back another value: taken by A's re-run, then A's refresh shows it"
TF_VAR_returned_admin=other@notes.test capply chain-returned
env_at a ADMIN_EMAIL other@notes.test
TF_VAR_returned_admin=other@notes.test cplan chain-drift 2
grep -q 'other@notes.test' "$WORK/chain-drift.log" || { cat "$WORK/chain-drift.log"; fail "the plan does not show the value the kit gave A"; }
capply chain-restored
env_at a ADMIN_EMAIL a@notes.test
cplan chain-settled 0
no_kit "after the chain's re-runs"

step "handoff chain: destroy"
TF_VAR_destroyable=true capply chain-destroyable
ctf destroy -auto-approve -input=false >"$WORK/chain-destroy.log" 2>&1 || { cat "$WORK/chain-destroy.log"; fail "tofu destroy (the chain)"; }
! on_target test -e /home/ops/a/notes || fail "A is left after the destroy"
! on_target test -e /home/ops/b/notes || fail "B is left after the destroy"
[[ $(ctf state list 2>/dev/null) == "" ]] || fail "the chain's state still holds resources"

echo PASS
