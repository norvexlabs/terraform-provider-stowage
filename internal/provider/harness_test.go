package provider

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const (
	digest    = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	digest2   = "sha256:fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	image     = "registry.example.com/notes/notes-installer@" + digest
	edKey     = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	hostLine  = "app.example ssh-ed25519 " + edKey
	secretVal = "s3cr3t-NEVER-IN-ARGV-OR-STATE"
)

type call struct {
	argv  []string
	stdin []byte
	unset []string // what the runner left out of the environment (WithoutEnv), for this call
}

// op names a call by the script it runs and the verb it asks.
func (c call) op() string {
	if len(c.argv) < 3 {
		return "?"
	}
	switch c.argv[2] {
	case installer.FetchScript:
		return "fetch"
	case installer.ToolsScript:
		return "tools"
	case installer.ResolveScript:
		return "resolve"
	case installer.RunLoginScript:
		return "install" // vm seed, the one program run so
	case installer.K8sScript:
		// sh -c K8sScript sh <dir> <kubeconfig> <files> <host> <user> sh ./<name>.sh <args…>
		switch {
		case len(c.argv) > 12 && c.argv[11] == "k8s":
			return "k8s-" + c.argv[12]
		case len(c.argv) > 11:
			return "k8s-" + c.argv[11]
		}
		return "?"
	case installer.RunScript:
		switch {
		case len(c.argv) > 8 && c.argv[8] == "status":
			return "new-status" // the fetched installer's, before a create
		case contains(c.argv, "describe"):
			return "describe"
		case contains(c.argv, "upgrade"):
			return "upgrade"
		}
		return "install"
	case installer.StowScript:
		switch verb := c.argv[9]; {
		case strings.HasPrefix(verb, "-"):
			return "rerun"
		case verb == "describe":
			return "kept-describe"
		default:
			return verb
		}
	}
	return "?"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type reply struct {
	stdout string
	stderr string
	exit   int
	// block: the call waits for its context to end, then fails as a run cut by it does.
	block bool
}

// fake is a Runner answering each operation with its next queued reply, else its set reply,
// recording every call and the connection it was made for. A call whose context is done fails
// as a real run would.
type fake struct {
	mu      sync.Mutex
	calls   []call
	conns   []Connection
	replies map[string]reply
	queue   map[string][]reply
	// lenient answers a call whose context is done, but for a blocking reply: a test of a
	// timeout then reaches the step it tests however slow the host is before it.
	lenient bool
}

const describe1 = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.2.0","revision":1},"channel":"stable","shapes":["docker"],"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"LEGACY"}],"first_install_only":["address","registry","registry_ca","channel"],"routes":[{"name":"web","loopback_port":8080}],"l4_routes":[]}`

func newFake() *fake {
	return &fake{queue: map[string][]reply{}, replies: map[string]reply{
		"fetch":         {},
		"resolve":       {stdout: "/home/ops\n"},
		"describe":      {stdout: describe1},
		"kept-describe": {stdout: describe1},
		"install":       {stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`},
		"rerun":         {stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`},
		"upgrade":       {stdout: `{"format":1,"result":"done","release":{"version":"2.0.0","revision":1}}`},
		"rollback":      {stdout: `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`},
		"new-status":    {exit: 3, stdout: `{"format":1,"refused":{"reason":"/srv/notes holds no install","fix":"sh notes.sh installs it","absent":true}}`, stderr: "error: /srv/notes holds no install\n"},
		"status":        {stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,"inputs":["ADMIN_EMAIL","SMTP_PASSWORD"]}`},
		"uninstall":     {stdout: `{"format":1,"result":"done"}`},
	}}
}

// then queues replies to op, given before its set reply.
func (f *fake) then(op string, rs ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[op] = append(f.queue[op], rs...)
}

// set replaces op's set reply.
func (f *fake) set(op string, r reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[op] = r
}

// callsOf is the calls made to op.
func (f *fake) callsOf(op string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.op() == op {
			out = append(out, c)
		}
	}
	return out
}

