package provider

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

const (
	kitArmor    = "-----BEGIN AGE ENCRYPTED FILE-----\nKIT-s3cr3t\n"
	kitIssued   = `{"kit":"-----BEGIN AGE ENCRYPTED FILE-----\nKIT-s3cr3t\n","code":"CODE-s3cr3t","log":""}`
	kitHandoffs = `"handoffs":[{"name":"client","for":"notes-client","instance":"","ask":[{"key":"SITE","prompt":"the site","secret":false},{"key":"TOKEN","prompt":"the token","secret":true}],"gives":["CLIENT_ID","CLIENT_SECRET"],"check":true},` +
		`{"name":"front","for":"notes-edge","instance":"","ask":[],"gives":["EDGE_ID","KIT_TLS_CERT","KIT_TLS_KEY"],"check":false}]`
	answerSecret = "answer-s3cr3t"
)

// withHandoffs is a describe answer listing kitHandoffs.
func withHandoffs(d string) string {
	return strings.Replace(d, `"routes":`, kitHandoffs+`,"routes":`, 1)
}

// kitSecrets are the values no command line, state or diagnostic may hold.
var kitSecrets = []string{answerSecret, "KIT-s3cr3t", "CODE-s3cr3t"}

func noKitSecret(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range kitSecrets {
		if strings.Contains(s, secret) {
			t.Errorf("%s holds %s", what, secret)
		}
	}
}

// typed drives one resource type that is not an install's through the protocol, as OpenTofu does.
type typed struct {
	h    *harness
	name string
	sc   *tfprotov6.Schema
	typ  tftypes.Object
	etyp tftypes.Object // the ephemeral stowage_kit's
}

func typedOf(h *harness, name string) *typed {
	h.t.Helper()
	s, err := h.srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || s.ResourceSchemas[name] == nil || s.EphemeralResourceSchemas["stowage_kit"] == nil {
		h.t.Fatalf("schema of %s: %v", name, err)
	}
	sc := s.ResourceSchemas[name]
	return &typed{h: h, name: name, sc: sc, typ: sc.ValueType().(tftypes.Object),
		etyp: s.EphemeralResourceSchemas["stowage_kit"].ValueType().(tftypes.Object)}
}

func (k *typed) config(vals map[string]tftypes.Value) tftypes.Value { return fill(k.typ, vals) }

func (k *typed) block(name string, vals map[string]tftypes.Value) tftypes.Value {
	return fill(k.typ.AttributeTypes[name].(tftypes.Object), vals)
}

// dockerBlock is the Docker install of dockerConfig: /srv/notes on app.example.
func (k *typed) dockerBlock() tftypes.Value {
	return k.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine),
		"install_id": str("/srv/notes"), "short_name": str("notes")})
}

func (k *typed) validate(cfg tftypes.Value) []*tfprotov6.Diagnostic {
	k.h.t.Helper()
	c := k.h.dv(k.typ, cfg)
	resp, err := k.h.srv.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{TypeName: k.name, Config: &c,
		ClientCapabilities: &tfprotov6.ValidateResourceConfigClientCapabilities{WriteOnlyAttributesAllowed: true}})
	if err != nil {
		k.h.t.Fatal(err)
	}
	return resp.Diagnostics
}

func (k *typed) withoutWriteOnly(cfg tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{}
	if err := cfg.As(&vals); err != nil {
		k.h.t.Fatal(err)
	}
	vals = maps.Clone(vals)
	for _, a := range k.sc.Block.Attributes {
		if a.WriteOnly {
			vals[a.Name] = tftypes.NewValue(k.typ.AttributeTypes[a.Name], nil)
		}
	}
	return tftypes.NewValue(k.typ, vals)
}

func (k *typed) null() *tfprotov6.DynamicValue {
	d := k.h.dv(k.typ, tftypes.NewValue(k.typ, nil))
	return &d
}

func (k *typed) plan(prior *tfprotov6.DynamicValue, cfg tftypes.Value) *tfprotov6.PlanResourceChangeResponse {
	k.h.t.Helper()
	if prior == nil {
		prior = k.null()
	}
	pv, err := prior.Unmarshal(k.typ)
	if err != nil {
		k.h.t.Fatal(err)
	}
	c := k.h.dv(k.typ, cfg)
	proposed := k.h.dv(k.typ, k.h.proposedNew(k.sc, pv, k.withoutWriteOnly(cfg)))
	resp, err := k.h.srv.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: k.name, PriorState: prior, ProposedNewState: &proposed, Config: &c})
	if err != nil {
		k.h.t.Fatal(err)
	}
	return resp
}

func (k *typed) apply(prior, planned *tfprotov6.DynamicValue, cfg tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	k.h.t.Helper()
	if prior == nil {
		prior = k.null()
	}
	c := k.h.dv(k.typ, cfg)
	resp, err := k.h.srv.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName: k.name, PriorState: prior, PlannedState: planned, Config: &c})
	if err != nil {
		k.h.t.Fatal(err)
	}
	return resp
}

// planApply plans cfg against prior and applies it: the apply's answer.
func (k *typed) planApply(prior *tfprotov6.DynamicValue, cfg tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	k.h.t.Helper()
	p := k.plan(prior, cfg)
	if d := errorsOf(p.Diagnostics); d != "" {
		k.h.t.Fatalf("plan: %s", d)
	}
	a := k.apply(prior, p.PlannedState, cfg)
	if errorsOf(a.Diagnostics) == "" {
		k.consistent(p.PlannedState, a.NewState)
	}
	return a
}

// consistent fails unless the applied state holds each value the plan knew, as OpenTofu checks
// ("Provider produced inconsistent result after apply").
func (k *typed) consistent(planned, applied *tfprotov6.DynamicValue) {
	k.h.t.Helper()
	pv, err := planned.Unmarshal(k.typ)
	if err != nil {
		k.h.t.Fatal(err)
	}
	if pv.IsNull() {
		return
	}
	pa, aa := map[string]tftypes.Value{}, k.attrs(applied)
	if err := pv.As(&pa); err != nil {
		k.h.t.Fatal(err)
	}
	for name, v := range pa {
		if v.IsFullyKnown() && !v.Equal(aa[name]) {
			k.h.t.Errorf("%s.%s: planned %v, applied %v", k.name, name, v, aa[name])
		}
	}
}

