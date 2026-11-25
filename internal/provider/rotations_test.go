package provider

import (
	"encoding/base64"
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// describeRot is describe1 with its secrets' ways (Stowage >= 0.46.0): SESSION_SECRET in one pass
// (but on Kubernetes), INTERNAL in three, POSTGRES_PASSWORD in none.
var describeRot = strings.TrimSuffix(describe1, "}") + `,"secrets":[
{"key":"INTERNAL","rotate":{"docker":"passes","kubernetes":"passes","kairos":"passes"},"settable":{},"as_file":false},
{"key":"POSTGRES_PASSWORD","rotate":{"docker":"none","kubernetes":"none","kairos":"none"},"settable":{},"as_file":false},
{"key":"SESSION_SECRET","rotate":{"docker":"replace","kubernetes":"none","kairos":"replace"},"settable":{},"as_file":false}]}`

func numMap(m map[string]int64) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for k, v := range m {
		vals[k] = num(v)
	}
	return tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, vals)
}

func rotationsOf(t *testing.T, h *harness, state *tfprotov6.DynamicValue) map[string]int64 {
	t.Helper()
	v := h.attrs(state)["secret_rotations"]
	if v.IsNull() {
		return nil
	}
	vals := map[string]tftypes.Value{}
	if err := v.As(&vals); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for k, n := range vals {
		var f big.Float
		if err := n.As(&f); err != nil {
			t.Fatal(err)
		}
		i, _ := f.Int64()
		out[k] = i
	}
	return out
}

// rotateCalls are the `secrets rotate` runs made, each as its arguments after the verb's key.
func rotateCalls(h *harness) [][]string {
	var out [][]string
	for _, c := range h.fake.callsOf("secrets") {
		if c.argv[8] != "dir" {
			h.t.Errorf("secrets rotate without --dir: %q", c.argv)
		}
		i := slices.Index(c.argv, "rotate")
		out = append(out, c.argv[i+1:])
	}
	return out
}

func rotationsHarness(t *testing.T) *harness {
	h := newHarness(t, nil)
	h.fake.set("describe", reply{stdout: describeRot})
	h.fake.set("kept-describe", reply{stdout: describeRot})
	h.fake.set("secrets", reply{})
	return h
}

func withRotations(h *harness, m map[string]int64) tftypes.Value {
	return h.dockerConfig(map[string]tftypes.Value{"secret_rotations": numMap(m)})
}

// A create records the numbers and rotates nothing; a changed number rotates its key after the
// update's other steps, in its way, and records the number; an unchanged one runs nothing.
func TestSecretRotationsRotateOnChange(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"SESSION_SECRET": 1, "INTERNAL": 1}))
	if len(rotateCalls(h)) != 0 {
		t.Fatalf("a create rotated: %q", rotateCalls(h))
	}
	h.fake.reset()
	if a := h.update(state, withRotations(h, map[string]int64{"SESSION_SECRET": 1, "INTERNAL": 1})); errorsOf(a.Diagnostics) != "" || len(h.fake.calls) != 0 {
		t.Fatalf("unchanged: %s %q", errorsOf(a.Diagnostics), h.fake.ops())
	}

	a := h.update(state, withRotations(h, map[string]int64{"SESSION_SECRET": 2, "INTERNAL": 1}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := rotateCalls(h); !slices.EqualFunc(got, [][]string{{"SESSION_SECRET", "--yes", "--no-input"}}, slices.Equal) {
		t.Fatalf("replace: %q", got)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"kept-describe", "secrets"}) {
		t.Fatalf("calls %q", got)
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 2 || got["INTERNAL"] != 1 {
		t.Fatalf("state %v", got)
	}

	h.fake.reset()
	b := h.update(a.NewState, withRotations(h, map[string]int64{"SESSION_SECRET": 2, "INTERNAL": 5}))
	if d := errorsOf(b.Diagnostics); d != "" {
		t.Fatal(d)
	}
	want := [][]string{{"INTERNAL", "--stage"}, {"INTERNAL"}, {"INTERNAL", "--finish"}}
	if got := rotateCalls(h); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("passes: %q", got)
	}
	if got := rotationsOf(t, h, b.NewState); got["INTERNAL"] != 5 {
		t.Fatalf("state %v", got)
	}

	// A key taken out is forgotten, nothing run.
	h.fake.reset()
	c := h.update(b.NewState, withRotations(h, map[string]int64{"INTERNAL": 5}))
	if d := errorsOf(c.Diagnostics); d != "" || len(rotateCalls(h)) != 0 {
		t.Fatalf("removed: %s %q", d, rotateCalls(h))
	}
	if got := rotationsOf(t, h, c.NewState); len(got) != 1 {
		t.Fatalf("state %v", got)
	}
}

