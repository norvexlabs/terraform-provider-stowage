// Package target runs commands where a Stowage install lives: on this machine, or on a host
// reached by the system ssh with its host key pinned.
package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Runner runs one program on a target. argv[0] is the program; stdin is its whole standard
// input (nil: empty). exit is the program's exit status when it ran to an exit; err is set
// when it could not be started or was cut by the context, and exit is then -1.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

// MaxOutput bounds what a run keeps of each of stdout and stderr; the rest is dropped and the
// kept part ends with a note, so a runaway program cannot exhaust the provider's memory.
const MaxOutput = 16 << 20

// Local runs argv on the machine that runs OpenTofu, in its environment without the variables
// Unset names.
type Local struct {
	Unset []string
}

// Run implements Runner.
func (l Local) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("local: no program to run")
	}
	cmd := exec.CommandContext(ctx, "sh", append([]string{"-c", OutputScript, "sh"}, argv...)...)
	// The wrapper and the program in a process group of their own, killed together by the
	// context: killing the wrapper alone would leave the program running.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	if len(l.Unset) > 0 {
		cmd.Env = without(os.Environ(), l.Unset)
	}
	return run(ctx, cmd, stdin)
}

// OutputScript runs its arguments as a program whose standard output goes to a private file
// (0600, removed afterwards) printed once the program exited, with its exit status: a process the
// program started that outlives it and keeps its standard output (a daemon, a container's
// attach) cannot add to the output a reader takes as one document. Its standard input and error
// are the program's own.
const OutputScript = `__stowage_o=$(umask 077 && mktemp) || exit 125
trap 'rm -f "$__stowage_o"' EXIT
trap 'exit 130' HUP INT TERM
__stowage_rc=0
"$@" >"$__stowage_o" || __stowage_rc=$?
cat "$__stowage_o"
exit "$__stowage_rc"
`

// EnvFilter is a Runner on the machine running OpenTofu that can run without some of its
// environment's variables: WithoutEnv is that Runner, leaving the keys named out.
type EnvFilter interface {
	Runner
	WithoutEnv(keys []string) Runner
}

// WithoutEnv implements EnvFilter.
func (l Local) WithoutEnv(keys []string) Runner {
	return Local{Unset: append(slices.Clone(l.Unset), keys...)}
}

// without is env (KEY=VALUE entries) less the keys named.
func without(env, keys []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if !slices.Contains(keys, k) {
			out = append(out, e)
		}
	}
	return out
}

// waitDelay is how long a run waits for its output to close once the program exited or was
// killed by its context: a program it started that keeps the output open (a container, a shared
// ssh connection) cannot hold the run past it. A program that exited keeps its exit status.
var waitDelay = 10 * time.Second

func run(ctx context.Context, cmd *exec.Cmd, stdin []byte) ([]byte, []byte, int, error) {
	cmd.WaitDelay = waitDelay
	var stdout, stderr capped
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	// The program exited 0 (another exit is an ExitError); a program it started kept the output
	// open past waitDelay. It ran to its exit, whatever the context did meanwhile.
	if errors.Is(err, exec.ErrWaitDelay) {
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
	if ctx.Err() != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, fmt.Errorf("%s: %w", cmd.Args[0], ctx.Err())
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return stdout.Bytes(), stderr.Bytes(), exit.ExitCode(), nil
	}
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, fmt.Errorf("%s: %w", cmd.Args[0], err)
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

// capped is a buffer keeping at most MaxOutput bytes.
type capped struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	if room := MaxOutput - c.buf.Len(); room < len(p) {
		c.truncated = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) Bytes() []byte {
	if c.truncated {
		return append(c.buf.Bytes(), "\n[output truncated]\n"...)
	}
	return c.buf.Bytes()
}

// ShellQuote quotes s for a POSIX shell: one word, nothing expanded.
func ShellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:@,+%") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShellJoin quotes each word of argv and joins them: the command line a remote shell splits
// back into argv exactly.
func ShellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = ShellQuote(a)
	}
	return strings.Join(q, " ")
}