// reset forgets the calls made.
func (f *fake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.conns = nil, nil
}

func (f *fake) factory(c Connection) (target.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, c)
	return f, nil
}

// filtered is the fake as an EnvFilter's result: its calls record the keys left out.
type filtered struct {
	f     *fake
	unset []string
}

// WithoutEnv makes the fake an EnvFilter, as the runner on this machine is.
func (f *fake) WithoutEnv(keys []string) target.Runner { return filtered{f, keys} }

// WithoutEnv adds keys to those left out, as Local's does.
func (r filtered) WithoutEnv(keys []string) target.Runner {
	return filtered{r.f, append(append([]string(nil), r.unset...), keys...)}
}

func (r filtered) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	return r.f.run(ctx, argv, stdin, r.unset)
}

func (f *fake) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	return f.run(ctx, argv, stdin, nil)
}

func (f *fake) run(ctx context.Context, argv []string, stdin []byte, unset []string) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := call{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...), unset: unset}
	f.calls = append(f.calls, c)
	if err := ctx.Err(); err != nil && !f.lenient {
		return nil, nil, -1, err
	}
	r, ok := f.replies[c.op()]
	if q := f.queue[c.op()]; len(q) > 0 {
		r, ok, f.queue[c.op()] = q[0], true, q[1:]
	}
	if !ok {
		return nil, []byte("unexpected call"), 99, nil
	}
	if r.block {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return nil, nil, -1, ctx.Err()
	}
	return []byte(r.stdout), []byte(r.stderr), r.exit, nil
}

func (f *fake) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.op())
	}
	return out
}

// harness drives the provider through the plugin protocol, as OpenTofu does.
type harness struct {
	t *testing.T
	// failedPlans are the planned states of plans that failed, which apply refuses.
	mu          sync.Mutex
	failedPlans map[*tfprotov6.DynamicValue]bool
	srv         tfprotov6.ProviderServer
	typ         tftypes.Object
	ptyp        tftypes.Object
	fake        *fake
	schema      *tfprotov6.Schema
	tmp         string // where Kubernetes runs make their private directories
}

func newHarness(t *testing.T, providerConfig map[string]tftypes.Value) *harness {
	t.Helper()
	f := newFake()
	// Kubernetes runs make their private directories here, and leave none.
	tmp := t.TempDir()
	t.Cleanup(func() { noneLeft(t, tmp) })
	srv, err := providerserver.NewProtocol6WithError(newIn("test", f.factory, time.Millisecond, tmp, t.TempDir())())()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || len(s.Diagnostics) > 0 {
		t.Fatalf("schema: %v %v", err, s.Diagnostics)
	}
	h := &harness{t: t, srv: srv, fake: f, schema: s.ResourceSchemas["stowage_install"], tmp: tmp}
	h.typ = h.schema.ValueType().(tftypes.Object)
	h.ptyp = s.Provider.ValueType().(tftypes.Object)
	cfg := h.dv(h.ptyp, fill(h.ptyp, providerConfig))
	resp, err := srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &cfg})
	if err != nil || len(resp.Diagnostics) > 0 {
		t.Fatalf("configure: %v %s", err, diagText(resp.Diagnostics))
	}
	return h
}

// noneLeft fails t when dir holds anything: a run's private directory (a kubeconfig) left.
func noneLeft(t *testing.T, dir string) {
	t.Helper()
	if left, err := os.ReadDir(dir); err != nil || len(left) > 0 {
		t.Errorf("left in %s: %v %v", dir, left, err)
	}
}

// fill is an object of typ with vals, every other attribute null.
func fill(typ tftypes.Object, vals map[string]tftypes.Value) tftypes.Value {
	all := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		if v, ok := vals[name]; ok {
			all[name] = v
		} else {
			all[name] = tftypes.NewValue(at, nil)
		}
	}
	return tftypes.NewValue(typ, all)
}