func (k *typed) read(state *tfprotov6.DynamicValue) *tfprotov6.ReadResourceResponse {
	k.h.t.Helper()
	resp, err := k.h.srv.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{TypeName: k.name, CurrentState: state})
	if err != nil {
		k.h.t.Fatal(err)
	}
	return resp
}

func (k *typed) attrs(state *tfprotov6.DynamicValue) map[string]tftypes.Value {
	k.h.t.Helper()
	v, err := state.Unmarshal(k.typ)
	if err != nil {
		k.h.t.Fatal(err)
	}
	out := map[string]tftypes.Value{}
	if err := v.As(&out); err != nil {
		k.h.t.Fatal(err)
	}
	return out
}

func (k *typed) text(state *tfprotov6.DynamicValue) string {
	v, err := state.Unmarshal(k.typ)
	if err != nil {
		k.h.t.Fatal(err)
	}
	return v.String()
}

// open opens the ephemeral stowage_kit of id: its kit and code, "<null>" for none.
func (k *typed) open(id string) (string, string) {
	k.h.t.Helper()
	c := k.h.dv(k.etyp, fill(k.etyp, map[string]tftypes.Value{"id": str(id)}))
	resp, err := k.h.srv.OpenEphemeralResource(context.Background(), &tfprotov6.OpenEphemeralResourceRequest{TypeName: "stowage_kit", Config: &c})
	if err != nil || errorsOf(resp.Diagnostics) != "" {
		k.h.t.Fatalf("open: %v %s", err, diagText(resp.Diagnostics))
	}
	v, err := resp.Result.Unmarshal(k.etyp)
	if err != nil {
		k.h.t.Fatal(err)
	}
	m := map[string]tftypes.Value{}
	if err := v.As(&m); err != nil {
		k.h.t.Fatal(err)
	}
	return asString(k.h.t, m["kit"]), asString(k.h.t, m["code"])
}

// kitHarness is a Docker harness whose kept installer lists kitHandoffs and issues a kit.
func kitHarness(t *testing.T) (*harness, *typed) {
	h := newHarness(t, nil)
	h.fake.set("kept-describe", reply{stdout: withHandoffs(describe1)})
	h.fake.set("kit-create", reply{stdout: kitIssued})
	return h, typedOf(h, "stowage_kit")
}

func (k *typed) kitConfig(extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{
		"docker": k.dockerBlock(), "handoff": str("client"), "instance": str("site-2"),
		"answers":    strMap(map[string]string{"SITE": "lyon"}),
		"answers_wo": strMap(map[string]string{"TOKEN": answerSecret}),
	}
	maps.Copy(vals, extra)
	return k.config(vals)
}

// stowage_kit issues a kit by the kept installer, the request a file on standard input, keeps it
// in the process for the ephemeral stowage_kit alone, and issues again on a change, forgetting
// the kit it replaces.
func TestKitIssue(t *testing.T) {
	t.Parallel()
	h, k := kitHarness(t)
	a := k.planApply(nil, k.kitConfig(map[string]tftypes.Value{"expires": str("30m")}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ops := h.fake.ops(); strings.Join(ops, " ") != "kept-describe kit-create" {
		t.Fatalf("ops %q", ops)
	}
	c := h.fake.callsOf("kit-create")[0]
	noKitSecret(t, "the command line", strings.Join(c.argv, " "))
	if !contains(c.argv, "client") || !contains(c.argv, "--json") || h.fake.conns[0].Host != "app.example" {
		t.Errorf("argv %q conn %+v", c.argv, h.fake.conns)
	}
	var req installer.KitRequest
	if err := json.Unmarshal([]byte(untarAll(t, c.stdin)["request"]), &req); err != nil || req.Instance != "site-2" ||
		req.Answers["SITE"] != "lyon" || req.Answers["TOKEN"] != answerSecret || req.Expires != int64(30*time.Minute) {
		t.Fatalf("request %+v %v", req, err)
	}
	noKitSecret(t, "state", k.text(a.NewState))
	noKitSecret(t, "the diagnostics", diagText(a.Diagnostics))
	at := k.attrs(a.NewState)
	id := asString(t, at["id"])
	if len(id) != 32 || asString(t, at["for"]) != "notes-client" || !strings.Contains(at["gives"].String(), "CLIENT_SECRET") {
		t.Fatalf("state %v", at)
	}
	if kit, code := k.open(id); kit != kitArmor || code != "CODE-s3cr3t" {
		t.Fatalf("open %q %q", kit, code)
	}
	if kit, code := k.open("another"); kit != "<null>" || code != "<null>" {
		t.Fatalf("open another %q %q", kit, code)
	}
	// Nothing changed: no plan of a change, and a refresh runs nothing.
	h.fake.reset()
	if p := k.plan(a.NewState, k.kitConfig(map[string]tftypes.Value{"expires": str("30m")})); !samePlan(t, k, a.NewState, p.PlannedState) {
		t.Fatalf("a plan without a change: %s", diagText(p.Diagnostics))
	}
	if r := k.read(a.NewState); errorsOf(r.Diagnostics) != "" || len(h.fake.calls) != 0 {
		t.Fatalf("read %s %q", diagText(r.Diagnostics), h.fake.ops())
	}
	// A version bump issues again: a new id, the old kit forgotten.
	u := k.planApply(a.NewState, k.kitConfig(map[string]tftypes.Value{"expires": str("30m"), "version": num(2)}))
	id2 := asString(t, k.attrs(u.NewState)["id"])
	if d := errorsOf(u.Diagnostics); d != "" || id2 == id || len(h.fake.callsOf("kit-create")) != 1 {
		t.Fatalf("%s %s %q", d, id2, h.fake.ops())
	}
	if kit, _ := k.open(id); kit != "<null>" {
		t.Fatalf("the replaced kit is kept: %q", kit)
	}
	if kit, _ := k.open(id2); kit != kitArmor {
		t.Fatalf("the new kit %q", kit)
	}
	// Destroyed: forgotten.
	if d := k.apply(u.NewState, k.null(), k.config(nil)); errorsOf(d.Diagnostics) != "" {
		t.Fatal(diagText(d.Diagnostics))
	}
	if kit, _ := k.open(id2); kit != "<null>" {
		t.Fatalf("a destroyed kit is kept: %q", kit)
	}
}

// samePlan says the plan changes nothing of state.
func samePlan(t *testing.T, k *typed, state, planned *tfprotov6.DynamicValue) bool {
	t.Helper()
	if planned == nil {
		return false
	}
	return k.text(state) == k.text(planned)
}

// What the provider cannot keep, and a handoff the release does not issue, are refused before
// anything is issued; a failed issue says what it issued may be lost, quoting no secret.
func TestKitIssueRefusals(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		describe string
		extra    map[string]tftypes.Value
		want     string
	}{
		"no such handoff": {withHandoffs(describe1), map[string]tftypes.Value{"handoff": str("other")}, `issues no handoff "other"; it issues client, front`},
		"no handoffs":     {describe1, nil, "lists no handoffs; an installer made before Stowage " + installer.KitMinStowage},
		"secret answer": {withHandoffs(describe1), map[string]tftypes.Value{"answers": strMap(map[string]string{"SITE": "lyon", "TOKEN": answerSecret}), "answers_wo": strMap(map[string]string{})},
			"The handoff client asks TOKEN as a secret (the token), and answers is kept in state: give it in answers_wo."},
		"tls": {withHandoffs(describe1), map[string]tftypes.Value{"handoff": str("front")}, "The handoff front gives KIT_TLS_CERT, KIT_TLS_KEY"},
	} {
		h, k := kitHarness(t)
		h.fake.set("kept-describe", reply{stdout: c.describe})
		cfg := k.kitConfig(c.extra)
		p := k.plan(nil, cfg)
		a := k.apply(nil, p.PlannedState, cfg)
		if got := errorsOf(a.Diagnostics); !strings.Contains(got, c.want) || len(h.fake.callsOf("kit-create")) != 0 {
			t.Errorf("%s: %q %q", name, got, h.fake.ops())
		}
		noKitSecret(t, name, diagText(a.Diagnostics))
	}
	h, k := kitHarness(t)
	h.fake.set("kit-create", reply{exit: 1, stderr: "error: the handoff client's run did not finish, and no kit is written\n"})
	a := k.planApply(nil, k.kitConfig(nil))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "did not finish") || !strings.Contains(got, "what it issued is lost") {
		t.Fatalf("a failed issue: %q", got)
	}
	noKitSecret(t, "a failed issue", diagText(a.Diagnostics))
}

