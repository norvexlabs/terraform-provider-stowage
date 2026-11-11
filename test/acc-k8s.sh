#!/usr/bin/env bash
# Acceptance test of the Kubernetes target, through OpenTofu: the provider binary (PROVIDER_BIN,
# default ./bin/terraform-provider-stowage, which `make acc-k8s` builds) loaded by tofu's
# dev_overrides, a real Stowage (STOWAGE_REPO, default ~/stowage: its bin/stowage and its test
# harness, test/lib.sh — zot, a kind cluster pulling from it, pinned kind/helm/kubectl), and
# examples/minimal shipped twice.
#
#   make acc-k8s     (or: PROVIDER_BIN=… bash test/acc-k8s.sh)
#
# Walks: create (1.0.0, the cluster reached by host and client certificate, a kubeconfig written
# for each run) → a plan shows no change → a changed input (k8s upgrade --given, the pod's
# environment changed) → ship 2.0.0, its installer upgrades → a plan shows no change → back to
# 1.0.0's installer with allow_rollback (k8s rollback) → destroy (k8s uninstall --data). Needs
# tofu, Docker, network access, a linux/amd64 host.
set -euo pipefail

PROV=$(cd "$(dirname "$0")/.." && pwd)
PROVIDER_BIN=$(realpath "${PROVIDER_BIN:-$PROV/bin/terraform-provider-stowage}")
STOWAGE_REPO=$(realpath "${STOWAGE_REPO:-$HOME/stowage}")
[[ -x $PROVIDER_BIN ]] || { echo "FAIL: no provider binary at $PROVIDER_BIN (make build)" >&2; exit 1; }
[[ -x $STOWAGE_REPO/bin/stowage ]] || { echo "FAIL: no $STOWAGE_REPO/bin/stowage (make build there)" >&2; exit 1; }
TOFU=$(command -v tofu) || { echo "FAIL: no tofu on PATH" >&2; exit 1; }

# The amd64 image of the example's postgres, as Stowage's suites pin it.
PG=postgres:18-alpine@sha256:123b17998ccaf47e53b502e8c6a1e79afd8f579c6f3eec83c563da026f957395
. "$STOWAGE_REPO/test/lib.sh"

k8s_tools
NS=notes
# This suite's cluster and network are named acc-k8s-<pid>: the harness's start_kind, renamed.
eval "$(declare -f start_kind | sed 's/stowage-e2e-\$\$/acc-k8s-$$/g')"

# release <version>: bump the example, with a web page of its own (a new image), ship it on stable.
release() {
	printf '%s\n' "$1" >"$P/VERSION"
	printf '### Changed\n- Release %s.\n' "$1" >"$P/changes/$1.md"
	printf '<!doctype html><title>notes %s</title>\n' "$1" >"$P/web/index.html"
	"${S[@]}" ship --channel stable 2>"$WORK/ship-$1.log" || { cat "$WORK/ship-$1.log"; fail "ship $1"; }
}
# runs <version>: helm lists the release at that app version, deployed, its web pods ready.
runs() {
	local got
	kubectl rollout status -n "$NS" deploy/notes-web --timeout 180s >/dev/null || { cluster_logs >&2; fail "notes-web is not ready"; }
	got=$(helm list -n "$NS" --filter '^notes$' --output json | jq -r '.[0] | .app_version + " " + .status')
	[[ $got == "$1 deployed" ]] || fail "helm lists '$got', want '$1 deployed'"
}
# st <attribute>: the attribute of stowage_install.notes in tofu's state.
st() { (cd "$TF" && "${TF_ENV[@]}" "$TOFU" show -json) | jq -r --arg a "$1" '.values.root_module.resources[] | select(.address == "stowage_install.notes") | .values[$a] | if type == "object" then tojson else . end'; }
# config <installer> <admin email> [extra lines]: main.tf for that release and input.
config() {
	cat >"$TF/main.tf" <<HCL
terraform {
  required_providers {
    stowage = { source = "norvexlabs/stowage" }
  }
}
provider "stowage" {
  username = "pull"
  password = "$PULL_TOKEN"
  kubernetes {
    host                   = "$K8S_HOST"
    cluster_ca_certificate = file("$WORK/k8s-ca.pem")
    client_certificate     = file("$WORK/k8s-cert.pem")
    client_key             = file("$WORK/k8s-key.pem")
  }
}
resource "stowage_install" "notes" {
  installer           = "$1"
  inputs              = { ADMIN_EMAIL = "$2" }
  deletion_protection = false
  delete_data         = true
  ${3:-}
  kubernetes {
    namespace = "$NS"
  }
}
HCL
}
# tf <args…>: tofu in the root, as an operator: no credential of the test's in its environment.
tf() { (cd "$TF" && "${TF_ENV[@]}" "$TOFU" "$@"); }

step "start zot, a kind cluster pulling from it"
start_zot
start_kind

step "project, ship 1.0.0"
new_project "$PG"
"${S[@]}" ship --channel stable 2>"$WORK/ship-1.0.0.log" || { cat "$WORK/ship-1.0.0.log"; fail "ship 1.0.0"; }
I1=$REG/notes/notes-installer@$(digest_of notes/notes-installer:latest)
[[ $I1 == *@sha256:* ]] || fail "no digest of the 1.0.0 installer"