// A rotation runs after the update's other steps, on the release they leave.
func TestSecretRotationsAfterTheReRun(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	cfg := func(email string, n int64) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": email}), "secret_rotations": numMap(map[string]int64{"SESSION_SECRET": n})})
	}
	state := h.create(cfg("a@example.com", 1))
	h.fake.reset()
	a := h.update(state, cfg("b@example.com", 2))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	ops := h.fake.ops()
	if r, s := slices.Index(ops, "rerun"), slices.Index(ops, "secrets"); r < 0 || s < r {
		t.Fatalf("calls %q", ops)
	}

	// The re-run failing: nothing rotated, the number not recorded.
	h.fake.reset()
	h.fake.then("rerun", reply{exit: 1, stderr: "error: compose up failed"})
	b := h.update(a.NewState, cfg("c@example.com", 3))
	if errorsOf(b.Diagnostics) == "" || len(rotateCalls(h)) != 0 {
		t.Fatalf("re-run failed: %s %q", errorsOf(b.Diagnostics), h.fake.ops())
	}
	if got := rotationsOf(t, h, b.NewState); got["SESSION_SECRET"] != 2 {
		t.Fatalf("state %v", got)
	}
}

// A pass that fails stops the rotation: the key's number stays, so the next plan shows it again,
// and a key after it is not run. The next apply resumes it at the pass that failed, the passes
// before it not run again.
func TestSecretRotationsStopAtAFailure(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	both := func(n int64) tftypes.Value {
		return withRotations(h, map[string]int64{"SESSION_SECRET": n, "INTERNAL": n})
	}
	state := h.create(both(1))
	h.fake.reset()
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.apply(state, h.plan(state, both(2)).PlannedState, both(2))
	d := errorsOf(a.Diagnostics)
	if !strings.Contains(d, "Rotating INTERNAL") || !strings.Contains(d, "stopped at the swap: the next apply runs it again") {
		t.Fatalf("diagnostic: %s", d)
	}
	// INTERNAL first (key order), so SESSION_SECRET was not run.
	if got := rotateCalls(h); len(got) != 2 || got[0][0] != "INTERNAL" || got[1][0] != "INTERNAL" {
		t.Fatalf("calls %q", got)
	}
	if got := rotationsOf(t, h, a.NewState); got["INTERNAL"] != 1 || got["SESSION_SECRET"] != 1 {
		t.Fatalf("state %v", got)
	}
	// Resumed at the swap: no second --stage.
	h.fake.reset()
	b := h.update(a.NewState, both(2))
	if d := errorsOf(b.Diagnostics); d != "" {
		t.Fatal(d)
	}
	want := [][]string{{"INTERNAL"}, {"INTERNAL", "--finish"}, {"SESSION_SECRET", "--yes", "--no-input"}}
	if got := rotateCalls(h); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("resumed: %q", got)
	}
	// Done: the next rotation starts at --stage.
	h.fake.reset()
	c := h.update(b.NewState, withRotations(h, map[string]int64{"SESSION_SECRET": 2, "INTERNAL": 3}))
	if got := rotateCalls(h); errorsOf(c.Diagnostics) != "" || len(got) != 3 || got[0][1] != "--stage" {
		t.Fatalf("after: %s %q", errorsOf(c.Diagnostics), got)
	}
}

