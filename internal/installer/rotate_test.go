package installer

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// passRecorder records each run's argv and answers the i-th run with exits[i] (0 past the end).
type passRecorder struct {
	runs   [][]string
	exits  []int
	stderr string
}

func (r *passRecorder) Run(_ context.Context, argv []string, _ []byte) ([]byte, []byte, int, error) {
	r.runs = append(r.runs, argv)
	exit := 0
	if i := len(r.runs) - 1; i < len(r.exits) {
		exit = r.exits[i]
	}
	if exit == 0 {
		return nil, nil, 0, nil
	}
	return nil, []byte(r.stderr), exit, nil
}

func TestDescribeRotateWays(t *testing.T) {
	t.Parallel()
	const head = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.0.0","revision":1},
"shapes":["docker","kubernetes"],"inputs":[],"tls_inputs":[],"first_install_only":[],"routes":[],"l4_routes":[]`
	d, err := ParseDescribe([]byte(head + `,"secrets":[
{"key":"SESSION_SECRET","rotate":{"docker":"replace","kubernetes":"none"},"settable":{"docker":false,"kubernetes":false},"as_file":false},
{"key":"INTERNAL","rotate":{"docker":"passes","kubernetes":"passes"},"settable":{"docker":false,"kubernetes":false},"as_file":false},
{"key":"OTHER","rotate":{"docker":"none"},"settable":{},"as_file":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.RotateWays(ShapeDocker), map[string]string{"SESSION_SECRET": RotateReplace, "INTERNAL": RotatePasses, "OTHER": RotateNone}; !maps.Equal(got, want) {
		t.Errorf("docker: %v", got)
	}
	// A shape the entry names no way for: "".
	if got, want := d.RotateWays(ShapeKubernetes), map[string]string{"SESSION_SECRET": RotateNone, "INTERNAL": RotatePasses, "OTHER": ""}; !maps.Equal(got, want) {
		t.Errorf("kubernetes: %v", got)
	}
	// Before Stowage 0.46.0: no secrets, no ways (nil, not empty); none declared: empty.
	old, err := ParseDescribe([]byte(head + `}`))
	if err != nil || old.RotateWays(ShapeDocker) != nil {
		t.Errorf("old: %v %v", err, old.RotateWays(ShapeDocker))
	}
	none, err := ParseDescribe([]byte(head + `,"secrets":[]}`))
	if err != nil || none.RotateWays(ShapeDocker) == nil {
		t.Errorf("none: %v %v", err, none.RotateWays(ShapeDocker))
	}
	if _, err := ParseDescribe([]byte(head + `,"secrets":[{"key":"K","rotate":"replace"}]}`)); err == nil {
		t.Error("a rotate of the wrong type was read")
	}
}

// Each target runs `secrets rotate KEY` with each pass's flags, in order: the one pass confirmed
// without a terminal (replace), the three passes (passes).
func TestRotateSecretArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tail := func(runs [][]string, from int) [][]string {
		var out [][]string
		for _, r := range runs {
			out = append(out, r[from:])
		}
		return out
	}
	for _, c := range []struct {
		way  string
		want [][]string
	}{
		{RotateReplace, [][]string{{"secrets", "rotate", "K", "--yes", "--no-input"}}},
		{RotatePasses, [][]string{{"secrets", "rotate", "K", "--stage"}, {"secrets", "rotate", "K"}, {"secrets", "rotate", "K", "--finish"}}},
	} {
		r := &passRecorder{}
		if err := (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).RotateSecret(ctx, "K", c.way, 0); err != nil {
			t.Fatal(err)
		}
		var want [][]string
		for _, w := range c.want {
			want = append(want, append([]string{"/srv", "notes", "notes", "", "dir"}, w...))
		}
		if got := tail(r.runs, 4); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("docker %s: %q", c.way, got)
		}
		r = &passRecorder{}
		if err := (VM{Runner: r, Short: "notes"}).RotateSecret(ctx, "K", c.way, 0); err != nil {
			t.Fatal(err)
		}
		want = nil
		for _, w := range c.want {
			want = append(want, append([]string{"sudo", "-n", "notes-stow"}, w...))
		}
		if got := tail(r.runs, 6); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("vm %s: %q", c.way, got)
		}
		r = &passRecorder{}
		if err := (K8s{Runner: r, Dir: "/op", Name: "notes", Namespace: "ns", Release: "rel"}).RotateSecret(ctx, "K", c.way, 0); err != nil {
			t.Fatal(err)
		}
		want = nil
		for _, w := range c.want {
			want = append(want, append(append([]string{"sh", "./notes.sh", "k8s"}, w...), "--namespace", "ns", "--release", "rel"))
		}
		if got := tail(r.runs, 9); !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("k8s %s: %q", c.way, got)
		}
	}
	// A way this provider does not run runs nothing.
	r := &passRecorder{}
	if err := (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).RotateSecret(ctx, "K", RotateNone, 0); err == nil || len(r.runs) != 0 {
		t.Errorf("none: %v %q", err, r.runs)
	}
}