// The configuration's own checks, each boundary on both sides.
func TestKitValidation(t *testing.T) {
	t.Parallel()
	_, k := kitHarness(t)
	if d := errorsOf(k.validate(k.kitConfig(nil))); d != "" {
		t.Fatalf("the clean configuration: %s", d)
	}
	if d := errorsOf(k.validate(k.kitConfig(map[string]tftypes.Value{"expires": str("1ns")}))); d != "" {
		t.Fatalf("1ns: %s", d)
	}
	vm := k.block("vm", map[string]tftypes.Value{"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(hostLine), "short_name": str("notes")})
	if d := errorsOf(k.validate(k.kitConfig(map[string]tftypes.Value{"docker": k.block("docker", nil), "vm": vm}))); !strings.Contains(d, "One install, in one block") {
		t.Fatalf("two blocks: %s", d)
	}
	if d := errorsOf(k.validate(k.kitConfig(map[string]tftypes.Value{"docker": tftypes.NewValue(k.typ.AttributeTypes["docker"], nil), "vm": vm}))); d != "" {
		t.Fatalf("the vm alone: %s", d)
	}
	for name, c := range map[string]struct {
		extra map[string]tftypes.Value
		want  string
	}{
		"no block":      {map[string]tftypes.Value{"docker": tftypes.NewValue(k.typ.AttributeTypes["docker"], nil)}, "One install, in one block"},
		"no install_id": {map[string]tftypes.Value{"docker": k.block("docker", map[string]tftypes.Value{"short_name": str("notes")})}, "Missing docker.install_id"},
		"vm without user": {map[string]tftypes.Value{"docker": tftypes.NewValue(k.typ.AttributeTypes["docker"], nil),
			"vm": k.block("vm", map[string]tftypes.Value{"host": str("192.0.2.7"), "known_hosts_line": str(hostLine), "short_name": str("notes")})}, "Missing vm.user"},
		"k8s without namespace": {map[string]tftypes.Value{"docker": tftypes.NewValue(k.typ.AttributeTypes["docker"], nil),
			"kubernetes": k.block("kubernetes", map[string]tftypes.Value{"installer": str(image)})}, "Missing kubernetes.namespace"},
		"zero expires": {map[string]tftypes.Value{"expires": str("0s")}, "a kit expires after now"},
		"days":         {map[string]tftypes.Value{"expires": str("7d")}, "expires is not a duration"},
		"twice":        {map[string]tftypes.Value{"answers_wo": strMap(map[string]string{"SITE": "x"})}, "SITE is in both answers and answers_wo"},
		"relative id": {map[string]tftypes.Value{"docker": k.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"),
			"known_hosts_line": str(hostLine), "install_id": str("srv/notes"), "short_name": str("notes")})}, "install_id is not an install's id"},
		"short name": {map[string]tftypes.Value{"docker": k.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"),
			"known_hosts_line": str(hostLine), "install_id": str("/srv/notes"), "short_name": str("no/te")})}, "short_name is not an installer's short name"},
	} {
		if d := errorsOf(k.validate(k.kitConfig(c.extra))); !strings.Contains(d, c.want) {
			t.Errorf("%s: %q", name, d)
		}
	}
}