// A resumed pass that fails again keeps the resume point (a pass run again is safe), and says how
// to forget it when the target moved on by hand: the key taken out, then given back. A refused
// first pass changed nothing: no resume point.
func TestSecretRotationsResumeRefused(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1}))
	h.fake.then("secrets", reply{}, reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2}))
	if d := errorsOf(a.Diagnostics); !strings.Contains(d, "Rotating INTERNAL stopped part-way") {
		t.Fatalf("finish failed: %s", d)
	}
	for range 2 {
		h.fake.reset()
		h.fake.then("secrets", reply{exit: 3, stderr: "error: the VM runs another release than its OS"})
		b := h.update(a.NewState, withRotations(h, map[string]int64{"INTERNAL": 2}))
		if d := errorsOf(b.Diagnostics); !strings.Contains(d, "resumed the rotation at `--finish`") || !strings.Contains(d, "take INTERNAL out of secret_rotations") {
			t.Fatalf("resume refused: %s", d)
		}
		if got := rotateCalls(h); len(got) != 1 || got[0][1] != "--finish" {
			t.Fatalf("calls %q", got)
		}
		a = b
	}
	// Out, then back: recorded, and the next rotation starts at --stage.
	out := h.update(a.NewState, h.dockerConfig(nil))
	back := h.update(out.NewState, withRotations(h, map[string]int64{"INTERNAL": 7}))
	h.fake.reset()
	c := h.update(back.NewState, withRotations(h, map[string]int64{"INTERNAL": 8}))
	if got := rotateCalls(h); errorsOf(c.Diagnostics) != "" || len(got) != 3 || got[0][1] != "--stage" {
		t.Fatalf("over: %s %q", errorsOf(c.Diagnostics), got)
	}

	// --stage refused: nothing changed, the installer's refusal as it is, no resume point.
	h.fake.reset()
	h.fake.then("secrets", reply{exit: 3, stderr: "error: the last rotation of INTERNAL is not finished: `x` ends it"})
	d := h.update(c.NewState, withRotations(h, map[string]int64{"INTERNAL": 9}))
	if e := errorsOf(d.Diagnostics); !strings.Contains(e, "not finished") || strings.Contains(e, "stopped part-way") {
		t.Fatalf("stage refused: %s", e)
	}
	if p := h.plan(d.NewState, withRotations(h, map[string]int64{"INTERNAL": 8})); strings.Contains(diagText(p.Diagnostics), "is not finished") {
		t.Fatalf("a refused --stage left a resume point: %s", diagText(p.Diagnostics))
	}
}

// A rotation left part-way whose number is given back to state's is a plan warning; a key taken
// out (its resume point forgotten by that apply) and added back is none.
func TestSecretRotationsPendingWarning(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2}))
	p := h.plan(a.NewState, withRotations(h, map[string]int64{"INTERNAL": 1}))
	if d := diagText(p.Diagnostics); errorsOf(p.Diagnostics) != "" || !strings.Contains(d, "the rotation of INTERNAL is not finished") {
		t.Fatalf("plan: %s", d)
	}
	out := h.update(a.NewState, h.dockerConfig(nil))
	if p := h.plan(out.NewState, withRotations(h, map[string]int64{"INTERNAL": 5})); strings.Contains(diagText(p.Diagnostics), "is not finished") {
		t.Fatalf("warned for a key state lacks: %s", diagText(p.Diagnostics))
	}
}

// The ways a plan checks by are the release state runs: after an upgrade, those of the release
// before are not read, and the apply checks on the new release's describe.
func TestSecretRotationsWaysOfTheRunningRelease(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(h.dockerConfig(nil))
	h.fake.set("describe", reply{stdout: strings.Replace(describeRot, `"version":"1.2.0"`, `"version":"2.0.0"`, 1)})
	h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
	h.fake.set("status", reply{stdout: status2})
	up := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	if d := errorsOf(up.Diagnostics); d != "" {
		t.Fatal(d)
	}
	// 2.0.0 replaces POSTGRES_PASSWORD; 1.2.0, whose ways the create kept, did not.
	two := strings.Replace(strings.Replace(describeRot, `"version":"1.2.0"`, `"version":"2.0.0"`, 1),
		`"POSTGRES_PASSWORD","rotate":{"docker":"none"`, `"POSTGRES_PASSWORD","rotate":{"docker":"replace"`, 1)
	h.fake.set("kept-describe", reply{stdout: two})
	cfg := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "secret_rotations": numMap(map[string]int64{"POSTGRES_PASSWORD": 1})})
	a := h.update(up.NewState, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := rotationsOf(t, h, a.NewState); got["POSTGRES_PASSWORD"] != 1 {
		t.Fatalf("state %v", got)
	}
}