func (h *harness) dv(typ tftypes.Type, v tftypes.Value) tfprotov6.DynamicValue {
	h.t.Helper()
	d, err := tfprotov6.NewDynamicValue(typ, v)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) block(name string, vals map[string]tftypes.Value) tftypes.Value {
	return fill(h.typ.AttributeTypes[name].(tftypes.Object), vals)
}

func str(s string) tftypes.Value   { return tftypes.NewValue(tftypes.String, s) }
func boolean(b bool) tftypes.Value { return tftypes.NewValue(tftypes.Bool, b) }
func num(n int64) tftypes.Value    { return tftypes.NewValue(tftypes.Number, n) }
func strMap(m map[string]string) tftypes.Value {
	vals := map[string]tftypes.Value{}
	for k, v := range m {
		vals[k] = str(v)
	}
	return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, vals)
}

// dockerConfig is a resource configuration on a Docker host over SSH, with extra attributes.
func (h *harness) dockerConfig(extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{
		"installer": str(image),
		"docker": h.block("docker", map[string]tftypes.Value{
			"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "work_dir": str("/srv"),
		}),
	}
	for k, v := range extra {
		vals[k] = v
	}
	return fill(h.typ, vals)
}

func diagText(ds []*tfprotov6.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString(d.Summary + ": " + d.Detail + "\n")
	}
	return b.String()
}

func errorsOf(ds []*tfprotov6.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		if d.Severity == tfprotov6.DiagnosticSeverityError {
			b.WriteString(d.Summary + ": " + d.Detail + "\n")
		}
	}
	return b.String()
}

func (h *harness) validate(config tftypes.Value) []*tfprotov6.Diagnostic {
	h.t.Helper()
	cfg := h.dv(h.typ, config)
	resp, err := h.srv.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName: "stowage_install", Config: &cfg,
		ClientCapabilities: &tfprotov6.ValidateResourceConfigClientCapabilities{WriteOnlyAttributesAllowed: true},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp.Diagnostics
}

// withoutWriteOnly is config with each write-only attribute null, as OpenTofu proposes a new
// state.
func (h *harness) withoutWriteOnly(config tftypes.Value) tftypes.Value {
	orig := map[string]tftypes.Value{}
	if err := config.As(&orig); err != nil {
		h.t.Fatal(err)
	}
	vals := maps.Clone(orig) // As shares the value's own map
	for _, a := range h.schema.Block.Attributes {
		if a.WriteOnly {
			vals[a.Name] = tftypes.NewValue(h.typ.AttributeTypes[a.Name], nil)
		}
	}
	return tftypes.NewValue(h.typ, vals)
}

// plan plans config against prior (nil: a create); proposed is config without its write-only
// values.
func (h *harness) plan(prior *tfprotov6.DynamicValue, config tftypes.Value) *tfprotov6.PlanResourceChangeResponse {
	h.t.Helper()
	if prior == nil {
		p := h.dv(h.typ, tftypes.NewValue(h.typ, nil))
		prior = &p
	}
	cfg := h.dv(h.typ, config)
	proposed := h.dv(h.typ, h.proposedNew(h.value(prior), h.withoutWriteOnly(config)))
	resp, err := h.srv.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: "stowage_install", PriorState: prior, ProposedNewState: &proposed, Config: &cfg,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if errorsOf(resp.Diagnostics) != "" && resp.PlannedState != nil {
		h.mu.Lock()
		if h.failedPlans == nil {
			h.failedPlans = map[*tfprotov6.DynamicValue]bool{}
		}
		h.failedPlans[resp.PlannedState] = true // OpenTofu applies no plan that failed
		h.mu.Unlock()
	}
	return resp
}