// A kit is taken by the install's re-run, its files on standard input; a kit expected and
// absent (lost) or without its code is refused before anything runs.
func TestKitTakeDocker(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	k := typedOf(h, "stowage_kit_take")
	cfg := func(extra map[string]tftypes.Value) tftypes.Value {
		vals := map[string]tftypes.Value{"docker": k.dockerBlock(), "kit_wo": str(kitArmor), "kit_code_wo": str("CODE-s3cr3t"), "kit_wo_version": str("k1")}
		maps.Copy(vals, extra)
		return k.config(vals)
	}
	a := k.planApply(nil, cfg(nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	r := h.fake.callsOf("rerun")
	if len(r) != 1 || strings.Join(h.fake.ops(), " ") != "status rerun" {
		t.Fatalf("ops %q", h.fake.ops())
	}
	files := untarAll(t, r[0].stdin)
	if files["kit"] != kitArmor || files["kit-code-file"] != "CODE-s3cr3t" || !contains(r[0].argv, "--no-input") || !contains(r[0].argv, "--json") {
		t.Fatalf("rerun %q %v", r[0].argv, files)
	}
	noKitSecret(t, "the command line", strings.Join(r[0].argv, " "))
	noKitSecret(t, "state", k.text(a.NewState))
	if asString(t, k.attrs(a.NewState)["id"]) != "k1" {
		t.Fatalf("state %v", k.attrs(a.NewState))
	}
	// The same version: nothing; another: taken again.
	h.fake.reset()
	if p := k.plan(a.NewState, cfg(nil)); !samePlan(t, k, a.NewState, p.PlannedState) {
		t.Fatal("a plan without a change")
	}
	if u := k.planApply(a.NewState, cfg(map[string]tftypes.Value{"kit_wo_version": str("k2")})); errorsOf(u.Diagnostics) != "" || len(h.fake.callsOf("rerun")) != 1 {
		t.Fatalf("%s %q", diagText(u.Diagnostics), h.fake.ops())
	}
	for name, c := range map[string]struct {
		extra map[string]tftypes.Value
		want  string
	}{
		"lost":    {map[string]tftypes.Value{"kit_wo": str(""), "kit_code_wo": str("")}, "Bump the stowage_kit's version"},
		"null":    {map[string]tftypes.Value{"kit_wo": tftypes.NewValue(tftypes.String, nil), "kit_code_wo": tftypes.NewValue(tftypes.String, nil)}, "kit_wo_version is given and kit_wo is null"},
		"no code": {map[string]tftypes.Value{"kit_code_wo": tftypes.NewValue(tftypes.String, nil)}, "kit_code_wo is not given"},
	} {
		h2 := newHarness(t, nil)
		k2 := typedOf(h2, "stowage_kit_take")
		c2 := cfg(c.extra)
		p := k2.plan(nil, c2)
		if got := errorsOf(k2.apply(nil, p.PlannedState, c2).Diagnostics); !strings.Contains(got, c.want) || len(h2.fake.calls) != 0 {
			t.Errorf("%s: %q %q", name, got, h2.fake.ops())
		}
	}
	// The installer refusing the kit says so, and nothing is recorded.
	h3 := newHarness(t, nil)
	h3.fake.set("rerun", reply{exit: 3, stdout: `{"format":1,"refused":{"reason":"--kit: nothing in the file is for this install","fix":""}}`, stderr: "error: --kit: nothing in the file is for this install\n"})
	k3 := typedOf(h3, "stowage_kit_take")
	if a := k3.planApply(nil, cfg(nil)); !strings.Contains(errorsOf(a.Diagnostics), "nothing in the file is for this install") || a.NewState != nil && !k3.isNull(a.NewState) {
		t.Fatalf("a refusal: %s", diagText(a.Diagnostics))
	}
}

func (k *typed) isNull(state *tfprotov6.DynamicValue) bool {
	v, err := state.Unmarshal(k.typ)
	return err == nil && v.IsNull()
}

// On Kubernetes, the kit goes to `k8s upgrade --kit` by the installer of the release it runs; a
// release running another installer is refused before anything is given.
func TestKitTakeK8s(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	k := typedOf(h, "stowage_kit_take")
	cfg := k.config(map[string]tftypes.Value{
		"kubernetes": k.block("kubernetes", map[string]tftypes.Value{"installer": str(image), "namespace": str("notes")}),
		"kit_wo":     str(kitArmor), "kit_code_wo": str("CODE-s3cr3t"), "kit_wo_version": str("k1")})
	if a := k.planApply(nil, cfg); errorsOf(a.Diagnostics) != "" {
		t.Fatal(diagText(a.Diagnostics))
	}
	up := h.fake.callsOf("k8s-upgrade")
	if len(up) != 1 {
		t.Fatalf("ops %q", h.fake.ops())
	}
	if files := untarAll(t, up[0].stdin); files["kit"] != kitArmor || files["kit-code-file"] != "CODE-s3cr3t" || !contains(up[0].argv, "--namespace") {
		t.Fatalf("upgrade %q %v", up[0].argv, files)
	}
	noKitSecret(t, "the command line", strings.Join(up[0].argv, " "))
	// The release upgraded after the take read it: read again just before the take.
	h3 := k8sHarness(t, nil)
	h3.fake.then("k8s-status", reply{stdout: statusK8s}, reply{stdout: statusK8s2})
	k3 := typedOf(h3, "stowage_kit_take")
	if got := errorsOf(k3.planApply(nil, cfg).Diagnostics); !strings.Contains(got, "runs notes 2.0.0") || len(h3.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("an upgrade meanwhile: %q %q", got, h3.fake.ops())
	}
	// The release gone between the take's first read and the one just before it.
	h6 := k8sHarness(t, nil)
	h6.fake.then("k8s-status", reply{stdout: statusK8s}, reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	k6 := typedOf(h6, "stowage_kit_take")
	if got := errorsOf(k6.planApply(nil, cfg).Diagnostics); !strings.Contains(got, "any more") || len(h6.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("a release gone: %q %q", got, h6.fake.ops())
	}
	// No release in the namespace: said as such, nothing run.
	h5 := k8sHarness(t, nil)
	h5.fake.set("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	k5 := typedOf(h5, "stowage_kit_take")
	if got := errorsOf(k5.planApply(nil, cfg).Diagnostics); !strings.Contains(got, "No release there") || len(h5.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("no release: %q %q", got, h5.fake.ops())
	}
	// Uninstalled with its history kept: nothing installed, refused as such.
	h4 := k8sHarness(t, nil)
	h4.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installed":{"version":"1.2.0","revision":1,"installer":"`+image+`"}`, `"installed":null`, 1)})
	k4 := typedOf(h4, "stowage_kit_take")
	if got := errorsOf(k4.planApply(nil, cfg).Diagnostics); !strings.Contains(got, "Nothing installed there") || len(h4.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("nothing installed: %q %q", got, h4.fake.ops())
	}
	h2 := k8sHarness(t, nil)
	h2.fake.set("k8s-status", reply{stdout: statusK8s2})
	k2 := typedOf(h2, "stowage_kit_take")
	a := k2.planApply(nil, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "The release runs another installer") || len(h2.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("another installer: %q %q", got, h2.fake.ops())
	}
}

// On a VM, a kit is issued by `sudo -n <short>-stow kit create --json`, the request on standard
// input, and taken by its install's re-run.
func TestKitVM(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("describe", reply{stdout: withHandoffs(vmDescribe)})
	f.set("kit-create", reply{stdout: kitIssued})
	k := typedOf(h, "stowage_kit")
	vm := func(k *typed) tftypes.Value {
		return k.block("vm", map[string]tftypes.Value{"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine), "short_name": str("notes")})
	}
	a := k.planApply(nil, k.config(map[string]tftypes.Value{"vm": vm(k), "handoff": str("client"),
		"answers": strMap(map[string]string{"SITE": "lyon"}), "answers_wo": strMap(map[string]string{"TOKEN": answerSecret})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	c := f.callsOf("kit-create")
	if len(c) != 1 || strings.Join(c[0].argv, " ") != "sudo -n notes-stow kit create client --json" || !strings.Contains(string(c[0].stdin), answerSecret) {
		t.Fatalf("kit create %q", f.ops())
	}
	if kit, _ := k.open(asString(t, k.attrs(a.NewState)["id"])); kit != kitArmor {
		t.Fatalf("open %q", kit)
	}
	tk := typedOf(h, "stowage_kit_take")
	if a := tk.planApply(nil, tk.config(map[string]tftypes.Value{"vm": vm(tk), "kit_wo": str(kitArmor), "kit_code_wo": str("CODE-s3cr3t"), "kit_wo_version": str("k1")})); errorsOf(a.Diagnostics) != "" {
		t.Fatal(diagText(a.Diagnostics))
	}
	in := f.callsOf("install")
	if len(in) != 1 || untarAll(t, in[0].stdin)["kit"] != kitArmor || untarAll(t, in[0].stdin)["kit-code-file"] != "CODE-s3cr3t" {
		t.Fatalf("install %q", f.ops())
	}
	noKitSecret(t, "the command line", strings.Join(in[0].argv, " "))
}

// checkHarness is a Docker harness whose kept installer lists kitHandoffs, for stowage_kit_check.
func checkHarness(t *testing.T) (*harness, *typed) {
	h := newHarness(t, nil)
	h.fake.set("kept-describe", reply{stdout: withHandoffs(describe1)})
	return h, typedOf(h, "stowage_kit_check")
}

func (k *typed) checkConfig(extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{"docker": k.dockerBlock(), "handoff": str("client"), "instance": str("site-2")}
	maps.Copy(vals, extra)
	return k.config(vals)
}

const (
	kitUnknown    = "client site-2: unknown\n"
	kitProvenLine = "client site-2: proven\n"
	kitNotProven  = "client site-2: not proven (its check, or starting it, exited 1)\n"
)

// stowage_kit_check tries `kit check` while it answers unknown or not at all, until a verdict or
// its timeout; not proven fails the apply by default; a refresh checks again while not proven.
func TestKitCheck(t *testing.T) {
	t.Parallel()
	h, k := checkHarness(t)
	h.fake.then("kit-check", reply{exit: 2, stdout: kitUnknown}, reply{exit: 255, stderr: "ssh: connect to host app.example port 22: Connection refused"},
		reply{exit: 0, stdout: kitProvenLine})
	a := k.planApply(nil, k.checkConfig(nil))
	at := k.attrs(a.NewState)
	if d := errorsOf(a.Diagnostics); d != "" || len(h.fake.callsOf("kit-check")) != 3 || asString(t, at["verdict"]) != "proven" || at["proven"].String() != "tftypes.Bool<\"true\">" {
		t.Fatalf("%s %q %v", d, h.fake.ops(), at)
	}
	if c := h.fake.callsOf("kit-check")[0]; !contains(c.argv, "site-2") || !contains(c.argv, "client") {
		t.Fatalf("argv %q", c.argv)
	}
	// Proven: a refresh runs nothing.
	h.fake.reset()
	if r := k.read(a.NewState); len(h.fake.calls) != 0 || errorsOf(r.Diagnostics) != "" {
		t.Fatalf("read %q", h.fake.ops())
	}

	// Not proven fails the apply, the state kept (tainted: the next apply checks again).
	h2, k2 := checkHarness(t)
	h2.fake.set("kit-check", reply{exit: 1, stdout: kitNotProven})
	a2 := k2.planApply(nil, k2.checkConfig(nil))
	if got := errorsOf(a2.Diagnostics); !strings.Contains(got, "The kit is not proven") || k2.isNull(a2.NewState) || asString(t, k2.attrs(a2.NewState)["verdict"]) != "not proven" {
		t.Fatalf("not proven: %q", got)
	}

	// fail_if_not_proven = false: a warning; a refresh checks once, and records the proof.
	h3, k3 := checkHarness(t)
	h3.fake.then("kit-check", reply{exit: 1, stdout: kitNotProven})
	a3 := k3.planApply(nil, k3.checkConfig(map[string]tftypes.Value{"fail_if_not_proven": boolean(false)}))
	if errorsOf(a3.Diagnostics) != "" || !strings.Contains(diagText(a3.Diagnostics), "The kit is not proven") || k3.attrs(a3.NewState)["proven"].String() != "tftypes.Bool<\"false\">" {
		t.Fatalf("not failing: %s", diagText(a3.Diagnostics))
	}
	h3.fake.reset()
	h3.fake.then("kit-check", reply{exit: 2, stdout: kitUnknown})
	r := k3.read(a3.NewState)
	if len(h3.fake.callsOf("kit-check")) != 1 || !strings.Contains(diagText(r.Diagnostics), "State keeps verdict not proven") || k3.attrs(r.NewState)["proven"].String() != "tftypes.Bool<\"false\">" {
		t.Fatalf("an unknown refresh: %s %q", diagText(r.Diagnostics), h3.fake.ops())
	}
	h3.fake.then("kit-check", reply{exit: 0, stdout: kitProvenLine})
	r = k3.read(a3.NewState)
	if errorsOf(r.Diagnostics) != "" || asString(t, k3.attrs(r.NewState)["verdict"]) != "proven" {
		t.Fatalf("a proving refresh: %s", diagText(r.Diagnostics))
	}

	// No verdict by the timeout: unknown, the last run's error said.
	h4, k4 := checkHarness(t)
	h4.fake.set("kit-check", reply{exit: 1, stderr: "sudo: a password is required"})
	a4 := k4.planApply(nil, k4.checkConfig(map[string]tftypes.Value{"timeout": str("20ms")}))
	if got := errorsOf(a4.Diagnostics); !strings.Contains(got, "gave no verdict in 20ms") || !strings.Contains(got, "a password is required") || len(h4.fake.callsOf("kit-check")) < 2 {
		t.Fatalf("timeout: %q %d", got, len(h4.fake.callsOf("kit-check")))
	}
	// A handoff without a check: refused before any check runs.
	h5, k5 := checkHarness(t)
	if got := errorsOf(k5.planApply(nil, k5.checkConfig(map[string]tftypes.Value{"handoff": str("front")})).Diagnostics); !strings.Contains(got, "The handoff front declares no check") || len(h5.fake.callsOf("kit-check")) != 0 {
		t.Fatalf("no check: %q %q", got, h5.fake.ops())
	}
	if d := errorsOf(k4.validate(k4.checkConfig(map[string]tftypes.Value{"timeout": str("0s")}))); !strings.Contains(d, "timeout is not a duration") {
		t.Fatalf("a zero timeout: %q", d)
	}
}

// A refresh shows an input whose recorded value changed since the refresh before (a kit taken)
// and differs from the configuration's: the next plan gives it back. The first refresh is the
// baseline; a value the installer always records otherwise (an empty one as its default), a file
// input recorded by its digest, and a key not reported are never a change; an update makes the
// next refresh a baseline again.
func TestRefreshShowsChangedInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "LEGACY": "file-body", "OTHER": "", "GONE": "g"})}))
	plain := h.fake.replies["status"] // the install made with the provider's settings
	status := func(admin string) {
		st := strings.Replace(plain.stdout, `"format":1,`, `"format":1,"values":{"inputs":{"ADMIN_EMAIL":"`+admin+`","LEGACY":"`+caDigest("file-body")+`","OTHER":"its-default"}},`, 1)
		h.fake.set("status", reply{stdout: strings.Replace(st, `"inputs":["ADMIN_EMAIL","SMTP_PASSWORD"]`, `"inputs":["ADMIN_EMAIL","LEGACY","OTHER","GONE"]`, 1)})
	}
	inputs := func(r *tfprotov6.ReadResourceResponse) string { return h.attrs(r.NewState)["inputs"].String() }
	status("a@b")
	r := h.read(state)
	if in := inputs(r); !strings.Contains(in, `"a@b"`) || !strings.Contains(in, `"file-body"`) || !strings.Contains(in, `"OTHER":tftypes.String<"">`) {
		t.Fatalf("the baseline: %s", in)
	}
	r = h.read(r.NewState) // the same record: nothing
	if in := inputs(r); strings.Contains(in, "its-default") {
		t.Fatalf("a default recorded for an empty value: %s", in)
	}
	status("kit@site")
	r = h.read(r.NewState)
	if in := inputs(r); !strings.Contains(in, `"kit@site"`) || strings.Contains(in, `"a@b"`) || !strings.Contains(in, `"file-body"`) || !strings.Contains(in, `"g"`) {
		t.Fatalf("a kit's value: %s", in)
	}
	// An update keeps what the install recorded at its end as the baseline: a change after it (a
	// kit taken later in that apply) shows at the next refresh, though none was saved between.
	status("c@d")
	u := h.update(r.NewState, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d", "LEGACY": "file-body", "OTHER": "", "GONE": "g"})}))
	if errorsOf(u.Diagnostics) != "" {
		t.Fatal(diagText(u.Diagnostics))
	}
	// The kit's value again, which the record before the update held: the update's baseline (c@d)
	// tells it from the record.
	status("kit@site")
	if in := inputs(h.read(u.NewState)); !strings.Contains(in, "kit@site") {
		t.Fatalf("a change after the update: %s", in)
	}
	if got := inputsChanged(types.MapNull(types.StringType), map[string]string{"A": "x"}, map[string]string{"A": "y"}); !got.IsNull() {
		t.Fatalf("a null map %v", got)
	}
}

// How the install is reached changes in place, issuing and taking nothing, the ids kept; which
// install it is replaces the resource; a directory holding no install is refused, not a crash.
func TestKitTargetChanges(t *testing.T) {
	t.Parallel()
	h, k := kitHarness(t)
	a := k.planApply(nil, k.kitConfig(nil))
	id := asString(t, k.attrs(a.NewState)["id"])
	moved := k.block("docker", map[string]tftypes.Value{"host": str("app.lan"), "user": str("ops"), "known_hosts_line": str(hostLine),
		"install_id": str("/srv/notes"), "short_name": str("notes"), "port": num(2222), "private_key_file": str("/keys/new")})
	h.fake.reset()
	p := k.plan(a.NewState, k.kitConfig(map[string]tftypes.Value{"docker": moved}))
	if len(p.RequiresReplace) != 0 || asString(t, k.attrsOf(p.PlannedState)["id"]) != id {
		t.Fatalf("a connection change: replace %v, id %v", p.RequiresReplace, k.attrsOf(p.PlannedState)["id"])
	}
	u := k.apply(a.NewState, p.PlannedState, k.kitConfig(map[string]tftypes.Value{"docker": moved}))
	if errorsOf(u.Diagnostics) != "" || len(h.fake.calls) != 0 || asString(t, k.attrs(u.NewState)["id"]) != id {
		t.Fatalf("an update of the connection ran %q: %s", h.fake.ops(), diagText(u.Diagnostics))
	}
	other := k.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine),
		"install_id": str("/srv/other"), "short_name": str("notes")})
	if p := k.plan(a.NewState, k.kitConfig(map[string]tftypes.Value{"docker": other})); len(p.RequiresReplace) == 0 {
		t.Fatal("another install does not replace the kit")
	}
	tk := typedOf(h, "stowage_kit_take")
	tcfg := func(b tftypes.Value) tftypes.Value {
		return tk.config(map[string]tftypes.Value{"docker": b, "kit_wo": str(kitArmor), "kit_code_wo": str("c"), "kit_wo_version": str("k1")})
	}
	ta := tk.planApply(nil, tcfg(tk.dockerBlock()))
	h.fake.reset()
	tm := tk.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine),
		"install_id": str("/srv/notes"), "short_name": str("notes"), "private_key_file": str("/keys/new")})
	if u := tk.planApply(ta.NewState, tcfg(tm)); errorsOf(u.Diagnostics) != "" || len(h.fake.calls) != 0 || asString(t, tk.attrs(u.NewState)["id"]) != "k1" {
		t.Fatalf("a take's connection change ran %q: %s", h.fake.ops(), diagText(u.Diagnostics))
	}
	h2, k2 := kitHarness(t)
	h2.fake.set("kept-describe", reply{exit: installer.NotInstalled, stderr: installer.NotInstalledMark + "\n"})
	if got := errorsOf(k2.planApply(nil, k2.kitConfig(nil)).Diagnostics); !strings.Contains(got, "No install there") || len(h2.fake.callsOf("kit-create")) != 0 {
		t.Fatalf("no install: %q %q", got, h2.fake.ops())
	}
	h2.fake.set("status", reply{exit: installer.NotInstalled, stderr: installer.NotInstalledMark + "\n"})
	tk2 := typedOf(h2, "stowage_kit_take")
	if got := errorsOf(tk2.planApply(nil, tk2.config(map[string]tftypes.Value{"docker": tk2.dockerBlock(), "kit_wo": str(kitArmor), "kit_code_wo": str("c"), "kit_wo_version": str("k1")})).Diagnostics); !strings.Contains(got, "No install there") || len(h2.fake.callsOf("rerun")) != 0 {
		t.Fatalf("a take with no install: %q %q", got, h2.fake.ops())
	}
}

