package provider

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

const (
	image2 = "registry.example.com/notes/notes-installer@" + digest2
	// describe2 is the release 2.0.0's offer: it adds NEW_KEY and the secret NEW_TOKEN.
	describe2 = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"2.0.0","revision":1},"channel":"stable","shapes":["docker"],
		"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"NEW_KEY"},{"key":"NEW_TOKEN","secret":true}],
		"routes":[{"name":"web","loopback_port":8080},{"name":"api","loopback_port":9000}]}`
	status2 = `{"format":1,"name":"notes","installed":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"channel":"stable",
		"previous":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"inputs":["ADMIN_EMAIL","NEW_KEY"]}`
)

// flags is the value following each flag in argv, repeated flags joined by a space.
func flags(argv []string) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(argv); i++ {
		if strings.HasPrefix(argv[i], "--") && !strings.HasPrefix(argv[i+1], "--") {
			out[argv[i]] = strings.TrimSpace(out[argv[i]] + " " + argv[i+1])
		}
	}
	return out
}

func inputsOf(t *testing.T, h *harness, state *tfprotov6.DynamicValue) map[string]string {
	t.Helper()
	v := h.attrs(state)["inputs"]
	if v.IsNull() {
		return nil
	}
	vals := map[string]tftypes.Value{}
	if err := v.As(&vals); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for k, s := range vals {
		out[k] = asString(t, s)
	}
	return out
}

func TestUpdateInputsReRunTheRunningRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()

	config := h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	p := h.plan(state, config)
	planned, _ := p.PlannedState.Unmarshal(h.typ)
	vals := map[string]tftypes.Value{}
	_ = planned.As(&vals)
	// What the apply reads back is unknown: the install may run another release than state says
	// (changed outside OpenTofu since the last refresh).
	for _, a := range []string{"status", "version", "revision", "channel", "previous"} {
		if vals[a].IsKnown() {
			t.Fatalf("a re-run plans %s known: %v", a, vals[a])
		}
	}
	if !vals["installer"].IsKnown() {
		t.Fatal("a re-run plans the installer unknown")
	}
	a := h.apply(state, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "rerun", "status"}) {
		t.Fatalf("calls %q", got)
	}
	rerun := h.fake.callsOf("rerun")[0]
	if f := flags(rerun.argv); f["--set"] != "ADMIN_EMAIL=b@example.com" || !contains(rerun.argv, "--no-input") || !contains(rerun.argv, "--json") || rerun.argv[8] != "dir" || rerun.stdin != nil {
		t.Fatalf("re-run %q", rerun.argv)
	}
	if got := inputsOf(t, h, a.NewState); got["ADMIN_EMAIL"] != "b@example.com" || asString(t, h.attrs(a.NewState)["installer"]) != image {
		t.Fatalf("state %v", got)
	}

	// An input taken out of the configuration is given empty: the installer's rule decides.
	h.fake.reset()
	a = h.update(a.NewState, h.dockerConfig(nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if f := flags(h.fake.callsOf("rerun")[0].argv); f["--set"] != "ADMIN_EMAIL=" {
		t.Fatalf("removed input: %q", h.fake.callsOf("rerun")[0].argv)
	}
	if got := inputsOf(t, h, a.NewState); got != nil {
		t.Fatalf("inputs %v, want null", got)
	}
}

func TestUpdateSecretVersionResendsSecrets(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	with := func(v int64) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{
			"secret_inputs_wo": strMap(map[string]string{"SMTP_PASSWORD": "new-" + secretVal}), "secret_inputs_wo_version": num(v)})
	}
	state := h.create(with(1))
	h.fake.reset()
	// The same version: nothing sent, the target not called.
	if d := errorsOf(h.update(state, with(1)).Diagnostics); d != "" || len(h.fake.calls) != 0 {
		t.Fatalf("an unchanged version: %s %q", d, h.fake.ops())
	}
	a := h.update(state, with(2))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	rerun := h.fake.callsOf("rerun")
	if len(rerun) != 1 || rerun[0].argv[7] != "answers" || untarAll(t, rerun[0].stdin)["answers"] != `SMTP_PASSWORD: "new-`+secretVal+`"`+"\n" {
		t.Fatalf("re-run %v", rerun)
	}
	for _, c := range h.fake.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) {
			t.Fatalf("a secret on the %s command line", c.op())
		}
	}
	if bytes.Contains(a.NewState.MsgPack, []byte(secretVal)) {
		t.Fatal("a secret in state")
	}
}

func TestUpdateUpgrade(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "LEGACY": "x"}),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8080)})}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
	h.fake.set("status", reply{stdout: status2})

	config := h.dockerConfig(map[string]tftypes.Value{
		"installer":                str(image2),
		"inputs":                   strMap(map[string]string{"ADMIN_EMAIL": "b@example.com", "NEW_KEY": "added"}),
		"secret_inputs_wo":         strMap(map[string]string{"SMTP_PASSWORD": "old-" + secretVal, "NEW_TOKEN": "tok-" + secretVal}),
		"secret_inputs_wo_version": num(1),
		"ports":                    tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8081), "api": num(9000)}),
	})
	a := h.update(state, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "fetch", "describe", "rerun", "upgrade", "status"}) {
		t.Fatalf("calls %q", got)
	}
	if fetch := h.fake.callsOf("fetch")[0]; fetch.argv[5] != image2 {
		t.Fatalf("fetched %q", fetch.argv)
	}
	// The re-run: what the running release declares, the secrets version bumped from none;
	// LEGACY, which 2.0.0 drops, is left to the upgrade.
	rerun := h.fake.callsOf("rerun")[0]
	if f := flags(rerun.argv); f["--set"] != "ADMIN_EMAIL=b@example.com" || f["--ports"] != "web=8081" ||
		untarAll(t, rerun.stdin)["answers"] != `SMTP_PASSWORD: "old-`+secretVal+`"`+"\n" {
		t.Fatalf("re-run %q %q", rerun.argv, untarAll(t, rerun.stdin))
	}
	// The upgrade: what the new release adds.
	up := h.fake.callsOf("upgrade")[0]
	if f := flags(up.argv); f["--set"] != "NEW_KEY=added" || f["--ports"] != "api=9000" || f["--dir"] != "./notes" ||
		untarAll(t, up.stdin)["answers"] != `NEW_TOKEN: "tok-`+secretVal+`"`+"\n" {
		t.Fatalf("upgrade %q %q", up.argv, untarAll(t, up.stdin))
	}
	at := h.attrs(a.NewState)
	if asString(t, at["installer"]) != image2 || asString(t, at["version"]) != "2.0.0" || asString(t, at["short_name"]) != "notes" {
		t.Fatalf("state %v", at)
	}
	if got := inputsOf(t, h, a.NewState); got["NEW_KEY"] != "added" {
		t.Fatalf("inputs %v", got)
	}
}

// A failed upgrade leaves the reconfigured release in state: the next plan shows the upgrade alone.
func TestUpdateUpgradeFailureSavesTheReRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"}),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8080)})}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("upgrade", reply{stderr: "pull failed", exit: 1})
	config := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com", "NEW_KEY": "added"}),
		"ports":  tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8081), "api": num(9000)})})
	a := h.update(state, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "upgrade failed (exit 1)") || !strings.Contains(got, "pull failed") {
		t.Fatalf("got %q", got)
	}
	at := h.attrs(a.NewState)
	if asString(t, at["installer"]) != image || asString(t, at["version"]) != "1.2.0" {
		t.Fatalf("state after the failed upgrade: %v", at)
	}
	if got := inputsOf(t, h, a.NewState); got["ADMIN_EMAIL"] != "b@example.com" || got["NEW_KEY"] != "" {
		t.Fatalf("inputs %v: the re-run's alone", got)
	}
	if v := at["ports"]; v.String() != tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8081)}).String() {
		t.Fatalf("ports %v: the re-run's alone", v)
	}
	// The next apply: the upgrade alone, NEW_KEY with it.
	h.fake.reset()
	h.fake.set("upgrade", reply{stdout: `{"format":1,"result":"done"}`})
	h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
	h.fake.set("status", reply{stdout: status2})
	a = h.update(a.NewState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); slices.Contains(got, "rerun") || flags(h.fake.callsOf("upgrade")[0].argv)["--set"] != "NEW_KEY=added" {
		t.Fatalf("calls %q", got)
	}
}

func TestUpdateReRunFailureKeepsState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("rerun", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1},"refused":{"reason":"ADMIN_EMAIL is not an address","fix":"give one"}}`, exit: 3})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "nope"})}))
	if got := errorsOf(a.Diagnostics); !strings.HasPrefix(got, "ADMIN_EMAIL is not an address: give one\n\nReconfiguring notes was refused by the installer (re-run, exit 3)") {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "upgrade") {
		t.Fatal("upgraded after a refused re-run")
	}
	if got := inputsOf(t, h, a.NewState); got["ADMIN_EMAIL"] != "a@example.com" || asString(t, h.attrs(a.NewState)["installer"]) != image {
		t.Fatalf("state changed: %v", h.attrs(a.NewState))
	}
}

