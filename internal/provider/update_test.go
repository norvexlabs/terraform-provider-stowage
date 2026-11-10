package provider

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const (
	image2 = "registry.example.com/notes/notes-installer@" + digest2
	// describe2 is the release 2.0.0's offer: it adds NEW_KEY and the secret NEW_TOKEN.
	describe2 = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"2.0.0","revision":1},"channel":"stable","shapes":["docker"],
		"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"NEW_KEY"},{"key":"NEW_TOKEN","secret":true}]}`
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
	if vals["status"].IsKnown() || !vals["version"].IsKnown() {
		t.Fatalf("a re-run plans status unknown, the release kept: status %v version %v", vals["status"], vals["version"])
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

	h = newHarness(t, nil)
	h.fake.set("status", installing)
	config = h.dockerConfig(map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("20ms")})})
	a = h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "still installing when the create timeout ran out") {
		t.Fatalf("timeout: %q", got)
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
		"h:a/b:c/notes/notes-stow":          {"h", "a/b:c", "notes", "notes"},
	} {
		host, work, name, short, err := parseImportID(id)
		if err != nil || [4]string{host, work, name, short} != want {
			t.Errorf("%s: got %q %q %q %q %v", id, host, work, name, short, err)
		}
	}
	for _, id := range []string{"", "notes-stow", "/srv/notes/notes", "/srv/notes/-stow", "/srv/Notes/notes-stow", ":/srv/notes/notes-stow",
		"-oProxy=x:/srv/notes/notes-stow", "ops@h:/srv/notes/notes-stow", "/srv//notes-stow"} {
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
			f.set("kept-describe", reply{stdout: `{"format":1,"name":"atlas","short_name":"atlas","release":{"version":"1"}}`})
		}, change, `describes the project "atlas"`, false},
		"new installer of another project": {func(f *fake) {
			f.set("describe", reply{stdout: `{"format":1,"name":"atlas","short_name":"atlas","release":{"version":"1"}}`})
		}, map[string]tftypes.Value{"installer": str(image2)}, `describes the project "atlas"`, false},
		"no docker shape": {func(f *fake) {
			f.set("describe", reply{stdout: `{"format":1,"name":"notes","short_name":"notes","release":{"version":"2"},"shapes":["kubernetes"]}`})
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
	// Moved without a new version, its value would never be sent: refused before anything runs.
	h.fake.reset()
	got := errorsOf(h.update(state, h.dockerConfig(map[string]tftypes.Value{
		"secret_inputs_wo": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})).Diagnostics)
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
	h.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"installing":true,"install_failed":"provision hook exited 1"}`})
	config := h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"), "secrets_wo": str("export-" + secretVal),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	p := h.plan(nil, config)
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
	h.fake.set("new-status", reply{exit: 3})
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