func (k *typed) attrsOf(d *tfprotov6.DynamicValue) map[string]tftypes.Value { return k.attrs(d) }

// A kit issued again for an install that exists (kit_wo_version unknown at plan) is said to be
// given nothing, before the apply.
func TestKitWOVersionUnknownWarns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"kit_wo": str(kitArmor), "kit_code_wo": str("c"), "kit_wo_version": str("k1")}))
	p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"kit_wo_version": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
	if got := diagText(p.Diagnostics); !strings.Contains(got, "kit_wo_version is not known yet") {
		t.Fatalf("plan %s", got)
	}
	// An install made without a kit, given one now.
	h2 := newHarness(t, nil)
	plain := h2.create(h2.dockerConfig(nil))
	p = h2.plan(plain, h2.dockerConfig(map[string]tftypes.Value{"kit_wo_version": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
	if got := diagText(p.Diagnostics); !strings.Contains(got, "kit_wo_version is not known yet") {
		t.Fatalf("plan from no kit: %s", got)
	}
}

// A create keeps what the install recorded at its end as the baseline: a kit taken later in the
// same apply shows at the next refresh.
func TestCreateKeepsTheBaseline(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	base := h.fake.replies["status"].stdout
	values := func(admin string) reply {
		return reply{stdout: strings.Replace(base, `"format":1,`, `"format":1,"values":{"inputs":{"ADMIN_EMAIL":"`+admin+`"}},`, 1)}
	}
	h.fake.set("status", values("a@b"))
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})}))
	h.fake.set("status", values("kit@site"))
	if in := h.attrs(h.read(state).NewState)["inputs"].String(); !strings.Contains(in, "kit@site") {
		t.Fatalf("a kit taken after the create: %s", in)
	}
}

