package installer

import (
	"context"
	"fmt"
	pathpkg "path"
	"regexp"
	"strings"
)

// PlanDirScript makes a private directory (0700) in work_dir for a plan's installer and prints its
// absolute path: `.stowage-plan.XXXXXX`, beside the install, never in it; plans' directories
// older than an hour there (ones a plan killed before its cleanup left) are removed first.
// Argument: work_dir.
const PlanDirScript = `set -eu
cd -- "$1"
find . -maxdepth 1 -type d -name '.stowage-plan.??????' -mmin +60 -exec rm -rf -- {} + 2>/dev/null || true
umask 077
mktemp -d "$(pwd -P)/.stowage-plan.XXXXXX"
`

// PlanRemoveScript removes a plan's directory: a `.stowage-plan.XXXXXX` whose parent is the work
// directory (both physical paths read); anything else is refused (exit 2). Arguments: the
// directory, the work directory.
const PlanRemoveScript = `set -eu
case ${1##*/} in
.stowage-plan.??????) ;;
*) echo "stowage-provider: $1 is not a plan's directory" >&2; exit 2 ;;
esac
p=$(cd -- "${1%/*}" && pwd -P) w=$(cd -- "$2" && pwd -P)
[ -n "$p" ] && [ "$p" = "$w" ] || { echo "stowage-provider: $1 is not in $2" >&2; exit 2; }
rm -rf -- "$1"
`

// planDirName is a plan directory's name, as mktemp makes it.
var planDirName = regexp.MustCompile(`^\.stowage-plan\.[A-Za-z0-9]{6}$`)

// PlanDir makes a plan's private directory in d's work directory: the absolute path.
func (d Docker) PlanDir(ctx context.Context) (string, error) {
	stdout, stderr, exit, err := d.Runner.Run(ctx, []string{"sh", "-c", PlanDirScript, "sh", d.WorkDir}, nil)
	if err != nil {
		return "", fmt.Errorf("a plan's directory: %w", err)
	}
	if exit != 0 {
		return "", &FailedError{Verb: "making a plan's directory in " + d.WorkDir, Exit: exit, Stderr: tail(stderr)}
	}
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	dir := strings.TrimSpace(lines[len(lines)-1]) // ssh may print a banner before it
	if !strings.HasPrefix(dir, "/") || pathpkg.Clean(dir) != dir || !planDirName.MatchString(pathpkg.Base(dir)) {
		return "", fmt.Errorf("a plan's directory: %q is not one", dir)
	}
	return dir, nil
}

// RemovePlanDir removes a directory PlanDir made.
func (d Docker) RemovePlanDir(ctx context.Context, dir string) error {
	_, stderr, exit, err := d.Runner.Run(ctx, []string{"sh", "-c", PlanRemoveScript, "sh", dir, d.WorkDir}, nil)
	if err != nil {
		return fmt.Errorf("removing %s: %w", dir, err)
	}
	if exit != 0 {
		return &FailedError{Verb: "removing " + dir, Exit: exit, Stderr: tail(stderr)}
	}
	return nil
}

// DryRunUpgrade runs the bootstrap script a plan fetched into dir (PlanDir's) `upgrade --dry-run
// --no-input --json` on d's install, given c's inputs, secret inputs and ports: what the upgrade
// would refuse, changing nothing. It runs in dir, so what the script extracts stays there; the
// install is named by its absolute path, dir's parent's.
func (d Docker) DryRunUpgrade(ctx context.Context, dir string, c Change) (*Result, error) {
	if !strings.HasPrefix(dir, "/") || !planDirName.MatchString(pathpkg.Base(dir)) {
		return nil, fmt.Errorf("upgrade --dry-run: %q is not a plan's directory", dir)
	}
	in := d
	in.WorkDir = dir
	return in.upgrade(ctx, c, "./"+d.Name+".sh", pathpkg.Join(pathpkg.Dir(dir), d.Name), true)
}

// DryRunRollback runs the kept installer's `rollback --dry-run --json`: what the rollback would
// refuse, changing nothing.
func (d Docker) DryRunRollback(ctx context.Context) (*Result, error) {
	stdout, stderr, exit, err := d.stow(ctx, nil, true, "rollback", "--dry-run", "--json")
	if err != nil {
		return nil, fmt.Errorf("rollback --dry-run: %w", err)
	}
	return Outcome("rollback --dry-run", stdout, stderr, exit)
}
