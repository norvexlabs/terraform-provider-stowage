package provider

import (
	"context"
	"maps"
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
}

// op names a call by the script it runs and the verb it asks.
func (c call) op() string {
	if len(c.argv) < 3 {
		return "?"
	}
	switch c.argv[2] {
	case installer.FetchScript:
		return "fetch"
	case installer.RunScript:
		switch {
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
}

const describe1 = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.2.0","revision":1},"channel":"stable","shapes":["docker"],"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"LEGACY"}]}`

func newFake() *fake {
	return &fake{queue: map[string][]reply{}, replies: map[string]reply{
		"fetch":         {},
		"describe":      {stdout: describe1},
		"kept-describe": {stdout: describe1},
		"install":       {stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`},
		"rerun":         {stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`},
		"upgrade":       {stdout: `{"format":1,"result":"done","release":{"version":"2.0.0","revision":1}}`},
		"rollback":      {stdout: `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`},
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

func (f *fake) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := call{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)}
	f.calls = append(f.calls, c)
	if err := ctx.Err(); err != nil {
		return nil, nil, -1, err
	}
	r, ok := f.replies[c.op()]
	if q := f.queue[c.op()]; len(q) > 0 {
		r, ok, f.queue[c.op()] = q[0], true, q[1:]
	}
	if !ok {
		return nil, []byte("unexpected call"), 99, nil
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
	t      *testing.T
	srv    tfprotov6.ProviderServer
	typ    tftypes.Object
	ptyp   tftypes.Object
	fake   *fake
	schema *tfprotov6.Schema
}

func newHarness(t *testing.T, providerConfig map[string]tftypes.Value) *harness {
	t.Helper()
	f := newFake()
	srv, err := providerserver.NewProtocol6WithError(newWithPoll("test", f.factory, time.Millisecond)())()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || len(s.Diagnostics) > 0 {
		t.Fatalf("schema: %v %v", err, s.Diagnostics)
	}
	h := &harness{t: t, srv: srv, fake: f, schema: s.ResourceSchemas["stowage_install"]}
	h.typ = h.schema.ValueType().(tftypes.Object)
	h.ptyp = s.Provider.ValueType().(tftypes.Object)
	cfg := h.dv(h.ptyp, fill(h.ptyp, providerConfig))
	resp, err := srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &cfg})
	if err != nil || len(resp.Diagnostics) > 0 {
		t.Fatalf("configure: %v %s", err, diagText(resp.Diagnostics))
	}
	return h
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
	proposed := h.dv(h.typ, h.withoutWriteOnly(config))
	resp, err := h.srv.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: "stowage_install", PriorState: prior, ProposedNewState: &proposed, Config: &cfg,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) apply(prior *tfprotov6.DynamicValue, planned *tfprotov6.DynamicValue, config tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	h.t.Helper()
	if prior == nil {
		p := h.dv(h.typ, tftypes.NewValue(h.typ, nil))
		prior = &p
	}
	cfg := h.dv(h.typ, config)
	resp, err := h.srv.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName: "stowage_install", PriorState: prior, PlannedState: planned, Config: &cfg,
	})
	if err != nil {
		h.t.Fatal(err)
	}
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
	v, err := state.Unmarshal(h.typ)
	if err != nil {
		h.t.Fatal(err)
	}
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