// A desired installer that describes the release installed is no upgrade: the installer would
// move it to a later revision.
func TestUpdateSameReleaseIsNoUpgrade(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	// The install records its installer's digest: Read would put it back, so the plan is refused.
	if got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)})).Diagnostics); !strings.Contains(got, "set installer to "+image) {
		t.Fatalf("another digest: %q", got)
	}
	// The install records none: the desired one is taken.
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"channel":"stable","previous":null}`})
	h.fake.reset()
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "fetch", "describe", "status"}) {
		t.Fatalf("calls %q", got)
	}
	if asString(t, h.attrs(a.NewState)["installer"]) != image2 {
		t.Fatal("the desired installer is not in state")
	}
}

func TestRollback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.set("status", reply{stdout: status2})
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	back := h.dockerConfig(nil) // installer: image, previous.installer
	if got := errorsOf(h.plan(state, back).Diagnostics); !strings.Contains(got, "a rollback needs allow_rollback") || !strings.Contains(got, "Set allow_rollback = true") {
		t.Fatalf("plan without allow_rollback: %q", got)
	}
	// Another older release is the installer's to refuse: no plan error.
	other := "registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("ab", 32)
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(other)})).Diagnostics); got != "" {
		t.Fatalf("another release: %q", got)
	}
	h.fake.reset()
	h.fake.then("status", reply{stdout: status2})
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null}`})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"allow_rollback": boolean(true)}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "rollback", "status"}) {
		t.Fatalf("calls %q", got)
	}
	if rb := h.fake.callsOf("rollback")[0]; !slices.Equal(rb.argv[9:], []string{"rollback", "--json"}) {
		t.Fatalf("rollback %q", rb.argv)
	}
	if at := h.attrs(a.NewState); asString(t, at["installer"]) != image || asString(t, at["version"]) != "1.2.0" || !at["previous"].IsNull() {
		t.Fatalf("state %v", at)
	}
}

func TestPlanRefusesWhatAReRunCannotSay(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	ports := func(m map[string]int64) tftypes.Value {
		vals := map[string]tftypes.Value{}
		for k, v := range m {
			vals[k] = num(v)
		}
		return tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, vals)
	}
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"tls": str("internal"),
		"domains": strMap(map[string]string{"web": "n.lan", "api": "api.n.lan"})}))
	for name, c := range map[string]struct {
		config tftypes.Value
		want   string
	}{
		"tls":    {h.dockerConfig(map[string]tftypes.Value{"domains": strMap(map[string]string{"web": "n.lan", "api": "api.n.lan"})}), "tls cannot be removed"},
		"domain": {h.dockerConfig(map[string]tftypes.Value{"tls": str("internal"), "domains": strMap(map[string]string{"web": "n.lan"})}), "domains.api cannot be removed"},
	} {
		if got := errorsOf(h.plan(state, c.config).Diagnostics); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	// Another mode and another domain are a re-run's.
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"tls": str("acme"),
		"domains": strMap(map[string]string{"web": "m.lan", "api": "api.n.lan"})})).Diagnostics); got != "" {
		t.Fatalf("a change: %q", got)
	}
	loop := h.create(h.dockerConfig(map[string]tftypes.Value{"ports": ports(map[string]int64{"web": 8080})}))
	if got := errorsOf(h.plan(loop, h.dockerConfig(nil)).Diagnostics); !strings.Contains(got, "ports.web cannot be removed") {
		t.Fatalf("ports: %q", got)
	}
}

func TestCreateWaitsForTheFirstInstall(t *testing.T) {
	t.Parallel()
	installing := reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"installing":true}`}
	h := newHarness(t, nil)
	h.fake.then("status", installing, installing)
	state := h.create(h.dockerConfig(nil))
	if got := h.fake.ops(); !slices.Equal(got, []string{"fetch", "describe", "new-status", "install", "status", "status", "status"}) || asString(t, h.attrs(state)["status"]) != "installed" {
		t.Fatalf("calls %q, status %s", got, asString(t, h.attrs(state)["status"]))
	}

	h = newHarness(t, nil)
	h.fake.then("status", installing)
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"installing":true,"install_failed":"provision hook exited 1"}`})
	config := h.dockerConfig(nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the first install failed: provision hook exited 1") {
		t.Fatalf("failed: %q", got)
	}
	if h.attrs(a.NewState) != nil {
		t.Fatal("a failed install left state")
	}

	// Every call before the wait answers however late (lenient): the timeout is the wait's.
	h = newHarness(t, nil)
	h.fake.lenient = true
	h.fake.set("status", installing)
	config = h.dockerConfig(map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("20ms")})})
	a = h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "still installing when the create timeout ran out") {
		t.Fatalf("timeout: %q", got)
	}

	// A status read the create timeout cuts says the same, not the read's own error.
	h = newHarness(t, nil)
	h.fake.lenient = true
	h.fake.set("status", reply{block: true})
	config = h.dockerConfig(map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("50ms")})})
	a = h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "still installing when the create timeout ran out") {
		t.Fatalf("a read cut by the timeout: %q", got)
	}
}

func TestUpdateTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"}),
		"timeouts": h.block("timeouts", map[string]tftypes.Value{"update": str("1ns")})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "deadline exceeded") {
		t.Fatalf("got %q", got)
	}
}

func TestReadDropsUnrecordedInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "NOTE": ""})}))
	// Unknown keys (an installer not carrying the installed release): kept.
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"}}`})
	if got := inputsOf(t, h, h.read(state).NewState); got["ADMIN_EMAIL"] != "a@example.com" {
		t.Fatalf("unknown keys dropped an input: %v", got)
	}
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"inputs":[]}`})
	got := inputsOf(t, h, h.read(state).NewState)
	if _, ok := got["ADMIN_EMAIL"]; ok || len(got) != 1 {
		t.Fatalf("an unrecorded input stays: %v", got)
	}
	// status --json lists every key the install records, whichever release declared it: a listed
	// key stays, an unlisted one goes, and Read runs no describe to tell them.
	state = h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "E2E_NOTE": "hello"})}))
	h.fake.reset()
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"inputs":["E2E_NOTE"]}`})
	got = inputsOf(t, h, h.read(state).NewState)
	if _, ok := got["ADMIN_EMAIL"]; ok || got["E2E_NOTE"] != "hello" {
		t.Fatalf("inputs %v: ADMIN_EMAIL (unlisted) dropped, E2E_NOTE (listed) kept", got)
	}
	if ops := h.fake.ops(); slices.Contains(ops, "kept-describe") {
		t.Fatalf("calls %q", ops)
	}
}

func (h *harness) importState(id string) *tfprotov6.ImportResourceStateResponse {
	h.t.Helper()
	resp, err := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: id})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func TestImport(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	resp := h.importState("app.example:/srv/notes/notes-stow")
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	state := resp.ImportedResources[0].State
	at := h.attrs(state)
	if asString(t, at["id"]) != "/srv/notes" || asString(t, at["short_name"]) != "notes" || !at["registry"].IsNull() {
		t.Fatalf("imported %v", at)
	}
	// Read has no connection yet: the state stays as imported, the target not called.
	r := h.read(state)
	if d := errorsOf(r.Diagnostics); d != "" || len(h.fake.calls) != 0 {
		t.Fatalf("read: %s %q", d, h.fake.ops())
	}
	// The first apply reconfigures the install with the configuration; the address is not compared.
	config := h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	a := h.update(r.NewState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "rerun", "status"}) {
		t.Fatalf("calls %q", got)
	}
	if c := h.fake.conns[0]; c.Host != "app.example" || c.KnownHostsLine != hostLine {
		t.Fatalf("connection %+v", c)
	}
	if f := flags(h.fake.callsOf("rerun")[0].argv); f["--set"] != "ADMIN_EMAIL=a@example.com" || f["--address"] != "" {
		t.Fatalf("re-run %q", h.fake.callsOf("rerun")[0].argv)
	}
	at = h.attrs(a.NewState)
	if asString(t, at["registry"]) != "" || asString(t, at["installer"]) != image || asString(t, at["version"]) != "1.2.0" {
		t.Fatalf("state %v", at)
	}
	// Now a state like any: the address is first-install only again.
	if got := errorsOf(h.plan(a.NewState, h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.3"),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})).Diagnostics); !strings.Contains(got, "address is first-install only") {
		t.Fatalf("address: %q", got)
	}
	// An import of another project's install directory than the installer's.
	other := h.importState("app.example:/srv/atlas/atlas-stow").ImportedResources[0].State
	if got := errorsOf(h.plan(other, h.dockerConfig(nil)).Diagnostics); !strings.Contains(got, "the installer's project is first-install only") {
		t.Fatalf("project: %q", got)
	}

	// A local install is read at once.
	h.fake.reset()
	local := h.importState("./notes/notes-stow").ImportedResources[0].State
	if at := h.attrs(local); asString(t, at["id"]) != "./notes" || !at["docker"].IsKnown() {
		t.Fatalf("local %v", at)
	}
	r = h.read(local)
	if got := h.fake.ops(); errorsOf(r.Diagnostics) != "" || !slices.Equal(got, []string{"status"}) || h.fake.calls[0].argv[4] != "." {
		t.Fatalf("local read: %s %q", errorsOf(r.Diagnostics), got)
	}
}

func TestParseImportID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string][4]string{
		"app.example:/srv/notes/notes-stow": {"app.example", "/srv", "notes", "notes"},
		"/notes/nt-stow":                    {"", "/", "notes", "nt"},
		"notes/notes-stow":                  {"", ".", "notes", "notes"},
		"h:/a/b:c/notes/notes-stow":         {"h", "/a/b:c", "notes", "notes"},
	} {
		host, work, name, short, err := parseImportID(id)
		if err != nil || [4]string{host, work, name, short} != want {
			t.Errorf("%s: got %q %q %q %q %v", id, host, work, name, short, err)
		}
	}
	for _, id := range []string{"", "notes-stow", "/srv/notes/notes", "/srv/notes/-stow", "/srv/Notes/notes-stow", ":/srv/notes/notes-stow",
		"-oProxy=x:/srv/notes/notes-stow", "ops@h:/srv/notes/notes-stow", "/srv//notes-stow",
		"h:a/b:c/notes/notes-stow", "srv/notes/notes-stow", "/srv/../x/notes/notes-stow"} {
		if _, _, _, _, err := parseImportID(id); err == nil {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestUpdateRefusals(t *testing.T) {
	t.Parallel()
	change := map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}
	for name, c := range map[string]struct {
		setup  func(*fake)
		extra  map[string]tftypes.Value
		want   string
		reruns bool
	}{
		"no install": {func(f *fake) { f.set("status", reply{exit: 100, stderr: "stowage-provider: no install directory"}) }, change, "holds no install", false},
		"kept installer of another project": {func(f *fake) {
			f.set("kept-describe", reply{stdout: `{"format":1,"name":"atlas","short_name":"atlas","release":{"version":"1"},"routes":[]}`})
		}, change, `describes the project "atlas"`, false},
		"new installer of another project": {func(f *fake) {
			f.set("describe", reply{stdout: `{"format":1,"name":"atlas","short_name":"atlas","release":{"version":"1"},"routes":[]}`})
		}, map[string]tftypes.Value{"installer": str(image2)}, `describes the project "atlas"`, false},
		"no docker shape": {func(f *fake) {
			f.set("describe", reply{stdout: `{"format":1,"name":"notes","short_name":"notes","release":{"version":"2"},"shapes":["kubernetes"],"routes":[]}`})
		}, map[string]tftypes.Value{"installer": str(image2)}, "offers kubernetes, not docker", false},
		"fetch fails": {func(f *fake) { f.set("fetch", reply{exit: 1, stderr: "manifest unknown"}) }, map[string]tftypes.Value{"installer": str(image2)}, "manifest unknown", false},
		// previous names the desired installer at apply, not at plan: still allow_rollback's.
		"rollback at apply": {func(f *fake) { f.set("status", reply{stdout: status2}) }, map[string]tftypes.Value{"installer": str(image)}, "a rollback needs allow_rollback", false},
		"status unreadable after the re-run": {func(f *fake) {
			f.then("status", f.replies["status"])
			f.set("status", reply{exit: 1, stderr: "docker is down"})
		}, change, "docker is down", true},
	} {
		h := newHarness(t, nil)
		created := image
		if name == "rollback at apply" {
			created = image2 // the state's previous is null: the plan does not see the rollback
		}
		state := h.create(h.dockerConfig(map[string]tftypes.Value{"installer": str(created)}))
		c.setup(h.fake)
		h.fake.reset()
		a := h.update(state, h.dockerConfig(c.extra))
		if got := errorsOf(a.Diagnostics); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
		if ran := len(h.fake.callsOf("rerun")) > 0; ran != c.reruns || slices.Contains(h.fake.ops(), "upgrade") || slices.Contains(h.fake.ops(), "rollback") {
			t.Errorf("%s: calls %q", name, h.fake.ops())
		}
		if c.reruns {
			// The re-run is recorded, the computed values state's.
			if got := inputsOf(t, h, a.NewState); got["ADMIN_EMAIL"] != "b@example.com" || asString(t, h.attrs(a.NewState)["version"]) != "1.2.0" {
				t.Errorf("%s: state %v", name, h.attrs(a.NewState))
			}
		}
	}
}

// Without a new secret_inputs_wo_version, an upgrade re-sends no recorded secret: only those the
// new release adds go with it.
func TestUpgradeKeepsSecretsUnlessBumped(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	secrets := map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"SMTP_PASSWORD": "pw", "NEW_TOKEN": "tok"}), "secret_inputs_wo_version": num(1)}
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"SMTP_PASSWORD": "pw"}), "secret_inputs_wo_version": num(1)}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	secrets["installer"] = str(image2)
	a := h.update(state, h.dockerConfig(secrets))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); slices.Contains(got, "rerun") {
		t.Fatalf("a re-run without a change: %q", got)
	}
	if files := untarAll(t, h.fake.callsOf("upgrade")[0].stdin); files["answers"] != "NEW_TOKEN: \"tok\"\n" {
		t.Fatalf("upgrade answers %q", files)
	}
}

func TestRollbackGoesAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.set("status", reply{stdout: status2})
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	config := h.dockerConfig(map[string]tftypes.Value{"allow_rollback": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if got := errorsOf(h.plan(state, config).Diagnostics); !strings.Contains(got, "a rollback goes alone") || !strings.Contains(got, "inputs changed with it") {
		t.Fatalf("plan: %q", got)
	}
	// previous seen only at apply: refused before anything runs.
	h2 := newHarness(t, nil)
	state = h2.create(h2.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	h2.fake.set("status", reply{stdout: status2})
	h2.fake.reset()
	if got := errorsOf(h2.update(state, config).Diagnostics); !strings.Contains(got, "a rollback goes alone") {
		t.Fatalf("apply: %q", got)
	}
	if ops := h2.fake.ops(); slices.Contains(ops, "rerun") || slices.Contains(ops, "rollback") {
		t.Fatalf("calls %q", ops)
	}
}

func TestDomainAddedWithAReleaseChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"tls": str("internal"), "domains": strMap(map[string]string{"web": "n.lan"})}))
	added := strMap(map[string]string{"web": "n.lan", "api": "api.n.lan"})
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "tls": str("internal"), "domains": added})).Diagnostics); !strings.Contains(got, "a domain added with a release change") {
		t.Fatalf("got %q", got)
	}
	// Without the release change, the re-run's.
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"tls": str("internal"), "domains": added})).Diagnostics); got != "" {
		t.Fatalf("a re-run's domain: %q", got)
	}
}

// An imported state records no port: a configured one goes to the re-run, never to the upgrade
// (which refuses a route the install records).
func TestImportedPortsGoToTheReRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	h.fake.set("describe", reply{stdout: describe2})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8080)})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if flags(h.fake.callsOf("rerun")[0].argv)["--ports"] != "web=8080" || flags(h.fake.callsOf("upgrade")[0].argv)["--ports"] != "" {
		t.Fatalf("calls %q", h.fake.calls)
	}
}

func TestImportedRollbackIsTakenOverFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	h.fake.set("status", reply{stdout: status2})
	got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"allow_rollback": boolean(true)})).Diagnostics)
	if !strings.Contains(got, "a rollback of an imported install") || !strings.Contains(got, "installer = "+image2) {
		t.Fatalf("got %q", got)
	}
	if ops := h.fake.ops(); slices.Contains(ops, "rollback") || slices.Contains(ops, "rerun") {
		t.Fatalf("calls %q", ops)
	}
}

// An installer not known at plan time may turn out unchanged: a domain added is not refused.
func TestDomainWithAnUnknownInstaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"tls": str("internal"), "domains": strMap(map[string]string{"web": "n.lan"})}))
	config := h.dockerConfig(map[string]tftypes.Value{"installer": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"tls": str("internal"), "domains": strMap(map[string]string{"web": "n.lan", "api": "api.n.lan"})})
	if got := errorsOf(h.plan(state, config).Diagnostics); got != "" {
		t.Fatalf("got %q", got)
	}
}

// A refused upgrade after the re-run: the refusal's reason and fix, and the re-run's state saved.
func TestUpdateUpgradeRefusalSavesTheReRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1},"refused":{"reason":"no key the install trusts signed notes 2.0.0","fix":"add the publisher's key to the install's trusted keys"}}`, exit: 3})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.HasPrefix(got, "no key the install trusts signed notes 2.0.0: add the publisher's key to the install's trusted keys\n\nUpgrading notes to 2.0.0 was refused") {
		t.Fatalf("got %q", got)
	}
	if got := inputsOf(t, h, a.NewState); got["ADMIN_EMAIL"] != "b@example.com" || asString(t, h.attrs(a.NewState)["installer"]) != image {
		t.Fatalf("state %v: the re-run's, the release state's", h.attrs(a.NewState))
	}
}

// An upgrade with nothing to do (the install already runs that release) succeeds: state is what
// status reads after it.
func TestUpdateUpgradeNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"2.0.0","revision":1}}`})
	h.fake.set("status", reply{stdout: status2})
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if at := h.attrs(a.NewState); asString(t, at["version"]) != "2.0.0" {
		t.Fatalf("state %v", at)
	}
}

// An imported install whose status names no installed release: the rollback is refused all the
// same, never a panic.
func TestImportedRollbackWithNoInstalledRelease(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	h.fake.set("status", reply{stdout: `{"format":1,"name":"notes","installed":null,"previous":{"version":"1.2.0","revision":1,"installer":"` + image + `"}}`})
	got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"allow_rollback": boolean(true)})).Diagnostics)
	if !strings.Contains(got, "a rollback of an imported install") || !strings.Contains(got, "installed.installer") {
		t.Fatalf("got %q", got)
	}
}