// Ways of another revision of the same version are not the running release's either.
func TestSecretRotationsWaysOfTheRunningRevision(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(h.dockerConfig(nil))
	// The install now runs revision 2 (a refresh read it): POSTGRES_PASSWORD replaced there.
	h.fake.set("status", reply{stdout: strings.Replace(h.fake.replies["status"].stdout, `"revision":1`, `"revision":2`, 1)})
	read := h.read(state)
	if d := errorsOf(read.Diagnostics); d != "" {
		t.Fatal(d)
	}
	cfg := withRotations(h, map[string]int64{"POSTGRES_PASSWORD": 1})
	if d := errorsOf(h.plan(read.NewState, cfg).Diagnostics); d != "" {
		t.Fatalf("plan by the ways of revision 1: %s", d)
	}
}

// An update that changes the release, secret_rotations kept, reads the new release's ways: a plan
// after it checks by them.
func TestSecretRotationsWaysAfterAnUpgrade(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	keep := map[string]int64{"SESSION_SECRET": 1}
	state := h.create(withRotations(h, keep))
	two := strings.Replace(describeRot, `"version":"1.2.0"`, `"version":"2.0.0"`, 1)
	h.fake.set("describe", reply{stdout: two})
	h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
	h.fake.set("status", reply{stdout: status2})
	h.fake.set("kept-describe", reply{stdout: two})
	cfg := func(m map[string]int64) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "secret_rotations": numMap(m)})
	}
	up := h.update(state, cfg(keep))
	if d := errorsOf(up.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if d := errorsOf(h.plan(up.NewState, cfg(map[string]int64{"SESSION_SECRET": 1, "POSTGRES_PASSWORD": 1})).Diagnostics); !strings.Contains(d, "POSTGRES_PASSWORD") {
		t.Fatalf("plan after the upgrade: %s", d)
	}
}

// An error before the update's first step saves keeps the numbers not rotated: a guard on the
// framework's prefill of the response with the prior state (a failure after a save:
// TestSecretRotationsUpgradeFailsAfterTheReRun).
func TestSecretRotationsEarlyFailureRecordsNothing(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"SESSION_SECRET": 1}))
	h.fake.then("status", reply{exit: 255, stderr: "ssh: connect to host 127.0.0.1: Connection refused"})
	cfg := h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "z@example.com"}), "secret_rotations": numMap(map[string]int64{"SESSION_SECRET": 2})})
	a := h.update(state, cfg)
	if errorsOf(a.Diagnostics) == "" || len(rotateCalls(h)) != 0 {
		t.Fatalf("%s %q", errorsOf(a.Diagnostics), rotateCalls(h))
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 1 {
		t.Fatalf("state %v", got)
	}
}

// A way this provider does not run is refused before any rotation.
func TestSecretRotationsUnknownWay(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	future := strings.Replace(describeRot, `"SESSION_SECRET","rotate":{"docker":"replace"`, `"SESSION_SECRET","rotate":{"docker":"future"`, 1)
	h.fake.set("describe", reply{stdout: future})
	h.fake.set("kept-describe", reply{stdout: future})
	cfg := withRotations(h, map[string]int64{"SESSION_SECRET": 1})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); !strings.Contains(d, `gives SESSION_SECRET the rotate way "future" on docker, which this provider does not run`) || len(rotateCalls(h)) != 0 {
		t.Fatalf("create: %s %q", d, rotateCalls(h))
	}
}

