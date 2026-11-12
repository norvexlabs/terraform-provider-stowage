#!/usr/bin/env bash
# acc-vm-run.sh — make acc-vm's launcher: STOWAGE_VM_ENV resolved here, on the host (the file may
# source others the runner does not see), and test/acc-vm.sh run in the acceptance runner with
# the variables it set or changed passed by NAME alone (`docker run -e NAME`: docker reads each
# value from this process's environment), never on a command line, in a file or printed. The
# directory of the SSH key it names (DEV_VM_SSH_KEY) is mounted read-only at its own path.
#
#   acc-vm-run.sh <image> docker run <argument>…   the image, then the runner's base command
#   ACC_DRY=1: prints the names the runner would receive, one per line, and runs nothing.
set -euo pipefail
image=$1
shift
[[ -n ${STOWAGE_VM_ENV:-} && -r $STOWAGE_VM_ENV ]] || { echo "FAIL: STOWAGE_VM_ENV=<file> is required (see test/acc-vm.sh)" >&2; exit 1; }

# The environment before and after the file, in memory alone: the names it set or changed.
declare -A before
while IFS= read -r -d '' e; do before[${e%%=*}]=${e#*=}; done < <(env -0)
set -a
# shellcheck disable=SC1090
. "$STOWAGE_VM_ENV" >/dev/null
set +a
names=()
while IFS= read -r -d '' e; do
	n=${e%%=*}
	case $n in PWD | OLDPWD | SHLVL | _ | BASH_* | STOWAGE_VM_ENV) continue ;; esac
	if [[ ! -v before[$n] || ${before[$n]} != "${e#*=}" ]]; then names+=("$n"); fi
done < <(env -0)

key=${DEV_VM_SSH_KEY:-}
[[ -n $key && -r $key ]] || { echo "FAIL: DEV_VM_SSH_KEY, which STOWAGE_VM_ENV sets, is not a readable file" >&2; exit 1; }
keydir=$(cd "$(dirname "$key")" && pwd)

args=("$@")
for n in "${names[@]}"; do args+=(-e "$n"); done
# acc-vm.sh takes the environment as it is: the file is not read again in the runner.
args+=(-e STOWAGE_VM_ENV_RESOLVED=1 -v "$keydir:$keydir:ro" "$image" bash test/acc-vm.sh)

if [[ -n ${ACC_DRY:-} ]]; then
	printf '%s\n' "${names[@]}" | sort
	exit 0
fi
exec "${args[@]}"
