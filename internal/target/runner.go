// Package target runs commands where a Stowage install lives: on this machine, or on a host
// reached by the system ssh with its host key pinned.
package target

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// Local runs argv on the machine that runs OpenTofu.
type Local struct{}

// Run implements Runner.
func (Local) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("local: no program to run")
	}
	return run(ctx, exec.CommandContext(ctx, argv[0], argv[1:]...), stdin)
}

func run(ctx context.Context, cmd *exec.Cmd, stdin []byte) ([]byte, []byte, int, error) {
	var stdout, stderr capped
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
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