// An input moved from inputs to secret_inputs_wo is not given empty: --set would win over its
// answer and empty it.
func TestInputMovedToSecretInputsIsNotEmptied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{
		"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"}), "secret_inputs_wo_version": num(1)}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	rerun := h.fake.callsOf("rerun")
	if len(rerun) != 1 || contains(rerun[0].argv, "--set") || untarAll(t, rerun[0].stdin)["answers"] != "ADMIN_EMAIL: \"b@example.com\"\n" {
		t.Fatalf("re-run %q", rerun)
	}
	// Moved without a new version, its value would never be sent: refused at plan, and so at
	// apply, whose final plan (OpenTofu's, the harness's) holds the configuration then.
	h.fake.reset()
	moved := h.dockerConfig(map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if got := errorsOf(h.plan(state, moved).Diagnostics); !strings.Contains(got, "an input moved to secret_inputs_wo") {
		t.Fatalf("plan: %q", got)
	}
	unchanged := h.plan(state, h.dockerConfig(nil))
	got := errorsOf(h.apply(state, unchanged.PlannedState, moved).Diagnostics)
	if !strings.Contains(got, "an input moved to secret_inputs_wo") || slices.Contains(h.fake.ops(), "rerun") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// Moved with the value the install records: nothing to send, not refused.
	h.fake.reset()
	if got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{
		"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})).Diagnostics); got != "" || slices.Contains(h.fake.ops(), "rerun") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// An input a release declares secret is refused in inputs on an update too, before anything runs.
func TestUpdateRefusesASecretInInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.reset()
	got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"SMTP_PASSWORD": "pw"})})).Diagnostics)
	if !strings.Contains(got, "secret input in inputs") || slices.Contains(h.fake.ops(), "rerun") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// The secret input the new release adds, given in inputs with the upgrade.
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	got = errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"NEW_TOKEN": "t"})})).Diagnostics)
	if !strings.Contains(got, "NEW_TOKEN is a secret input of notes 2.0.0") || slices.Contains(h.fake.ops(), "upgrade") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// An input kept as it is that the new release makes secret.
	h.fake.set("describe", reply{stdout: describe1})
	kept := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: strings.Replace(describe2, `{"key":"ADMIN_EMAIL"}`, `{"key":"ADMIN_EMAIL","secret":true}`, 1)})
	got = errorsOf(h.update(kept, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})).Diagnostics)
	if !strings.Contains(got, "ADMIN_EMAIL is a secret input of notes 2.0.0") || slices.Contains(h.fake.ops(), "upgrade") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	// Given empty (taken out) is no value on the command line: not refused.
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe1})
	if got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": ""})})).Diagnostics); got != "" {
		t.Fatalf("an empty input: %q", got)
	}
}

// A create over an install already in the directory runs the installer again on it without the
// settings only a first install takes; another project's install is refused.
func TestCreateOverAnExistingInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes"), "registry_ca": str("-----BEGIN CERTIFICATE-----\n")})
	held := `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"installing":true,"install_failed":"provision hook exited 1",
		"registry":"mirror.lan/notes","address":"127.0.0.2","registry_ca":"sha256:b93f51c3ac1bdd90edcce2019d2452a328cbf97443f3744cc7ec63033a5b2c16"}`
	config := h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"), "secrets_wo": str("export-" + secretVal),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	p := h.plan(nil, config)
	// The install recorded other first-install settings than the configuration's: refused,
	// each named, before anything runs (a re-run is never given them).
	for field, want := range map[string]string{
		`"address":"127.0.0.2"`:         `address is "127.0.0.2", and the install holds "127.0.0.3"`,
		`"registry":"mirror.lan/notes"`: `the install pulls from "ghcr.io/acme/notes"`,
		`"registry_ca":"` + "sha256:b93f51c3ac1bdd90edcce2019d2452a328cbf97443f3744cc7ec63033a5b2c16" + `"`: "and the install keeps none",
	} {
		other := map[string]string{`"address":"127.0.0.2"`: `"address":"127.0.0.3"`, `"registry":"mirror.lan/notes"`: `"registry":"ghcr.io/acme/notes"`}[field]
		if other == "" {
			other = `"registry_ca":""`
		}
		h.fake.reset()
		h.fake.set("new-status", reply{stdout: strings.Replace(held, field, other, 1)})
		got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics)
		if !strings.Contains(got, want) || slices.Contains(h.fake.ops(), "install") {
			t.Errorf("%s: got %q, calls %q", field, got, h.fake.ops())
		}
	}
	h.fake.reset()
	h.fake.set("new-status", reply{stdout: held})
	a := h.apply(nil, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if !strings.Contains(diagText(a.Diagnostics), "already holds an install of notes") {
		t.Fatalf("no warning: %s", diagText(a.Diagnostics))
	}
	install := h.fake.callsOf("install")[0]
	f := flags(install.argv)
	if f["--address"] != "" || f["--registry"] != "" || f["--set"] != "ADMIN_EMAIL=a@example.com" || install.argv[5] != "" || install.stdin != nil {
		t.Fatalf("install %q", install.argv)
	}
	// A fresh directory gets them.
	h.fake.reset()
	h.fake.set("new-status", reply{exit: 3, stdout: `{"format":1,"refused":{"reason":"/srv/notes holds no install","fix":"x","absent":true}}`})
	h.apply(nil, p.PlannedState, config)
	install = h.fake.callsOf("install")[0]
	if f := flags(install.argv); f["--address"] != "127.0.0.2" || f["--registry"] != "mirror.lan/notes" || install.argv[5] != "registry-ca secrets" {
		t.Fatalf("fresh install %q", install.argv)
	}
	// Another project's install, and a status that fails.
	for stdout, want := range map[string]string{
		`{"format":1,"name":"atlas","installed":{"version":"1","revision":1}}`: `holds an install of "atlas"`,
		``: "Reading the install directory",
	} {
		h.fake.reset()
		r := reply{stdout: stdout}
		if stdout == "" {
			r.exit = 1
		}
		h.fake.set("new-status", r)
		got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics)
		if !strings.Contains(got, want) || slices.Contains(h.fake.ops(), "install") {
			t.Errorf("got %q, calls %q", got, h.fake.ops())
		}
	}
}