// A key the release does not rotate on the target is refused: at create before the install, at
// plan once the ways are known, by the apply before any rotation otherwise.
func TestSecretRotationsRefused(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	cfg := withRotations(h, map[string]int64{"POSTGRES_PASSWORD": 1})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); !strings.Contains(d, "POSTGRES_PASSWORD does not rotate on docker: The describe of notes 1.2.0 gives POSTGRES_PASSWORD the rotate way none on docker: its rotation is refused there. Take POSTGRES_PASSWORD out of secret_rotations.") || slices.Contains(h.fake.ops(), "install") {
		t.Fatalf("create: %s %q", d, h.fake.ops())
	}
	cfg = withRotations(h, map[string]int64{"NOPE": 1})
	if d := errorsOf(h.apply(nil, h.plan(nil, cfg).PlannedState, cfg).Diagnostics); !strings.Contains(d, "lists no generated secret NOPE (it lists INTERNAL, POSTGRES_PASSWORD, SESSION_SECRET)") {
		t.Fatalf("create: %s", d)
	}

	state := h.create(withRotations(h, map[string]int64{"SESSION_SECRET": 1}))
	h.fake.reset()
	for _, m := range []map[string]int64{{"SESSION_SECRET": 1, "POSTGRES_PASSWORD": 1}, {"SESSION_SECRET": 1, "NOPE": 3}} {
		if d := errorsOf(h.plan(state, withRotations(h, m)).Diagnostics); !strings.Contains(d, "secret_rotations") {
			t.Fatalf("plan %v: %s", m, d)
		}
	}
	if len(h.fake.calls) != 0 {
		t.Fatalf("a plan ran %q", h.fake.ops())
	}

	// Without the ways (a state from before secret_rotations): the apply refuses, before any
	// rotation, and records nothing.
	h.keepPrivate(state, nil)
	cfg = withRotations(h, map[string]int64{"SESSION_SECRET": 2, "POSTGRES_PASSWORD": 1})
	p := h.plan(state, cfg)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatalf("plan without ways: %s", d)
	}
	a = h.apply(state, p.PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); !strings.Contains(d, "POSTGRES_PASSWORD does not rotate on docker") || len(rotateCalls(h)) != 0 {
		t.Fatalf("apply without ways: %s %q", d, rotateCalls(h))
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 1 || len(got) != 1 {
		t.Fatalf("state %v", got)
	}
	// That apply read the ways: the next plan refuses.
	if d := errorsOf(h.plan(a.NewState, cfg).Diagnostics); !strings.Contains(d, "The describe of the release the install runs (1.2.0) gives POSTGRES_PASSWORD the rotate way none on docker") {
		t.Fatalf("plan after: %s", d)
	}
}

// A release whose describe lists no secrets (before Stowage 0.46.0) takes no secret_rotations.
func TestSecretRotationsNeedARelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	cfg := withRotations(h, map[string]int64{"SESSION_SECRET": 1})
	if d := errorsOf(h.apply(nil, h.plan(nil, cfg).PlannedState, cfg).Diagnostics); !strings.Contains(d, "Stowage 0.46.0 or later") {
		t.Fatalf("create: %s", d)
	}
	state := h.create(h.dockerConfig(nil))
	// The create kept the release's (no) ways: a key added is refused at plan.
	if d := errorsOf(h.plan(state, cfg).Diagnostics); !strings.Contains(d, "Stowage 0.46.0 or later") {
		t.Fatalf("plan: %s", d)
	}
}

// A key added later is checked and recorded, never rotated.
func TestSecretRotationsAddedKey(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(h.dockerConfig(nil))
	h.fake.reset()
	a := h.update(state, withRotations(h, map[string]int64{"SESSION_SECRET": 4}))
	if d := errorsOf(a.Diagnostics); d != "" || len(rotateCalls(h)) != 0 {
		t.Fatalf("added: %s %q", d, rotateCalls(h))
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 4 {
		t.Fatalf("state %v", got)
	}
}

func TestSecretRotationsValidate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if d := errorsOf(h.validate(withRotations(h, map[string]int64{"9BAD": 1}))); !strings.Contains(d, "not a secret's key") {
		t.Fatalf("validate: %s", d)
	}
	nullValue := tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"K": tftypes.NewValue(tftypes.Number, nil)})
	if d := errorsOf(h.validate(h.dockerConfig(map[string]tftypes.Value{"secret_rotations": nullValue}))); !strings.Contains(d, "secret_rotations.K is null") {
		t.Fatalf("validate null: %s", d)
	}
}