// On a VM, a take before its first install finished is refused (its seed's installer answers,
// and would run a first install); a create retried after its first install ran needs no kit,
// while one before it refuses a kit lost.
func TestKitVMBeforeAndAfterInstall(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"installed":{"version":"1.1.0","revision":1}`, `"installed":null`, 1)})
	tk := typedOf(h, "stowage_kit_take")
	vm := tk.block("vm", map[string]tftypes.Value{"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine), "short_name": str("notes")})
	if got := errorsOf(tk.planApply(nil, tk.config(map[string]tftypes.Value{"vm": vm, "kit_wo": str(kitArmor), "kit_code_wo": str("c"), "kit_wo_version": str("k1")})).Diagnostics); !strings.Contains(got, "Nothing installed there") || len(f.callsOf("install")) != 0 {
		t.Fatalf("a take before the first install: %q %q", got, f.ops())
	}
	lost := h.vmConfig("1.1.0", map[string]tftypes.Value{"kit_wo_version": str("k1")})
	if got := errorsOf(h.apply(nil, h.plan(nil, lost).PlannedState, lost).Diagnostics); !strings.Contains(got, "The kit is not here") || strings.Contains(got, "remove kit_wo_version") || len(f.callsOf("install")) != 0 {
		t.Fatalf("a kit lost before the first install (never taken: no way on without it): %q %q", got, f.ops())
	}
	// Installed already (its first boot's install, or an earlier create's): the VM cannot tell
	// whether it took this kit, so a kit lost is refused, naming both ways on.
	h2, f2 := newVMHarness(t)
	a := h2.apply(nil, h2.plan(nil, lost).PlannedState, lost)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Bump the stowage_kit's version") || !strings.Contains(got, "remove kit_wo_version") || len(f2.callsOf("install")) != 0 {
		t.Fatalf("a kit lost on an installed VM: %q %q", got, f2.ops())
	}
	// A first install that failed (perhaps after taking the kit): the remove hint too.
	hf, ff := newVMHarness(t)
	ff.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"installed":{"version":"1.1.0","revision":1}`, `"installed":null,"install_failed":"the hook failed"`, 1)})
	if got := errorsOf(hf.apply(nil, hf.plan(nil, lost).PlannedState, lost).Diagnostics); !strings.Contains(got, "remove kit_wo_version") || len(ff.callsOf("install")) != 0 {
		t.Fatalf("a failed first install: %q %q", got, ff.ops())
	}
	// An earlier create's first install still running: apply again once it ends.
	h3, f3 := newVMHarness(t)
	f3.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"installed":{"version":"1.1.0","revision":1}`, `"installed":null,"installing":true`, 1)})
	a = h3.apply(nil, h3.plan(nil, lost).PlannedState, lost)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "The first install is still running") || !strings.Contains(got, "remove kit_wo_version") || !strings.Contains(got, "if no run is under way") || strings.Contains(got, "needing no kit") || len(f3.callsOf("install")) != 0 {
		t.Fatalf("a first install running: %q %q", got, f3.ops())
	}
}

// A Docker create retried while an earlier create's first install is marked as running is
// refused, naming the ways on (apply again once it ends; a new kit when no run is under way); with an unfinished one (failed), a kit lost is refused, the warning not
// saying a kit is given.
func TestDockerRetryWithALostKit(t *testing.T) {
	t.Parallel()
	lost := func(h *harness) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"kit_wo_version": str("k1")})
	}
	h := newHarness(t, nil)
	h.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":null,"installing":true,"address":"127.0.0.1"}`})
	if got := errorsOf(h.apply(nil, h.plan(nil, lost(h)).PlannedState, lost(h)).Diagnostics); !strings.Contains(got, "The first install is still running") || !strings.Contains(got, "needing no kit") ||
		!strings.Contains(got, "If no run is under way") || strings.Contains(got, "remove kit_wo_version") || len(h.fake.callsOf("install")) != 0 {
		t.Fatalf("running: %q %q", got, h.fake.ops())
	}
	// Another project's install there: said first, never a new kit asked for.
	h3 := newHarness(t, nil)
	h3.fake.set("new-status", reply{stdout: `{"format":1,"name":"other","installed":null,"address":"127.0.0.1"}`})
	if got := errorsOf(h3.apply(nil, h3.plan(nil, lost(h3)).PlannedState, lost(h3)).Diagnostics); !strings.Contains(got, "Another project's install") || strings.Contains(got, "The kit is not here") {
		t.Fatalf("another project: %q", got)
	}
	h4 := newHarness(t, nil)
	h4.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":null,"address":"10.0.0.9"}`})
	if got := errorsOf(h4.apply(nil, h4.plan(nil, lost(h4)).PlannedState, lost(h4)).Diagnostics); !strings.Contains(got, "The install there was made otherwise") || strings.Contains(got, "The kit is not here") {
		t.Fatalf("made otherwise: %q", got)
	}
	h2 := newHarness(t, nil)
	h2.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":null,"install_failed":"the hook failed","address":"127.0.0.1"}`})
	a := h2.apply(nil, h2.plan(nil, lost(h2)).PlannedState, lost(h2))
	if got := diagText(a.Diagnostics); !strings.Contains(errorsOf(a.Diagnostics), "The kit is not here") || strings.Contains(got, "is given, as nothing is installed yet") || len(h2.fake.callsOf("install")) != 0 {
		t.Fatalf("unfinished: %s %q", got, h2.fake.ops())
	}
}