// An imported state holds registry null until its first apply records the provider's: the plan
// says so (unknown), or OpenTofu refuses the apply's result as inconsistent with it.
func TestImportedPlanLeavesTheRegistryToTheApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	config := h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	p := h.plan(state, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	planned := h.attrs(p.PlannedState)["registry"]
	a := h.apply(state, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.attrs(a.NewState)["registry"]; planned.IsKnown() && !planned.Equal(got) {
		t.Fatalf("planned registry %v, applied %v", planned, got)
	}
}

// A null element is refused by key at validation; an unknown one (known at apply) is not, and a
// port out of range is refused at apply once known, before anything runs.
func TestNullMapElementsAndPortsKnownAtApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	null := tftypes.NewValue(tftypes.String, nil)
	config := h.dockerConfig(map[string]tftypes.Value{
		"inputs": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, map[string]tftypes.Value{"ADMIN_EMAIL": null}),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number},
			map[string]tftypes.Value{"web": tftypes.NewValue(tftypes.Number, nil)}),
	})
	got := errorsOf(h.validate(config))
	for _, want := range []string{"inputs.ADMIN_EMAIL is null", "ports.web is null"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
	h.fake.reset()
	config = h.dockerConfig(map[string]tftypes.Value{"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(70000)})})
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "70000 is not a port") {
		t.Fatalf("got %q", got)
	}
	if ops := h.fake.ops(); slices.Contains(ops, "install") {
		t.Fatalf("calls %q", ops)
	}
}

// The Cloudflare token a TLS mode adds is a declared secret (describe's tls_inputs): refused in
// inputs, and an upgrade does not re-send it without a new secret_inputs_wo_version.
func TestTLSModeTokenIsADeclaredSecret(t *testing.T) {
	t.Parallel()
	withTLS := func(d string) string {
		return strings.TrimSuffix(d, "}") + `,"tls_inputs":[{"key":"STOWAGE_CLOUDFLARE_API_TOKEN","secret":true,"tls_modes":["dns-cloudflare"]}]}`
	}
	h := newHarness(t, nil)
	h.fake.set("describe", reply{stdout: withTLS(describe1)})
	config := h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"STOWAGE_CLOUDFLARE_API_TOKEN": "x"})})
	if got := errorsOf(h.apply(nil, h.plan(nil, config).PlannedState, config).Diagnostics); !strings.Contains(got, "STOWAGE_CLOUDFLARE_API_TOKEN is a secret input") {
		t.Fatalf("in inputs: %q", got)
	}
	h = newHarness(t, nil)
	h.fake.set("describe", reply{stdout: withTLS(describe1)})
	secrets := map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"STOWAGE_CLOUDFLARE_API_TOKEN": "tok"}), "secret_inputs_wo_version": num(1)}
	state := h.create(h.dockerConfig(secrets))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: withTLS(describe2)})
	h.fake.set("kept-describe", reply{stdout: withTLS(describe1)})
	secrets["installer"] = str(image2)
	if d := errorsOf(h.update(state, h.dockerConfig(secrets)).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if files := untarAll(t, h.fake.callsOf("upgrade")[0].stdin); strings.Contains(files["answers"], "STOWAGE_CLOUDFLARE_API_TOKEN") {
		t.Fatalf("upgrade answers %q", files)
	}
}

func warningsOf(ds []*tfprotov6.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Severity == tfprotov6.DiagnosticSeverityWarning {
			b.WriteString(d.Summary + ": " + d.Detail + "\n")
		}
	}
	return b.String()
}