// On Kubernetes, the rotation runs by the installer of the release state names, fetched for it,
// with the release's flags and no --registry-ca (the verb takes none, even with the provider's
// registry CA set); SESSION_SECRET (replace: true) is refused there.
func TestSecretRotationsKubernetes(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes"), "registry_ca": str("-----BEGIN CERTIFICATE-----\nX\n")})
	desc := strings.TrimSuffix(describeK8s, "}") + `,"secrets":[
{"key":"INTERNAL","rotate":{"kubernetes":"passes"},"settable":{},"as_file":false},
{"key":"OTHER","rotate":{"docker":"replace"},"settable":{},"as_file":false},
{"key":"SESSION_SECRET","rotate":{"docker":"replace","kubernetes":"none"},"settable":{},"as_file":false}]}`
	h.fake.set("k8s-describe", reply{stdout: desc})
	h.fake.set("k8s-secrets", reply{})
	cfg := func(m map[string]int64) tftypes.Value {
		return h.k8sConfig(map[string]tftypes.Value{"secret_rotations": numMap(m)})
	}
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(cfg(map[string]int64{"INTERNAL": 1}))
	h.fake.reset()
	a := h.update(state, cfg(map[string]int64{"INTERNAL": 2}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	var got [][]string
	for _, c := range h.fake.callsOf("k8s-secrets") {
		if c.argv[6] != "" {
			t.Errorf("k8s secrets rotate given files (%q): it takes no --registry-ca", c.argv[6])
		}
		got = append(got, c.argv[11:])
	}
	want := [][]string{
		{"k8s", "secrets", "rotate", "INTERNAL", "--stage", "--namespace", "notes"},
		{"k8s", "secrets", "rotate", "INTERNAL", "--namespace", "notes"},
		{"k8s", "secrets", "rotate", "INTERNAL", "--finish", "--namespace", "notes"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("calls %q", got)
	}
	if got := rotationsOf(t, h, a.NewState); got["INTERNAL"] != 2 {
		t.Fatalf("state %v", got)
	}
	if d := errorsOf(h.plan(a.NewState, cfg(map[string]int64{"INTERNAL": 2, "SESSION_SECRET": 1})).Diagnostics); !strings.Contains(d, "SESSION_SECRET does not rotate on kubernetes") {
		t.Fatalf("plan: %s", d)
	}
	if d := errorsOf(h.plan(a.NewState, cfg(map[string]int64{"INTERNAL": 2, "OTHER": 1})).Diagnostics); !strings.Contains(d, "names no rotate way for OTHER on kubernetes") {
		t.Fatalf("plan: %s", d)
	}
}

// On the VM, the rotation runs over SSH by the host daemon's installer, as the other verbs.
func TestSecretRotationsVM(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	desc := strings.TrimSuffix(vmDescribe, "}") + `,"secrets":[{"key":"SESSION_SECRET","rotate":{"docker":"replace","kairos":"replace"},"settable":{},"as_file":false}]}`
	f.set("describe", reply{stdout: desc})
	f.set("secrets", reply{})
	cfg := func(n int64) tftypes.Value {
		return h.vmConfig("1.1.0", map[string]tftypes.Value{"secret_rotations": numMap(map[string]int64{"SESSION_SECRET": n})})
	}
	state := h.create(cfg(1))
	f.reset()
	a := h.update(state, cfg(2))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	calls := f.callsOf("secrets")
	if len(calls) != 1 || !slices.Equal(calls[0].argv[6:], []string{"sudo", "-n", "notes-stow", "secrets", "rotate", "SESSION_SECRET", "--yes", "--no-input"}) {
		t.Fatalf("calls %q (ops %q)", calls, f.ops())
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 2 {
		t.Fatalf("state %v", got)
	}
}

// A key taken out, then added back, forgets where its rotation stopped: its next rotation starts
// at --stage.
func TestSecretRotationsPendingForgotten(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1, "SESSION_SECRET": 1}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2, "SESSION_SECRET": 1}))
	if errorsOf(a.Diagnostics) == "" {
		t.Fatal("the swap did not fail")
	}
	// Taken out alone (nothing rotates, the phase does not run), then added back.
	b := h.update(a.NewState, withRotations(h, map[string]int64{"SESSION_SECRET": 1}))
	c := h.update(b.NewState, withRotations(h, map[string]int64{"SESSION_SECRET": 2, "INTERNAL": 5}))
	h.fake.reset()
	d := h.update(c.NewState, withRotations(h, map[string]int64{"SESSION_SECRET": 2, "INTERNAL": 6}))
	if got := rotateCalls(h); errorsOf(d.Diagnostics) != "" || len(got) != 3 || got[0][1] != "--stage" {
		t.Fatalf("%s %s %s %q", errorsOf(b.Diagnostics), errorsOf(c.Diagnostics), errorsOf(d.Diagnostics), got)
	}
}

