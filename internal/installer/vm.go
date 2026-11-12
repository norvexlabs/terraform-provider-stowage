package installer

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// VM drives an install on a Kairos VM over SSH: `<Short>-stow` is on the VM's PATH, carried by its
// OS bundle, and runs the release that OS carries. The verbs that change the install run as root
// (`sudo -n`, never asking a password); the reads run as the SSH user, in the admin group.
type VM struct {
	Runner target.Runner
	Short  string
}

// SSHFailure is the exit status ssh gives for its own failure (no connection, a host key
// refused) — or the remote program's own 255.
const SSHFailure = 255

func (v VM) ctl() (string, error) {
	if !ValidShortName(v.Short) {
		return "", fmt.Errorf("vm.short_name %q is not an installer's short name (stowage_vm_seed's short_name)", v.Short)
	}
	return v.Short + "-stow", nil
}

// Describe runs the VM's `<short>-stow describe --json`: the release its OS carries.
func (v VM) Describe(ctx context.Context) (*Describe, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := v.Runner.Run(ctx, []string{ctl, "describe", "--json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("describe: %w", err)
	}
	if exit != 0 {
		return nil, describeFailed("describe", exit, stderr)
	}
	desc, err := ParseDescribe(stdout)
	if errors.Is(err, ErrPreRoutes) {
		return nil, fmt.Errorf("%w; the VM's OS carries such an installer: move the VM on by hand first (`sudo %s upgrade --version V`, a release of Stowage %s or later), then run OpenTofu again", err, ctl, MinStowage)
	}
	return desc, err
}

// CtlScript says whether its argument is a command the VM's shell finds (POSIX `command -v`):
// exit 0 found, 1 not.
const CtlScript = `command -v "$1" >/dev/null || exit 1`

// HasCtl says whether the VM's shell finds <Short>-stow — the OS bundle's /usr/sbin/<short>-stow
// — whatever its language or the message a missing program printed.
func (v VM) HasCtl(ctx context.Context) (bool, error) {
	ctl, err := v.ctl()
	if err != nil {
		return false, err
	}
	_, stderr, exit, err := v.Runner.Run(ctx, []string{"sh", "-c", CtlScript, "sh", ctl}, nil)
	switch {
	case err != nil:
		return false, fmt.Errorf("looking for %s: %w", ctl, err)
	case exit == 0:
		return true, nil
	case exit == 1:
		return false, nil
	}
	return false, &FailedError{Verb: "looking for " + ctl, Exit: exit, Stderr: tail(stderr)}
}

// VMStatus is the VM's `status --json`: the fields of every shape, and the boot's.
type VMStatus struct {
	Status
	// OS is the release the OS carries.
	OS *Release `json:"os"`
	// Boot is the system Kairos booted: active, passive (the fallback) or recovery.
	Boot     string `json:"boot"`
	Fallback bool   `json:"fallback"`
	// OnTrial: the boot runs an upgrade's new system, which the health gate has not judged yet.
	OnTrial bool `json:"on_trial"`
	// FailedBoot is the last boot the health gate found unhealthy.
	FailedBoot    *FailedBoot `json:"failed_boot"`
	RebootPending bool        `json:"reboot_pending"`
	// BootID is the kernel's id of this boot: it changes at every reboot. Empty when the VM
	// cannot read it.
	BootID string `json:"boot_id"`
	// Events is the event journal's last entry number (0 before the first); nil when the journal
	// cannot be read, EventsError then saying why.
	Events      *int64 `json:"events"`
	EventsError string `json:"events_error"`
}

// Event is an entry of the VM's event journal (`events --json`), the fields the provider reads:
// a boot.judged entry is the health gate's verdict on one boot (healthy, went_back or
// kept_unhealthy), its reason, and why a boot found unhealthy keeps running.
type Event struct {
	Seq     int64    `json:"seq"`
	Kind    string   `json:"kind"`
	BootID  string   `json:"boot_id"`
	Release *Release `json:"release"`
	OnTrial *bool    `json:"on_trial"`
	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason"`
	Kept    string   `json:"kept"`
}

// The health gate's verdicts on a boot.
const (
	EvBootJudged         = "boot.judged"
	VerdictHealthy       = "healthy"
	VerdictWentBack      = "went_back"
	VerdictKeptUnhealthy = "kept_unhealthy"
)

// Events runs the VM's `<short>-stow events --after N --json`: the journal's entries after N, one
// JSON object per line.
func (v VM) Events(ctx context.Context, after int64) ([]Event, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	argv := []string{ctl, "events", "--after", fmt.Sprint(after), "--json"}
	stdout, stderr, exit, err := v.Runner.Run(ctx, argv, nil)
	logRun(ctx, argv, stdout, stderr, true, exit, err)
	if err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	if exit != 0 {
		return nil, &FailedError{Verb: "events", Exit: exit, Stderr: tail(stderr)}
	}
	var out []Event
	dec := json.NewDecoder(bytes.NewReader(stdout))
	for {
		var e Event
		if err := dec.Decode(&e); errors.Is(err, io.EOF) {
			return out, nil
		} else if err != nil {
			return nil, fmt.Errorf("events --json: %w", err)
		}
		if e.Kind == "" || e.Seq <= 0 {
			return nil, fmt.Errorf("events --json: an entry without a kind or a seq")
		}
		out = append(out, e)
	}
}

// FailedBoot is a boot the health gate found unhealthy: the release it ran, when, why, and why
// the gate kept it when going back was refused.
type FailedBoot struct {
	Release Release `json:"release"`
	At      string  `json:"at"`
	Reason  string  `json:"reason"`
	Kept    string  `json:"kept"`
}

// ParseVMStatus reads the VM's `status --json`.
func ParseVMStatus(b []byte) (*VMStatus, error) {
	const doc = "status --json"
	var s VMStatus
	if err := decodeOne(doc, b, &s); err != nil {
		return nil, err
	}
	if err := checkFormat(doc, s.Format); err != nil {
		return nil, err
	}
	if s.Name == "" {
		return nil, fmt.Errorf("%s: no name", doc)
	}
	for field, r := range map[string]*Release{"installed": s.Installed, "previous": s.Previous, "os": s.OS} {
		if err := r.check(doc, field); err != nil {
			return nil, err
		}
	}
	if s.FailedBoot != nil {
		if err := s.FailedBoot.Release.check(doc, "failed_boot.release"); err != nil {
			return nil, err
		}
	}
	return &s, nil
}

// Status runs the VM's `<short>-stow status --json`.
func (v VM) Status(ctx context.Context) (*VMStatus, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	argv := []string{ctl, "status", "--json"}
	stdout, stderr, exit, err := v.Runner.Run(ctx, argv, nil)
	logRun(ctx, argv, stdout, stderr, true, exit, err)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	switch exit {
	case 0:
		return ParseVMStatus(stdout)
	case 3: // the VM's status answers before the first install: any refusal is one
		r, err := ParseRefused("status --json", stdout, stderr)
		if err != nil {
			return nil, err
		}
		return nil, r
	}
	return nil, &FailedError{Verb: "status", Exit: exit, Stderr: tail(stderr)}
}

// VMInstall is what the VM's `install` is given: a first install, or its re-run.
type VMInstall struct {
	Change
	Given   string // --given, a file on standard input
	Secrets string // --secrets, a file on standard input
}

// Install runs `sudo -n <short>-stow install --no-input --json` on the VM: its first install, or
// the re-run of the install it holds. The secret inputs (an answers file), the given material
// and a secrets export reach it as files on standard input, in a private directory removed
// afterwards (RunScript); nothing secret is on its command line.
func (v VM) Install(ctx context.Context, in VMInstall) (*Result, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	args, files, err := changeArgs(in.Change, "sudo", "-n", ctl, "install", "--no-input", "--json")
	if err != nil {
		return nil, err
	}
	for name, val := range map[string]string{"given": in.Given, "secrets": in.Secrets} {
		if val != "" {
			files[name] = val
		}
	}
	names, stdin, err := filesInput(files)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := v.Runner.Run(ctx, append([]string{"sh", "-c", RunScript, "sh", ".", names}, args...), stdin)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	return Outcome("install", stdout, stderr, exit)
}

// ErrCut is a verb whose SSH connection was lost before it printed its result (exit 255 and no
// result object): an upgrade or a rollback may have scheduled its reboot, which status tells.
var ErrCut = errors.New("the connection ended before the result")

// Upgrade runs `sudo -n <short>-stow upgrade --version version --yes --json`: the job ends as it
// schedules the reboot, so the result's release is the one installed before it.
func (v VM) Upgrade(ctx context.Context, version string) (*Result, error) {
	if version == "" || strings.HasPrefix(version, "-") || strings.ContainsAny(version, " \t\r\n") {
		return nil, fmt.Errorf("vm.version %q is not a version", version)
	}
	return v.rebooting(ctx, "upgrade", "upgrade", "--version", version, "--yes", "--json")
}

// Rollback runs `sudo -n <short>-stow rollback --yes --json`: the reboot into the passive
// system, back to the release the last upgrade replaced.
func (v VM) Rollback(ctx context.Context) (*Result, error) {
	return v.rebooting(ctx, "rollback", "rollback", "--yes", "--json")
}

func (v VM) rebooting(ctx context.Context, verb string, args ...string) (*Result, error) {
	ctl, err := v.ctl()
	if err != nil {
		return nil, err
	}
	argv := append([]string{"sudo", "-n", ctl}, args...)
	stdout, stderr, exit, err := v.Runner.Run(ctx, argv, nil)
	logRun(ctx, argv, stdout, stderr, false, exit, err)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", verb, err)
	}
	if exit == SSHFailure && len(strings.TrimSpace(string(stdout))) == 0 {
		return nil, fmt.Errorf("%s: %w (%s)", verb, ErrCut, tail(stderr))
	}
	return Outcome(verb, stdout, stderr, exit)
}