// An imported install's first apply compares the configuration with what the install records
// (status --json --values): matching, it runs nothing on the target; otherwise it re-runs with
// what differs alone — a recorded secret kept, an unrecorded one sent. plan_dry_run says which.
func TestImportedTakeOverComparesTheRecordedValues(t *testing.T) {
	t.Parallel()
	const recorded = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,` +
		`"inputs":["ADMIN_EMAIL","LEGACY","SMTP_PASSWORD"],"values":{"inputs":{"ADMIN_EMAIL":"a@example.com","LEGACY":"old"},"tls":"none","ports":{"web":8080}}}`
	cfg := func(email string, secrets map[string]string, extra map[string]tftypes.Value) tftypes.Value {
		v := map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": email}),
			"ports":            tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8080)}),
			"secret_inputs_wo": strMap(secrets), "secret_inputs_wo_version": num(1)}
		maps.Copy(v, extra)
		return newHarness(t, nil).dockerConfig(v)
	}
	takeOver := func(status string, config tftypes.Value) (*harness, *tfprotov6.ApplyResourceChangeResponse) {
		h := newHarness(t, nil)
		h.fake.set("status", reply{stdout: status})
		state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
		return h, h.update(state, config)
	}

	// Matching: nothing runs; LEGACY, recorded and left out, is never emptied.
	h, a := takeOver(recorded, cfg("a@example.com", map[string]string{"SMTP_PASSWORD": "pw"}, nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"status", "kept-describe", "status"}) || !contains(h.fake.calls[0].argv, "--values") {
		t.Fatalf("calls %q %q", got, h.fake.calls[0].argv)
	}
	if at := h.attrs(a.NewState); asString(t, at["installer"]) != image || at["registry"].IsNull() {
		t.Fatalf("state %v", at)
	}

	// Differing: the input that differs, and the secret the install does not record.
	h, a = takeOver(strings.Replace(recorded, `"LEGACY","SMTP_PASSWORD"`, `"LEGACY"`, 1), cfg("b@example.com", map[string]string{"SMTP_PASSWORD": "pw"}, nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	rerun := h.fake.callsOf("rerun")
	if len(rerun) != 1 || flags(rerun[0].argv)["--set"] != "ADMIN_EMAIL=b@example.com" || flags(rerun[0].argv)["--ports"] != "" ||
		untarAll(t, rerun[0].stdin)["answers"] != "SMTP_PASSWORD: \"pw\"\n" {
		t.Fatalf("re-run %q", h.fake.calls)
	}

	// The install serves a TLS mode the configuration leaves out: refused, nothing run.
	h, a = takeOver(strings.Replace(recorded, `"tls":"none"`, `"tls":"acme","domains":{"web":"notes.example.com"}`, 1), cfg("a@example.com", nil, nil))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, `the install serves a TLS mode: The install serves TLS mode acme and the configuration leaves tls out (loopback mode): give tls = "acme"`) ||
		len(h.fake.callsOf("rerun")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}

	// An installer before 0.38.0 refuses --values: status is read without it, and every value
	// given goes again.
	h = newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	h.fake.then("status", reply{exit: 1, stderr: "Error: unknown flag: --values\n"})
	a = h.update(state, cfg("a@example.com", nil, nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if st := h.fake.callsOf("status"); len(st) < 2 || !contains(st[0].argv, "--values") || contains(st[1].argv, "--values") ||
		len(h.fake.callsOf("rerun")) != 1 || flags(h.fake.callsOf("rerun")[0].argv)["--set"] != "ADMIN_EMAIL=a@example.com" {
		t.Fatalf("calls %q", h.fake.calls)
	}

	// plan_dry_run says what the take-over does.
	h = newHarness(t, nil)
	h.fake.set("status", reply{stdout: recorded})
	state = h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	p := h.plan(state, cfg("a@example.com", nil, map[string]tftypes.Value{"plan_dry_run": boolean(true)}))
	if d := errorsOf(p.Diagnostics); d != "" || !strings.Contains(warningsOf(p.Diagnostics), "Taking over the install: its values match") {
		t.Fatalf("plan: %s %s", d, warningsOf(p.Diagnostics))
	}
	p = h.plan(state, cfg("b@example.com", nil, map[string]tftypes.Value{"plan_dry_run": boolean(true)}))
	if w := warningsOf(p.Diagnostics); !strings.Contains(w, "its values differ: The first apply re-runs the install with what differs from what it records: input ADMIN_EMAIL.") ||
		strings.Contains(w, "b@example.com") {
		t.Fatalf("plan: %s", w)
	}
}

// The take-over's other paths: a release change beside it is dry-run against what the install
// records; a rollback of an imported install is refused at plan as at apply; a failed upgrade
// after the re-run saves every value the install holds; a secret no release declares still goes
// to the re-run (which refuses it); an install that records no TLS mode is not compared.
func TestImportedTakeOverPaths(t *testing.T) {
	t.Parallel()
	const recorded = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable",` +
		`"previous":{"version":"1.1.0","revision":1,"installer":"` + image2 + `"},"inputs":["ADMIN_EMAIL","LEGACY"],"values":{"inputs":{"ADMIN_EMAIL":"a@example.com","LEGACY":"old"},"tls":"none"}}`
	imported := func(status string) (*harness, *tfprotov6.DynamicValue) {
		h := newHarness(t, nil)
		h.fake.set("status", reply{stdout: status})
		return h, h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	}
	cfg := func(h *harness, extra map[string]tftypes.Value) tftypes.Value {
		v := map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}
		maps.Copy(v, extra)
		return h.dockerConfig(v)
	}

	// An upgrade beside a matching take-over is dry-run.
	h, state := imported(recorded)
	h.fake.set("describe", reply{stdout: describe2})
	p := h.plan(state, cfg(h, map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)), "plan_dry_run": boolean(true)}))
	if w := warningsOf(p.Diagnostics); strings.Contains(w, "No dry run of this plan") || !strings.Contains(w, "The release change the plan makes runs after it") || len(h.fake.callsOf("dry-upgrade")) != 1 {
		t.Fatalf("upgrade: %s %s %q", errorsOf(p.Diagnostics), w, h.fake.ops())
	}

	// A rollback of an imported install: refused at plan.
	h, state = imported(recorded)
	p = h.plan(state, cfg(h, map[string]tftypes.Value{"installer": str(image2), "allow_rollback": boolean(true), "plan_dry_run": boolean(true)}))
	if got := errorsOf(p.Diagnostics); !strings.Contains(got, "a rollback of an imported install") || len(h.fake.callsOf("dry-rollback")) != 0 ||
		strings.Contains(warningsOf(p.Diagnostics), "Taking over") {
		t.Fatalf("rollback: %q %q %s", got, h.fake.ops(), warningsOf(p.Diagnostics))
	}

	// An upgrade beside a re-run the take-over makes: no fetch (the re-run is certain), and the
	// report still says the release changes, never naming what only the new release declares.
	h, state = imported(recorded)
	p = h.plan(state, cfg(h, map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)), "plan_dry_run": boolean(true),
		"inputs":           strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"}),
		"secret_inputs_wo": strMap(map[string]string{"NEW_TOKEN": "t"}), "secret_inputs_wo_version": num(1)}))
	if w := warningsOf(p.Diagnostics); !strings.Contains(w, "its values differ") || !strings.Contains(w, "The release change the plan makes runs after it") ||
		!strings.Contains(w, "does not declare secret input NEW_TOKEN (the install records none): the new release's upgrade gives") || strings.Contains(w, "changes no release") {
		t.Fatalf("upgrade beside a re-run: %s %s", errorsOf(p.Diagnostics), w)
	}

	// A home-relative import is not read at plan: said.
	h = newHarness(t, nil)
	home := h.importState("app.example:notes/notes-stow").ImportedResources[0].State
	homeCfg := cfg(h, map[string]tftypes.Value{"plan_dry_run": boolean(true), "docker": h.block("docker", map[string]tftypes.Value{
		"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine)})})
	if p := h.plan(home, homeCfg); !strings.Contains(warningsOf(p.Diagnostics), "its values are not read at plan") {
		t.Fatalf("home: %s %s", warningsOf(p.Diagnostics), errorsOf(p.Diagnostics))
	}

	// The re-run ran, the upgrade failed: state holds the matching input too.
	h, state = imported(recorded)
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("upgrade", reply{exit: 1, stderr: "pull failed"})
	a := h.update(state, cfg(h, map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "LEGACY": "new"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "pull failed") || len(h.fake.callsOf("rerun")) != 1 {
		t.Fatalf("upgrade failure: %q %q", got, h.fake.ops())
	}
	if at := h.attrs(a.NewState); at["inputs"].IsNull() {
		t.Fatalf("partial state %v", at)
	} else {
		var in map[string]tftypes.Value
		if err := at["inputs"].As(&in); err != nil || len(in) != 2 {
			t.Fatalf("partial inputs %v %v", in, err)
		}
	}

	// A configuration leaving inputs out keeps them null in the partial state.
	h, state = imported(recorded)
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.set("upgrade", reply{exit: 1, stderr: "pull failed"})
	a = h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(9090)})}))
	if at := h.attrs(a.NewState); len(h.fake.callsOf("rerun")) != 1 || !at["inputs"].IsNull() {
		t.Fatalf("partial without inputs: %q %v", h.fake.ops(), at["inputs"])
	}

	// A secret no release declares goes to the re-run.
	h, state = imported(recorded)
	a = h.update(state, cfg(h, map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"SMTP_PASWORD": "x"}), "secret_inputs_wo_version": num(1)}))
	if r := h.fake.callsOf("rerun"); errorsOf(a.Diagnostics) != "" || len(r) != 1 || untarAll(t, r[0].stdin)["answers"] != "SMTP_PASWORD: \"x\"\n" {
		t.Fatalf("undeclared secret: %s %q", errorsOf(a.Diagnostics), h.fake.ops())
	}

	// No TLS mode recorded: the configuration's stands.
	h, state = imported(strings.Replace(recorded, `,"tls":"none"`, "", 1))
	if got := errorsOf(h.plan(state, cfg(h, map[string]tftypes.Value{"tls": str("acme"), "plan_dry_run": boolean(true)})).Diagnostics); strings.Contains(got, "serves loopback mode") {
		t.Fatalf("tls: %q", got)
	}
}