// A rotation that fails after the update's other steps keeps what the install records as the
// next refresh's baseline.
func TestSecretRotationsBaselineKeptWhenARotationFails(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	cfg := func(email string, n int64) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": email}), "secret_rotations": numMap(map[string]int64{"SESSION_SECRET": n})})
	}
	state := h.create(cfg("a@example.com", 1))
	h.fake.set("status", reply{stdout: strings.Replace(h.fake.replies["status"].stdout, `"format":1,`, `"format":1,"values":{"inputs":{"ADMIN_EMAIL":"recorded-b"}},`, 1)})
	h.fake.then("secrets", reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, cfg("b@example.com", 2))
	if errorsOf(a.Diagnostics) == "" {
		t.Fatal("the rotation did not fail")
	}
	if !strings.Contains(string(h.privateOf(a.NewState)), base64.StdEncoding.EncodeToString([]byte(`{"ADMIN_EMAIL":"recorded-b"}`))) {
		t.Fatalf("the baseline was not kept: %s", h.privateOf(a.NewState))
	}
}

// A key taken out in an update whose other steps fail forgets its resume point all the same:
// given back with another number, it rotates from --stage.
func TestSecretRotationsPendingForgottenWhenTheUpdateFails(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2}))
	h.fake.then("rerun", reply{exit: 1, stderr: "error: compose up failed"})
	b := h.update(a.NewState, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "z@example.com"})}))
	if errorsOf(b.Diagnostics) == "" {
		t.Fatal("the re-run did not fail")
	}
	h.fake.reset()
	c := h.update(b.NewState, withRotations(h, map[string]int64{"INTERNAL": 5}))
	if got := rotateCalls(h); errorsOf(c.Diagnostics) != "" || len(got) != 3 || got[0][1] != "--stage" {
		t.Fatalf("%s %q", errorsOf(c.Diagnostics), got)
	}
}

// A failed read of the new release's ways after an upgrade that succeeded is a warning.
func TestSecretRotationsWaysReadFailsAfterAnUpgrade(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	keep := map[string]int64{"SESSION_SECRET": 1}
	state := h.create(withRotations(h, keep))
	h.fake.set("describe", reply{stdout: strings.Replace(describeRot, `"version":"1.2.0"`, `"version":"2.0.0"`, 1)})
	h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
	h.fake.set("status", reply{stdout: status2})
	h.fake.then("kept-describe", reply{stdout: describeRot}, reply{exit: 1, stderr: "error: boom"})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "secret_rotations": numMap(keep)}))
	if d := errorsOf(a.Diagnostics); d != "" || !strings.Contains(diagText(a.Diagnostics), "rotate ways not read)") {
		t.Fatalf("%s | %s", d, diagText(a.Diagnostics))
	}
}