// proposedNew is the proposed new state as OpenTofu makes it: config, with prior's value for each
// computed attribute config leaves null, at the top and in each nested block both hold.
func (h *harness) proposedNew(prior, config tftypes.Value) tftypes.Value {
	if prior.IsNull() || config.IsNull() {
		return config
	}
	v, err := mergeComputed(h.schema.Block, prior, config)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

// mergeComputed is config with prior's computed values, as proposedNew says; a nesting the harness
// does not model (set or map blocks, nested attributes but single) is an error, never a silent
// proposal OpenTofu would not make.
func mergeComputed(b *tfprotov6.SchemaBlock, prior, config tftypes.Value) (tftypes.Value, error) {
	var pv, cv map[string]tftypes.Value
	if prior.As(&pv) != nil || config.As(&cv) != nil {
		return config, nil
	}
	out := map[string]tftypes.Value{}
	for k, v := range cv {
		out[k] = v
	}
	for _, a := range b.Attributes {
		switch {
		case a.NestedType != nil && a.NestedType.Nesting != tfprotov6.SchemaObjectNestingModeSingle:
			return config, fmt.Errorf("the harness models no %v nested attribute (%s)", a.NestedType.Nesting, a.Name)
		case a.Computed && cv[a.Name].IsNull():
			out[a.Name] = pv[a.Name]
		case a.NestedType != nil && !cv[a.Name].IsNull() && !pv[a.Name].IsNull():
			m, err := mergeComputed(&tfprotov6.SchemaBlock{Attributes: a.NestedType.Attributes}, pv[a.Name], cv[a.Name])
			if err != nil {
				return config, err
			}
			out[a.Name] = m
		}
	}
	for _, nb := range b.BlockTypes {
		c, pr := cv[nb.TypeName], pv[nb.TypeName]
		switch nb.Nesting {
		case tfprotov6.SchemaNestedBlockNestingModeSingle, tfprotov6.SchemaNestedBlockNestingModeList:
		default:
			return config, fmt.Errorf("the harness models no %v block (%s)", nb.Nesting, nb.TypeName)
		}
		if c.IsNull() || pr.IsNull() || !c.IsKnown() || !pr.IsKnown() {
			continue
		}
		switch nb.Nesting {
		case tfprotov6.SchemaNestedBlockNestingModeSingle:
			m, err := mergeComputed(nb.Block, pr, c)
			if err != nil {
				return config, err
			}
			out[nb.TypeName] = m
		case tfprotov6.SchemaNestedBlockNestingModeList:
			// Elements correspond by index, as OpenTofu proposes them.
			var ce, pe []tftypes.Value
			if c.As(&ce) == nil && pr.As(&pe) == nil {
				merged := make([]tftypes.Value, len(ce))
				for i := range ce {
					merged[i] = ce[i]
					if i < len(pe) {
						m, err := mergeComputed(nb.Block, pe[i], ce[i])
						if err != nil {
							return config, err
						}
						merged[i] = m
					}
				}
				out[nb.TypeName] = tftypes.NewValue(c.Type(), merged)
			}
		}
	}
	return tftypes.NewValue(config.Type(), out), nil
}

func (h *harness) apply(prior *tfprotov6.DynamicValue, planned *tfprotov6.DynamicValue, config tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	h.t.Helper()
	if prior == nil {
		p := h.dv(h.typ, tftypes.NewValue(h.typ, nil))
		prior = &p
	}
	// The final plan, as OpenTofu makes it before an apply: config known now. Its errors are the
	// apply's, nothing applied; a value the first plan knew must stand in it.
	validated := h.validate(config)
	if errorsOf(validated) != "" {
		return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: validated, NewState: prior}
	}
	final := h.plan(prior, config)
	if errorsOf(final.Diagnostics) != "" {
		return &tfprotov6.ApplyResourceChangeResponse{Diagnostics: append(validated, final.Diagnostics...), NewState: prior}
	}
	h.mu.Lock()
	failed := h.failedPlans[planned]
	h.mu.Unlock()
	switch {
	case planned == nil:
		return &tfprotov6.ApplyResourceChangeResponse{NewState: prior, Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError, Summary: "harness: no plan to apply"}}}
	case failed:
		return &tfprotov6.ApplyResourceChangeResponse{NewState: prior, Diagnostics: []*tfprotov6.Diagnostic{{
			Severity: tfprotov6.DiagnosticSeverityError, Summary: "harness: the plan applied failed", Detail: "OpenTofu applies no plan that failed."}}}
	}
	for _, e := range h.inconsistencies(planned, final.PlannedState, false) {
		h.t.Errorf("inconsistent final plan: %s", e)
	}
	cfg := h.dv(h.typ, config)
	resp, err := h.srv.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName: "stowage_install", PriorState: prior, PlannedState: final.PlannedState, Config: &cfg,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.consistent(final.PlannedState, resp.NewState, errorsOf(resp.Diagnostics) == "")
	resp.Diagnostics = append(append(validated, final.Diagnostics...), resp.Diagnostics...) // OpenTofu shows them all
	return resp
}

