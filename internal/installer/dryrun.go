package installer

import (
	"context"
	"fmt"
	"strings"
)

// PlanDirScript makes a private directory (0700) in work_dir for a plan's installer and prints its
// absolute path: `.stowage-plan.XXXXXX`, beside the install, never in it. Argument: work_dir.
const PlanDirScript = `set -eu
cd -- "$1"
umask 077
mktemp -d "$(pwd -P)/.stowage-plan.XXXXXX"
`

// PlanRemoveScript removes a plan's directory, and refuses (exit 2) any other path. Argument:
// the directory.
const PlanRemoveScript = `case $1 in
/*/.stowage-plan.*) rm -rf -- "$1" ;;
*) echo "stowage-provider: $1 is not a plan's directory" >&2; exit 2 ;;
esac
`

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
	if !strings.HasPrefix(dir, "/") || !strings.Contains(dir, "/.stowage-plan.") {
		return "", fmt.Errorf("a plan's directory: %q is not one", dir)
	}
	return dir, nil
}

// RemovePlanDir removes a directory PlanDir made.
func (d Docker) RemovePlanDir(ctx context.Context, dir string) error {
	_, stderr, exit, err := d.Runner.Run(ctx, []string{"sh", "-c", PlanRemoveScript, "sh", dir}, nil)
	if err != nil {
		return fmt.Errorf("removing %s: %w", dir, err)
	}
	if exit != 0 {
		return &FailedError{Verb: "removing " + dir, Exit: exit, Stderr: tail(stderr)}
	}
	return nil
}

// DryRunUpgrade runs the bootstrap script at script (an absolute path, a plan's directory's)
// `upgrade --dry-run --no-input --json` on d's install, given c's inputs, secret inputs and ports:
// what the upgrade would refuse, changing nothing.
func (d Docker) DryRunUpgrade(ctx context.Context, script string, c Change) (*Result, error) {
	if !strings.HasPrefix(script, "/") {
		return nil, fmt.Errorf("upgrade --dry-run: %q is not an absolute path", script)
	}
	added := Change{Inputs: c.Inputs, SecretInputs: c.SecretInputs, Ports: c.Ports}
	args, files, err := changeArgs(added, "sh", script, "upgrade", "--dry-run", "--no-input", "--json", "--dir", "./"+d.Name)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.run(ctx, files, args...)
	if err != nil {
		return nil, fmt.Errorf("upgrade --dry-run: %w", err)
	}
	return Outcome("upgrade --dry-run", stdout, stderr, exit)
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