// logRun logs, at debug level, a command of the machine interface and its standard output (its
// first 16 KiB): the status (input keys alone, never a value), the event journal's entries (the
// daemon's and the gate's records: kinds, releases, verdicts and their reasons), or the upgrade's
// or the rollback's JSON result. withStderr is set for the reads, whose standard error is the
// daemon's own error; a verb's standard error is its run's free-text log, of which the size alone
// is logged. TF_LOG_PROVIDER_STOWAGE=DEBUG shows the entries.
func logRun(ctx context.Context, argv []string, stdout, stderr []byte, withStderr bool, exit int, err error) {
	fields := map[string]any{"argv": strings.Join(argv, " "), "exit": exit, "stdout": head(stdout)}
	if withStderr {
		fields["stderr"] = tail(stderr)
	} else {
		fields["stderr_bytes"] = len(stderr)
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	tflog.Debug(ctx, "ran on the VM", fields)
}

// head is b's first 16 KiB, cut on a character: a JSON document's leading fields are the ones read.
func head(b []byte) string {
	const max = 16 << 10
	if len(b) <= max {
		return string(b)
	}
	return strings.ToValidUTF8(string(b[:max]), "") + "…"
}

// HostKey reads the VM's own ed25519 host key, `ssh-ed25519 <base64>`, through the connection,
// whose pinned key vouches for it: the key the VM serves alone once its one-time key retired.
func (v VM) HostKey(ctx context.Context) (string, error) {
	stdout, stderr, exit, err := v.Runner.Run(ctx, []string{"cat", "/etc/ssh/ssh_host_ed25519_key.pub"}, nil)
	if err != nil {
		return "", fmt.Errorf("reading the VM's host key: %w", err)
	}
	if exit != 0 {
		return "", &FailedError{Verb: "reading the VM's host key", Exit: exit, Stderr: tail(stderr)}
	}
	f := strings.Fields(string(stdout))
	if len(f) < 2 || f[0] != "ssh-ed25519" {
		return "", fmt.Errorf("reading the VM's host key: no ed25519 key in %q", tail(stdout))
	}
	line := f[0] + " " + f[1]
	if _, err := target.ParseKnownHostsLine(line); err != nil {
		return "", fmt.Errorf("reading the VM's host key: %w", err)
	}
	return line, nil
}

// Boot runs `sudo -n systemctl start <name>.service`: the VM's boot unit (`<short>-stow boot`),
// which returns once that boot did its part — loaded the release's images, made the first install
// from the cloud-config's settings or found it waiting, or wrote another release's OS and
// rebooted (the connection then ends, exit 255). It is what `vm create` runs before it gives an
// install over SSH, so the two never run at once (the install's lock refuses the second).
func (v VM) Boot(ctx context.Context, name string) error {
	if !ValidName(name) {
		return fmt.Errorf("%q is not a project's name", name)
	}
	_, stderr, exit, err := v.Runner.Run(ctx, []string{"sudo", "-n", "systemctl", "start", name + ".service"}, nil)
	if err != nil {
		return fmt.Errorf("the VM's boot unit: %w", err)
	}
	if exit != 0 {
		return &FailedError{Verb: "starting the VM's boot unit " + name + ".service", Exit: exit, Stderr: tail(stderr)}
	}
	return nil
}

// hostKeyResult is the VM's `<short>-stow host-key --json`: its own SSH host key, and whether the
// seed's one-time key is retired (its files gone, sshd's reload taken), the reason when not.
type hostKeyResult struct {
	Format    *int   `json:"format"`
	PublicKey string `json:"public_key"`
	Retired   *bool  `json:"retired"`
	Reason    string `json:"reason"`
}

// NotRetiredError is a host-key run that read the VM's own key but whose retirement did not take
// (sshd did not reload): the VM may serve the one-time key until it does. Reason and Stderr are
// the installer's, its fix among them.
type NotRetiredError struct{ Reason, Stderr string }

func (e *NotRetiredError) Error() string {
	msg := "host-key read the VM's own key, but the one-time key is not retired: " + e.Reason
	if e.Stderr != "" {
		msg += "\n" + e.Stderr
	}
	return msg
}

// RetireHostKey runs `sudo -n <short>-stow host-key --json` on the VM: the VM's own ed25519 host
// key, "ssh-ed25519 <base64>" (`public_key`, whatever the VM's host name), and the seed's one-time
// key retired by Stowage's own steps (run again, it changes nothing more). A run whose reload did
// not take (exit 1, `"retired": false`) gives the key with a *NotRetiredError; a refusal (exit 3:
// a recovery or live boot, not run as root, the first install not done) is a *RefusedError, no
// key; any other failure, no key.
func (v VM) RetireHostKey(ctx context.Context) (string, error) {
	ctl, err := v.ctl()
	if err != nil {
		return "", err
	}
	stdout, stderr, exit, err := v.Runner.Run(ctx, []string{"sudo", "-n", ctl, "host-key", "--json"}, nil)
	if err != nil {
		return "", fmt.Errorf("host-key: %w", err)
	}
	const doc = "host-key --json"
	out := bytes.TrimSpace(stdout)
	failed := func(cause error) error {
		return &FailedError{Verb: "host-key", Exit: exit, Stderr: tail(stderr), Cause: cause}
	}
	switch {
	case exit == 3:
		r, err := ParseRefused(doc, stdout, stderr)
		if err != nil {
			return "", err
		}
		return "", r
	case exit == 1 && len(out) == 0 && bytes.Contains(stderr, []byte("unknown command")):
		return "", failed(fmt.Errorf("the VM's %s has no host-key verb: its OS carries an installer built before Stowage %s's; the reboot of the next release change retires the one-time key: set vm.version to a release built by Stowage %s or later and apply", ctl, MinStowage, MinStowage))
	case exit != 0 && exit != 1, len(out) == 0:
		return "", failed(nil)
	}
	var h hostKeyResult
	if err := decodeOne(doc, out, &h); err != nil {
		return "", failed(err)
	}
	if err := checkFormat(doc, h.Format); err != nil {
		return "", err
	}
	if h.Retired == nil {
		return "", failed(fmt.Errorf("%s: no retired: the installer breaks the machine interface", doc))
	}
	f := strings.Fields(h.PublicKey)
	if len(f) != 2 || f[0] != "ssh-ed25519" {
		return "", failed(fmt.Errorf("%s: public_key %q is not an ed25519 public key (`ssh-ed25519 <base64>`)", doc, h.PublicKey))
	}
	if _, err := target.ParseKnownHostsLine(h.PublicKey); err != nil {
		return "", failed(fmt.Errorf("%s: public_key: %w", doc, err))
	}
	key := f[0] + " " + f[1]
	switch {
	case exit == 0 && *h.Retired:
		return key, nil
	case exit == 1 && !*h.Retired:
		return key, &NotRetiredError{Reason: cmp.Or(h.Reason, "sshd did not reload"), Stderr: tail(stderr)}
	}
	return "", failed(fmt.Errorf("%s: exit %d with retired %v: the installer breaks the machine interface", doc, exit, *h.Retired))
}