// create plans and applies config, failing the test on an error.
func (h *harness) create(config tftypes.Value) *tfprotov6.DynamicValue {
	h.t.Helper()
	if d := errorsOf(h.validate(config)); d != "" {
		h.t.Fatalf("validate: %s", d)
	}
	p := h.plan(nil, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		h.t.Fatalf("plan: %s", d)
	}
	a := h.apply(nil, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		h.t.Fatalf("apply: %s", d)
	}
	return a.NewState
}

// destroy applies a destroy of state.
func (h *harness) destroy(state *tfprotov6.DynamicValue) *tfprotov6.ApplyResourceChangeResponse {
	h.t.Helper()
	null := h.dv(h.typ, tftypes.NewValue(h.typ, nil))
	resp, err := h.srv.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName: "stowage_install", PriorState: state, PlannedState: &null, Config: &null,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	// A destroy that succeeds returns no state; one that fails, a wholly known one.
	h.consistent(&null, resp.NewState, errorsOf(resp.Diagnostics) == "")
	return resp
}

// update plans config against state and applies it, failing the test on a plan error.
func (h *harness) update(state *tfprotov6.DynamicValue, config tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	h.t.Helper()
	p := h.plan(state, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		h.t.Fatalf("plan: %s", d)
	}
	return h.apply(state, p.PlannedState, config)
}

func (h *harness) read(state *tfprotov6.DynamicValue) *tfprotov6.ReadResourceResponse {
	h.t.Helper()
	resp, err := h.srv.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{TypeName: "stowage_install", CurrentState: state})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

// attrs decodes a state into its attributes.
func (h *harness) attrs(state *tfprotov6.DynamicValue) map[string]tftypes.Value {
	h.t.Helper()
	v := h.value(state)
	out := map[string]tftypes.Value{}
	if v.IsNull() {
		return nil
	}
	if err := v.As(&out); err != nil {
		h.t.Fatal(err)
	}
	return out
}