// A resumed pass that succeeds, a later one failing, is no moved-on target: the resume point
// moves on, with no out-and-back advice.
func TestSecretRotationsResumedThenLaterFails(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	b := h.update(a.NewState, withRotations(h, map[string]int64{"INTERNAL": 2}))
	if d := errorsOf(b.Diagnostics); !strings.Contains(d, "stopped at `--finish`") || strings.Contains(d, "take INTERNAL out") {
		t.Fatalf("%s", d)
	}
	h.fake.reset()
	c := h.update(b.NewState, withRotations(h, map[string]int64{"INTERNAL": 2}))
	if got := rotateCalls(h); errorsOf(c.Diagnostics) != "" || len(got) != 1 || got[0][1] != "--finish" {
		t.Fatalf("%s %q", errorsOf(c.Diagnostics), got)
	}
}

// A key whose way is no longer passes drops its resume point.
func TestSecretRotationsPendingDroppedByAnotherWay(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"INTERNAL": 1}))
	h.fake.then("secrets", reply{}, reply{exit: 1, stderr: "error: compose up failed"})
	a := h.update(state, withRotations(h, map[string]int64{"INTERNAL": 2}))
	replace := strings.Replace(describeRot, `"INTERNAL","rotate":{"docker":"passes"`, `"INTERNAL","rotate":{"docker":"replace"`, 1)
	h.fake.set("kept-describe", reply{stdout: replace})
	h.fake.then("secrets", reply{exit: 1, stderr: "error: compose up failed"})
	b := h.update(a.NewState, withRotations(h, map[string]int64{"INTERNAL": 2}))
	if errorsOf(b.Diagnostics) == "" {
		t.Fatal("the replace did not fail")
	}
	h.fake.set("kept-describe", reply{stdout: describeRot})
	h.fake.reset()
	c := h.update(b.NewState, withRotations(h, map[string]int64{"INTERNAL": 2}))
	if got := rotateCalls(h); errorsOf(c.Diagnostics) != "" || len(got) != 3 || got[0][1] != "--stage" {
		t.Fatalf("%s %q", errorsOf(c.Diagnostics), got)
	}
}

// An update whose release step fails after the re-run saved keeps the numbers not rotated.
func TestSecretRotationsUpgradeFailsAfterTheReRun(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	state := h.create(withRotations(h, map[string]int64{"SESSION_SECRET": 1}))
	h.fake.set("describe", reply{stdout: strings.Replace(describeRot, `"version":"1.2.0"`, `"version":"2.0.0"`, 1)})
	h.fake.then("upgrade", reply{exit: 1, stderr: "error: pull failed"})
	cfg := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "z@example.com"}),
		"secret_rotations": numMap(map[string]int64{"SESSION_SECRET": 2})})
	a := h.update(state, cfg)
	if errorsOf(a.Diagnostics) == "" || len(h.fake.callsOf("rerun")) != 1 || len(rotateCalls(h)) != 0 {
		t.Fatalf("%s %q", errorsOf(a.Diagnostics), h.fake.ops())
	}
	if got := rotationsOf(t, h, a.NewState); got["SESSION_SECRET"] != 1 {
		t.Fatalf("state %v", got)
	}
}

// With secret_rotations set and no ways kept (a state of an earlier provider), an update that
// rotates nothing reads them: the next plan checks by them.
func TestSecretRotationsWaysReadWhenNoneKept(t *testing.T) {
	t.Parallel()
	h := rotationsHarness(t)
	cfg := func(email string, m map[string]int64) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": email}), "secret_rotations": numMap(m)})
	}
	state := h.create(cfg("a@example.com", map[string]int64{"SESSION_SECRET": 1}))
	h.keepPrivate(state, nil)
	a := h.update(state, cfg("b@example.com", map[string]int64{"SESSION_SECRET": 1}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if d := errorsOf(h.plan(a.NewState, cfg("b@example.com", map[string]int64{"SESSION_SECRET": 1, "POSTGRES_PASSWORD": 1})).Diagnostics); !strings.Contains(d, "POSTGRES_PASSWORD does not rotate on docker") {
		t.Fatalf("plan: %s", d)
	}
}