// A pass that fails stops the rest, naming that pass (a rotation by passes); the rotation then
// resumes at it, the passes before it not run again.
func TestRotateSecretStops(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := "error: the last rotation of K is not finished: `x` ends it"
	var refused *RefusedError
	var stopped *RotationStoppedError
	var failed *FailedError

	r := &passRecorder{exits: []int{3}, stderr: refusal}
	err := (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).RotateSecret(ctx, "K", RotatePasses, 0)
	if !errors.As(err, &refused) || !errors.As(err, &stopped) || stopped.Pass != 0 || len(r.runs) != 1 || !strings.Contains(refused.Reason, "is not finished") {
		t.Errorf("stage refused: %v (%d runs)", err, len(r.runs))
	}

	r = &passRecorder{exits: []int{0, 0, 1}, stderr: "error: compose up failed"}
	err = (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).RotateSecret(ctx, "K", RotatePasses, 0)
	if !errors.As(err, &stopped) || !errors.As(err, &failed) || stopped.Pass != 2 || stopped.Name != "`--finish`" || failed.Exit != 1 ||
		!strings.Contains(err.Error(), "stopped at `--finish`: the next apply runs it again") {
		t.Errorf("finish failed: %v", err)
	}

	r = &passRecorder{exits: []int{0, 3}, stderr: refusal}
	err = (VM{Runner: r, Short: "notes"}).RotateSecret(ctx, "K", RotatePasses, 0)
	if !errors.As(err, &stopped) || !errors.As(err, &refused) || stopped.Pass != 1 || stopped.Name != "the swap" || len(r.runs) != 2 {
		t.Errorf("vm swap refused: %v", err)
	}

	// Resumed at the swap: the swap, then --finish, no --stage.
	r = &passRecorder{}
	if err := (K8s{Runner: r, Dir: "/op", Name: "notes", Namespace: "ns"}).RotateSecret(ctx, "K", RotatePasses, 1); err != nil || len(r.runs) != 2 ||
		!slices.Equal(r.runs[0][9:], []string{"sh", "./notes.sh", "k8s", "secrets", "rotate", "K", "--namespace", "ns"}) ||
		!slices.Equal(r.runs[1][9:], []string{"sh", "./notes.sh", "k8s", "secrets", "rotate", "K", "--finish", "--namespace", "ns"}) {
		t.Errorf("resumed: %v %q", err, r.runs)
	}
	// A pass out of range starts over; the one pass ignores it.
	r = &passRecorder{}
	if err := (VM{Runner: r, Short: "notes"}).RotateSecret(ctx, "K", RotatePasses, 7); err != nil || len(r.runs) != 3 {
		t.Errorf("out of range: %v %d", err, len(r.runs))
	}
	r = &passRecorder{}
	if err := (VM{Runner: r, Short: "notes"}).RotateSecret(ctx, "K", RotateReplace, 1); err != nil || len(r.runs) != 1 {
		t.Errorf("replace from 1: %v %d", err, len(r.runs))
	}

	// The one pass failing is its own error.
	r = &passRecorder{exits: []int{255}, stderr: "ssh: connect to host"}
	err = (VM{Runner: r, Short: "notes"}).RotateSecret(ctx, "K", RotateReplace, 0)
	if !errors.As(err, &failed) || errors.As(err, &stopped) || failed.Exit != 255 {
		t.Errorf("replace failed: %v", err)
	}
}