// Beside a release change the take-over's report names what the running release declares
// alone; an install recording no installer leaves the change open; a secret the install records
// is kept on the VM's upgrade too.
func TestImportedTakeOverReportBesideARelease(t *testing.T) {
	t.Parallel()
	const recorded = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,` +
		`"inputs":["ADMIN_EMAIL"],"values":{"inputs":{"ADMIN_EMAIL":"a@example.com"},"tls":"none"}}`
	for name, c := range map[string]struct{ status, want string }{
		"recorded":     {recorded, "The release change the plan makes runs after it, as planned."},
		"not recorded": {strings.Replace(recorded, `,"installer":"`+image+`"`, "", 1), "If the plan changes the release (the install records no installer to tell), that change runs after it."},
	} {
		h := newHarness(t, nil)
		h.fake.set("status", reply{stdout: c.status})
		state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
		p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)),
			"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com", "NEW_KEY": "k"})}))
		w := warningsOf(p.Diagnostics)
		if !strings.Contains(w, "what differs from what it records: input ADMIN_EMAIL.") || !strings.Contains(w, "The running release does not declare input NEW_KEY: the new release's upgrade gives what it declares, the re-run refuses the rest.") || !strings.Contains(w, c.want) {
			t.Errorf("%s: %s %s", name, errorsOf(p.Diagnostics), w)
		}
	}

	// vmSplit: a recorded secret the running release does not declare is kept on an upgrade.
	var diags diag.Diagnostics
	plan := installModel{Inputs: types.MapNull(types.StringType), Domains: types.MapNull(types.StringType), Ports: types.MapNull(types.Int64Type),
		TLS: types.StringNull(), SecretInputsWOVersion: types.Int64Value(1)}
	u := &update{plan: &plan, state: &plan, secrets: map[string]string{"OLD_KEY": "k", "NEW_TOKEN": "t"}, diags: &diags, adopted: &plan, recorded: []string{"OLD_KEY"}}
	now, later := vmSplit(context.Background(), u, &installer.Describe{}, true)
	if _, ok := later.SecretInputs["OLD_KEY"]; ok || now.SecretInputs["OLD_KEY"] != "" || later.SecretInputs["NEW_TOKEN"] != "t" {
		t.Errorf("now %v later %v", now.SecretInputs, later.SecretInputs)
	}
}

// The take-over's report when the new installer is read (no certain re-run): another release is an
// upgrade after it; the release the install runs, no release change — though the install records
// no installer. On the VM a domain of a route only the new release has waits for its upgrade.
func TestImportedTakeOverReportReadsTheNewRelease(t *testing.T) {
	t.Parallel()
	const recorded = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"channel":"stable","previous":null,` +
		`"inputs":["ADMIN_EMAIL"],"values":{"inputs":{"ADMIN_EMAIL":"a@example.com"},"tls":"none"}}`
	for name, c := range map[string]struct {
		describe, want string
	}{
		"same release": {describe1, "The plan changes no release."},
		"upgrade":      {describe2, "The release change the plan makes runs after it, as planned."},
	} {
		h := newHarness(t, nil)
		h.fake.set("status", reply{stdout: recorded})
		h.fake.set("describe", reply{stdout: c.describe})
		state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
		p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"plan_dry_run": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
		if w := warningsOf(p.Diagnostics); !strings.Contains(w, "its values match") || !strings.Contains(w, c.want) || len(h.fake.callsOf("fetch")) != 1 {
			t.Errorf("%s: %s %s %q", name, errorsOf(p.Diagnostics), w, h.fake.ops())
		}
	}

	var diags diag.Diagnostics
	domains := types.MapValueMust(types.StringType, map[string]attr.Value{"web": types.StringValue("a.example.com"), "api": types.StringValue("b.example.com")})
	plan := installModel{Inputs: types.MapNull(types.StringType), Domains: domains, Ports: types.MapNull(types.Int64Type), TLS: types.StringValue("acme")}
	was := plan
	was.Domains = types.MapValueMust(types.StringType, map[string]attr.Value{})
	u := &update{plan: &plan, state: &was, diags: &diags, adopted: &was}
	plan.Domains = types.MapValueMust(types.StringType, map[string]attr.Value{"web": types.StringValue("a.example.com"), "api": types.StringValue("b.example.com"), "db": types.StringValue("c.example.com")})
	now, later := vmSplit(context.Background(), u, &installer.Describe{Routes: []installer.Route{{Name: "web"}}, L4Routes: []installer.L4Route{{Name: "db"}}}, true)
	if now.Domains["web"] != "a.example.com" || now.Domains["db"] != "c.example.com" || now.Domains["api"] != "" || later.Domains["api"] != "b.example.com" {
		t.Errorf("now %v later %v", now.Domains, later.Domains)
	}
}

// The take-over's report names what the upgrade gives too, and says "differ" whenever anything
// is given; a domain for a route only the new release has is refused beside the upgrade.
func TestImportedTakeOverReportNamesTheUpgrade(t *testing.T) {
	t.Parallel()
	const recorded = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,` +
		`"inputs":["ADMIN_EMAIL"],"values":{"inputs":{"ADMIN_EMAIL":"a@example.com"},"tls":"acme","domains":{"web":"notes.example.com"}}}`
	newer := "registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("c", 64)
	plan := func(extra map[string]tftypes.Value) (*harness, *tfprotov6.PlanResourceChangeResponse) {
		h := newHarness(t, nil)
		h.fake.set("status", reply{stdout: recorded})
		h.fake.set("describe", reply{stdout: describe2})
		state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
		v := map[string]tftypes.Value{"installer": str(newer), "plan_dry_run": boolean(true), "tls": str("acme"),
			"domains": strMap(map[string]string{"web": "notes.example.com"}), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "NEW_KEY": "k"})}
		maps.Copy(v, extra)
		return h, h.plan(state, h.dockerConfig(v))
	}
	_, p := plan(nil)
	if w := warningsOf(p.Diagnostics); !strings.Contains(w, "its values differ") || !strings.Contains(w, "The upgrade gives what only the new release declares: input NEW_KEY.") {
		t.Errorf("upgrade keys: %s %s", errorsOf(p.Diagnostics), w)
	}
	h, p := plan(map[string]tftypes.Value{"domains": strMap(map[string]string{"web": "notes.example.com", "api": "api.example.com"})})
	if got := errorsOf(p.Diagnostics); !strings.Contains(got, "a domain added with a release change: domains.api names a route the running release does not have") ||
		len(h.fake.callsOf("dry-upgrade")) != 0 {
		t.Errorf("new route's domain: %q %q", got, h.fake.ops())
	}

	h = newHarness(t, nil)
	h.fake.set("status", reply{stdout: recorded})
	h.fake.set("describe", reply{stdout: describe2})
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(newer), "tls": str("acme"),
		"domains": strMap(map[string]string{"web": "notes.example.com", "api": "api.example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "a domain added with a release change") || len(h.fake.callsOf("rerun")) != 0 || len(h.fake.callsOf("upgrade")) != 0 {
		t.Errorf("apply, new route's domain: %q %q", got, h.fake.ops())
	}

	// Only what the running release does not declare, the fetch skipped: still "differ".
	var diags diag.Diagnostics
	u := &update{adopted: &installModel{}}
	adoptionReport(&diags, u, installer.Change{Inputs: map[string]string{"NEW_KEY": "k"}}, installer.Change{}, releaseChanges, &installer.Describe{}, true)
	if len(diags) != 1 || diags[0].Summary() != "Taking over the install: its values differ" {
		t.Errorf("undeclared alone: %v", diags)
	}
}