// A VM update that re-runs the release it runs keeps what the VM records at its end as the
// baseline: a kit taken after it shows at the next refresh.
func TestVMUpdateKeepsTheBaseline(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	values := func(admin string) reply {
		return reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"values":{"inputs":{"ADMIN_EMAIL":"`+admin+`"}}`)}
	}
	f.set("status", values("a@b"))
	state := h.create(h.vmConfig("1.1.0", nil))
	f.set("status", values("c@d"))
	u := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if errorsOf(u.Diagnostics) != "" || len(f.callsOf("install")) < 2 {
		t.Fatalf("update: %s %q", diagText(u.Diagnostics), f.ops())
	}
	f.set("status", values("a@b")) // a kit gives back the value before the update
	if in := h.attrs(h.read(u.NewState).NewState)["inputs"].String(); !strings.Contains(in, "a@b") {
		t.Fatalf("a kit taken after the update: %s", in)
	}
}

// A Kubernetes create taking over a release there names the kit it does not give.
func TestK8sTakeOverNamesTheKit(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	cfg := h.k8sConfig(map[string]tftypes.Value{"kit_wo": str(kitArmor), "kit_code_wo": str("c"), "kit_wo_version": str("k1")})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := diagText(a.Diagnostics); errorsOf(a.Diagnostics) != "" || !strings.Contains(got, "kit_wo with kit_code_wo, as a release is there") {
		t.Fatalf("%s", got)
	}
	if in := h.fake.callsOf("k8s-upgrade"); len(in) > 0 && untarAll(t, in[0].stdin)["kit"] != "" {
		t.Fatalf("the take-over gave the kit: %q", h.fake.ops())
	}
}