func asString(t *testing.T, v tftypes.Value) string {
	t.Helper()
	var s string
	if v.IsNull() {
		return "<null>"
	}
	if err := v.As(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// consistent holds an apply to OpenTofu's rules, failing the test on a breach: on success,
// inconsistencies; on an error, the state it returns is still wholly known.
func (h *harness) consistent(planned, applied *tfprotov6.DynamicValue, ok bool) {
	h.t.Helper()
	for _, e := range h.applyBreaches(planned, applied, ok) {
		h.t.Error(e)
	}
}

// applyBreaches is what consistent fails on.
func (h *harness) applyBreaches(planned, applied *tfprotov6.DynamicValue, ok bool) []string {
	h.t.Helper()
	if applied == nil || h.value(applied).IsNull() {
		if ok && planned != nil && !h.value(planned).IsNull() {
			return []string{"invalid result: a create or update succeeded with no state"}
		}
		return nil
	}
	if ok {
		return h.inconsistencies(planned, applied, true)
	}
	return unknowns(h.value(applied))
}

func (h *harness) value(d *tfprotov6.DynamicValue) tftypes.Value {
	h.t.Helper()
	v, err := d.Unmarshal(h.typ)
	if err != nil {
		h.t.Fatal(err)
	}
	return v
}

// unknowns is the breach of a state returned holding an unknown value.
func unknowns(v tftypes.Value) []string {
	if !v.IsFullyKnown() {
		return []string{fmt.Sprintf("invalid result: the applied state holds an unknown value: %v", v)}
	}
	return nil
}

// inconsistencies are the breaches of OpenTofu's rules against an earlier plan: every value it
// knew — at any depth, a collection's length included, a set by its length alone — is the one
// in later; and later is wholly known when it is an applied state (known).
func (h *harness) inconsistencies(planned, later *tfprotov6.DynamicValue, applied bool) []string {
	h.t.Helper()
	return breaches(h.value(planned), h.value(later), applied)
}

// breaches is inconsistencies on decoded values.
func breaches(pv, av tftypes.Value, applied bool) []string {
	var out []string
	if applied {
		out = unknowns(av)
	}
	_ = tftypes.Walk(pv, func(p *tftypes.AttributePath, v tftypes.Value) (bool, error) {
		if !v.IsKnown() {
			return false, nil // planned unknown: any value goes
		}
		got, rest, err := tftypes.WalkAttributePath(av, p)
		g, isValue := got.(tftypes.Value)
		if err != nil || len(rest.Steps()) > 0 || !isValue {
			out = append(out, fmt.Sprintf("inconsistent result: %s planned %v, absent", p, v))
			return false, nil
		}
		if v.IsFullyKnown() && !v.Type().Is(tftypes.Object{}) || v.IsNull() {
			if !v.Equal(g) {
				out = append(out, fmt.Sprintf("inconsistent result: %s planned %v, then %v", p, v, g))
			}
			return false, nil
		}
		if v.Type().Is(tftypes.Set{}) {
			out = append(out, setBreaches(p, v, g)...)
			return false, nil
		}
		// A block, or a collection known in part: its length is known, then each element.
		if n, m := elems(v), elems(g); n != m {
			out = append(out, fmt.Sprintf("inconsistent result: %s planned %d elements, then %d", p, n, m))
		}
		return true, nil
	})
	return out
}

// setBreaches holds a set known in part to OpenTofu's rule: every planned element — known in part
// included — is compatible with an applied one, every applied element with a planned one, and the
// applied set is no longer than planned (unknown elements may coalesce).
func setBreaches(p *tftypes.AttributePath, planned, applied tftypes.Value) []string {
	var pe, ae []tftypes.Value
	if planned.As(&pe) != nil || applied.As(&ae) != nil {
		return []string{fmt.Sprintf("inconsistent result: %s planned %v, then %v", p, planned, applied)}
	}
	compatible := func(x, y tftypes.Value) bool { return len(breaches(x, y, false)) == 0 }
	var out []string
	if len(ae) > len(pe) {
		out = append(out, fmt.Sprintf("inconsistent result: %s planned %d elements, then %d", p, len(pe), len(ae)))
	}
	for _, e := range pe {
		if !slices.ContainsFunc(ae, func(a tftypes.Value) bool { return compatible(e, a) }) {
			out = append(out, fmt.Sprintf("inconsistent result: %s planned element %v does not correlate with any applied", p, e))
		}
	}
	for _, a := range ae {
		if !slices.ContainsFunc(pe, func(e tftypes.Value) bool { return compatible(e, a) }) {
			out = append(out, fmt.Sprintf("inconsistent result: %s applied element %v was not planned", p, a))
		}
	}
	return out
}

// elems is the number of elements of a known collection, -1 for anything else.
func elems(v tftypes.Value) int {
	switch {
	case v.Type().Is(tftypes.Map{}):
		var m map[string]tftypes.Value
		if v.As(&m) == nil {
			return len(m)
		}
	case v.Type().Is(tftypes.List{}), v.Type().Is(tftypes.Set{}):
		var l []tftypes.Value
		if v.As(&l) == nil {
			return len(l)
		}
	}
	return -1
}