step "an OpenTofu root: the provider by dev_overrides, the cluster by host and client certificate in its kubernetes block"
TF=$WORK/tf
mkdir -p "$TF" "$WORK/tfhome" "$WORK/tmp"
cat >"$WORK/tofurc" <<HCL
provider_installation {
  dev_overrides { "norvexlabs/stowage" = "$(dirname "$PROVIDER_BIN")" }
  direct {}
}
HCL
kubectl config view --raw --minify -o json >"$WORK/kc.json"
K8S_HOST=$(jq -r '.clusters[0].cluster.server' "$WORK/kc.json")
(umask 077
	jq -r '.clusters[0].cluster."certificate-authority-data"' "$WORK/kc.json" | base64 -d >"$WORK/k8s-ca.pem"
	jq -r '.users[0].user."client-certificate-data"' "$WORK/kc.json" | base64 -d >"$WORK/k8s-cert.pem"
	jq -r '.users[0].user."client-key-data"' "$WORK/kc.json" | base64 -d >"$WORK/k8s-key.pem")
# No KUBECONFIG: the provider writes the kubeconfig each run reads, from the block.
# TMPDIR: each run's private directory (its kubeconfig) is made there, and must be gone after it.
TF_ENV=(env -i "PATH=$PATH" "HOME=$WORK/tfhome" "TMPDIR=$WORK/tmp" "TF_CLI_CONFIG_FILE=$WORK/tofurc" "TF_IN_AUTOMATION=1")
for v in DOCKER_HOST DOCKER_CONTEXT; do
	if [[ -n ${!v:-} ]]; then TF_ENV+=("$v=${!v}"); fi
done

step "create: 1.0.0 installed by its installer's k8s install"
config "$I1" admin@notes.test
tf apply -auto-approve -no-color >"$WORK/apply1.log" 2>&1 || { cat "$WORK/apply1.log"; cluster_logs >&2; fail "tofu apply (create)"; }
runs 1.0.0
[[ $(st status) == installed && $(st version) == 1.0.0 && $(st installer) == "$I1" && $(st id) == notes/notes ]] ||
	fail "state after the create: status $(st status), version $(st version), id $(st id)"
[[ $(kubectl exec -n "$NS" deploy/notes-web -- printenv ADMIN_EMAIL) == admin@notes.test ]] || fail "the pod's ADMIN_EMAIL is not the input"
! grep -qF -- "$(sed -n 2p "$WORK/k8s-key.pem")" "$TF/terraform.tfstate" || fail "the cluster's client key is in state"
[[ -z $(ls -A "$WORK/tmp") ]] || fail "a run's private directory (its kubeconfig) is left: $(ls -A "$WORK/tmp")"

step "a plan after the create shows no change"
rc=0; tf plan -detailed-exitcode -no-color >"$WORK/plan1.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { cat "$WORK/plan1.log"; fail "tofu plan after the create exited $rc, not 0 (no change)"; }

step "a changed input: the running release applied again, given it (k8s upgrade --given)"
config "$I1" ops@notes.test
tf apply -auto-approve -no-color >"$WORK/apply2.log" 2>&1 || { cat "$WORK/apply2.log"; cluster_logs >&2; fail "tofu apply (input)"; }
runs 1.0.0
kubectl rollout status -n "$NS" deploy/notes-web --timeout 180s >/dev/null
[[ $(kubectl exec -n "$NS" deploy/notes-web -- printenv ADMIN_EMAIL) == ops@notes.test ]] || fail "the pod's ADMIN_EMAIL did not change"
[[ $(kubectl get -n "$NS" secret notes-stowage -o jsonpath='{.data.ADMIN_EMAIL}' | base64 -d) == ops@notes.test ]] || fail "the release's Secret holds another ADMIN_EMAIL"

step "ship 2.0.0; its installer upgrades the release"
release 2.0.0
I2=$REG/notes/notes-installer@$(digest_of notes/notes-installer:latest)
[[ $I2 != "$I1" ]] || fail "2.0.0's installer has 1.0.0's digest"
config "$I2" ops@notes.test
tf apply -auto-approve -no-color >"$WORK/apply3.log" 2>&1 || { cat "$WORK/apply3.log"; cluster_logs >&2; fail "tofu apply (upgrade)"; }
runs 2.0.0
[[ $(st version) == 2.0.0 && $(st installer) == "$I2" && $(st previous | jq -r .installer) == "$I1" ]] ||
	fail "state after the upgrade: version $(st version), previous $(st previous)"
rc=0; tf plan -detailed-exitcode -no-color >"$WORK/plan2.log" 2>&1 || rc=$?
[[ $rc == 0 ]] || { cat "$WORK/plan2.log"; fail "tofu plan after the upgrade exited $rc, not 0 (no change)"; }

step "back to 1.0.0's installer: refused without allow_rollback, then k8s rollback"
config "$I1" ops@notes.test
rc=0; tf plan -no-color >"$WORK/plan3.log" 2>&1 || rc=$?
[[ $rc != 0 ]] && grep -q 'a rollback needs allow_rollback' "$WORK/plan3.log" || { cat "$WORK/plan3.log"; fail "the plan back to 1.0.0 without allow_rollback was not refused"; }
config "$I1" ops@notes.test "allow_rollback = true"
tf apply -auto-approve -no-color >"$WORK/apply4.log" 2>&1 || { cat "$WORK/apply4.log"; cluster_logs >&2; fail "tofu apply (rollback)"; }
runs 1.0.0
[[ $(st version) == 1.0.0 && $(st installer) == "$I1" ]] || fail "state after the rollback: version $(st version), installer $(st installer)"

step "destroy: k8s uninstall --data"
tf destroy -auto-approve -no-color >"$WORK/destroy.log" 2>&1 || { cat "$WORK/destroy.log"; cluster_logs >&2; fail "tofu destroy"; }
! helm status notes -n "$NS" >/dev/null 2>&1 || { helm status notes -n "$NS"; fail "the release is still there"; }
! kubectl get -n "$NS" secret notes-stowage >/dev/null 2>&1 || fail "the release's Secret is still there"
[[ $(kubectl get pvc -n "$NS" -o json | jq '.items | length') == 0 ]] || fail "the release's claims are still there (delete_data)"

echo PASS
