package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SSHAllowMinStowage is the first Stowage release whose VM changes SSH's sources after its first
// boot (`<short>-stow ssh-allow`).
const SSHAllowMinStowage = "0.50.0"

// ErrNoSSHAllow is a VM's `<short>-stow` without the ssh-allow verb: one of an OS built with
// Stowage before SSHAllowMinStowage.
var ErrNoSSHAllow = fmt.Errorf("the VM's installer has no ssh-allow verb (an OS built with Stowage before %s): set version to a release built by Stowage %s or later and apply, then ssh_allow", SSHAllowMinStowage, SSHAllowMinStowage)

// ErrSSHPending is a `ssh-allow set` refused because a change of SSH's sources awaits
// confirmation already.
var ErrSSHPending = errors.New("a change of SSH's sources awaits confirmation on the VM")

// SSHAllow is the VM's answer on SSH's sources (`ssh-allow --json`, and each of its verbs'):
// Sources what the rule file accepts, each a masked CIDR (nil when the file does not read as SSH's
// sources, Rules then saying why); From what set them ("set", "cloud-config" or "image"); Pending
// a change awaiting confirmation, whose sources Sources are.
type SSHAllow struct {
	Message string      `json:"message"`
	Sources []string    `json:"sources"`
	From    string      `json:"from"`
	Rules   string      `json:"rules"`
	Pending *SSHPending `json:"pending"`
}

// SSHPending is a change of SSH's sources awaiting confirmation: the sources its revert brings
// back, and when it does.
type SSHPending struct {
	Previous []string `json:"previous"`
	Deadline string   `json:"deadline"`
	SecsLeft int      `json:"secs_left"`
}

// SSHAllow runs `sudo -n <short>-stow ssh-allow --json`: the sources SSH accepts on the VM.
func (v VM) SSHAllow(ctx context.Context) (*SSHAllow, error) {
	return v.sshAllow(ctx, "ssh-allow", "ssh-allow", "--json")
}

// SSHAllowSet runs `sudo -n <short>-stow ssh-allow set CIDR… --json`, sources sorted: the VM
// applies them at once, and undoes them unless SSHAllowConfirm, run over a new connection, keeps
// them within the default window (the answer's pending.secs_left). A change already awaiting
// confirmation is ErrSSHPending.
func (v VM) SSHAllowSet(ctx context.Context, sources []string) (*SSHAllow, error) {
	if len(sources) == 0 {
		return nil, errors.New("ssh-allow set: no source")
	}
	for _, s := range sources {
		if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n,") {
			return nil, fmt.Errorf("ssh-allow set: %q is not one source", s)
		}
	}
	sorted := slices.Sorted(slices.Values(sources))
	return v.sshAllow(ctx, "ssh-allow set", append(append([]string{"ssh-allow", "set"}, sorted...), "--json")...)
}

// SSHAllowConfirm runs `sudo -n <short>-stow ssh-allow confirm --json`: the change awaiting
// confirmation kept. The runner opens a new SSH connection for it, which the VM lets in only when
// the new sources hold the address it comes from.
func (v VM) SSHAllowConfirm(ctx context.Context) (*SSHAllow, error) {
	return v.sshAllow(ctx, "ssh-allow confirm", "ssh-allow", "confirm", "--json")
}

// SSHAllowRevert runs `sudo -n <short>-stow ssh-allow revert --json`: the change awaiting
// confirmation undone now.
func (v VM) SSHAllowRevert(ctx context.Context) (*SSHAllow, error) {
	return v.sshAllow(ctx, "ssh-allow revert", "ssh-allow", "revert", "--json")
}

// sshAllow runs `sudo -n <short>-stow <args…>` and reads its answer: exit 0, the object; an
// installer without the verb (cobra's unknown command), ErrNoSSHAllow; a change awaiting
// confirmation named by a set's failure, ErrSSHPending; exit 3, a *RefusedError (its reason the
// last line of standard error); any other exit, ssh's own 255 included, a *FailedError.
func (v VM) sshAllow(ctx context.Context, verb string, args ...string) (*SSHAllow, error) {
	stdout, stderr, exit, err := v.verb(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", verb, err)
	}
	switch {
	case exit == 0:
	case exit != 3 && exit != SSHFailure && bytes.Contains(stderr, []byte(`unknown command "ssh-allow"`)):
		return nil, fmt.Errorf("%s: %w", verb, ErrNoSSHAllow)
	case verb == "ssh-allow set" && exit != SSHFailure && awaitsConfirmation(stderr):
		return nil, fmt.Errorf("%s: %w: %s", verb, ErrSSHPending, lastLine(stderr))
	case exit == 3:
		return nil, &RefusedError{Verb: verb, Refusal: Refusal{Reason: lastLine(stderr)}}
	default:
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr)}
	}
	const doc = "ssh-allow --json"
	var a SSHAllow
	if err := decodeOne(doc, stdout, &a); err != nil {
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: err}
	}
	if a.From == "" {
		return nil, &FailedError{Verb: verb, Exit: exit, Stderr: tail(stderr), Cause: fmt.Errorf("%s: no from: the installer breaks the machine interface", doc)}
	}
	return &a, nil
}

// awaitsConfirmation says whether stderr is the VM's refusal of a change while another awaits
// confirmation ("a change of SSH's sources awaits confirmation: …"), never its "no change … awaits
// confirmation".
func awaitsConfirmation(stderr []byte) bool {
	l := lastLine(stderr)
	return strings.Contains(l, "awaits confirmation") && !strings.Contains(l, "no change")
}
