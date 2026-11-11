package provider

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const (
	seedLine = "vm-seed-host-key ssh-ed25519 " + edKey
	ownKey   = "AAAAC3NzaC1lZDI1NTE5AAAAIA6m3nbmS3rbQ6jH1gm8K07n6l9Tqv9c0vY2pY6J6oTd"
	ownLine  = "192.0.2.7 ssh-ed25519 " + ownKey
)

// vmFake answers a VM's commands: each op with its queued replies, then its set one. It serves
// one host key, as sshd does: the one-time key until the key is retired (the retire op, or a
// status of another boot than the first: the boot after the install retires it), then the VM's
// own. A connection that does not accept the key served fails as ssh does, exit 255.
type vmFake struct {
	mu      sync.Mutex
	calls   []call
	conns   []Connection
	replies map[string]reply
	queue   map[string][]reply
	retired bool
	// callConns[i] is the connection calls[i] was made over.
	callConns []Connection
	// retireNoop: the retire op exits 0 but sshd goes on serving the one-time key (its reload not
	// taken).
	retireNoop bool
}

// hang, a reply's stderr, makes the run block until its context ends, as a run the deadline cuts.
const hang = "<hang>"

// vmConn is the fake's runner for one connection.
type vmConn struct {
	f *vmFake
	c Connection
}

func vmOp(argv []string) string {
	switch {
	case len(argv) > 2 && argv[0] == "sh" && argv[2] == installer.RunScript:
		return "install"
	case len(argv) > 2 && argv[0] == "sh" && argv[2] == installer.CtlScript:
		return "probe"
	case len(argv) == 3 && argv[1] == "describe":
		return "describe"
	case len(argv) == 3 && argv[1] == "status":
		return "status"
	case len(argv) > 4 && argv[0] == "sudo" && argv[2] == "systemctl":
		return "boot"
	case len(argv) == 5 && argv[0] == "sudo" && argv[3] == "host-key" && argv[4] == "--json":
		return "retire"
	case len(argv) > 3 && argv[0] == "sudo":
		return argv[3]
	case len(argv) == 2 && argv[0] == "cat":
		return "hostkey"
	}
	return "?"
}

func vmStatusJSON(version, boot string, extra string) string {
	return fmt.Sprintf(`{"format":1,"name":"notes","os":{"version":%q,"revision":1},"installed":{"version":%q,"revision":1},"previous":null,"channel":"stable","inputs":["ADMIN_EMAIL"],"boot":"active","boot_id":%q%s}`, version, version, boot, extra)
}

const vmDescribe = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.1.0","revision":1},"channel":"","shapes":["docker","kairos"],"inputs":[{"key":"ADMIN_EMAIL"},{"key":"API_KEY","secret":true}],"first_install_only":["address","registry","registry_ca","channel"],"routes":[]}`

// describeOf is vmDescribe of another release, with more inputs.
func describeOf(version string, extraInputs string) string {
	d := strings.Replace(vmDescribe, `"version":"1.1.0"`, fmt.Sprintf(`"version":%q`, version), 1)
	if extraInputs != "" {
		d = strings.Replace(d, `{"key":"ADMIN_EMAIL"}`, `{"key":"ADMIN_EMAIL"},`+extraInputs, 1)
	}
	return d
}

func newVMFake() *vmFake {
	return &vmFake{queue: map[string][]reply{}, replies: map[string]reply{
		"describe": {stdout: vmDescribe},
		"boot":     {},
		"status":   {stdout: vmStatusJSON("1.1.0", "boot-1", "")},
		"install":  {stdout: `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`},
		"upgrade":  {stdout: `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`},
		"rollback": {stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`},
		"hostkey":  {stdout: "ssh-ed25519 " + ownKey + " root@notes\n"},
		"retire":   {stdout: `{"format":1,"known_hosts_line":"notes ssh-ed25519 ` + ownKey + `","fingerprint":"SHA256:own"}`},
		"probe":    {}, // the installer is found
	}}
}

func (f *vmFake) then(op string, rs ...reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue[op] = append(f.queue[op], rs...)
}

func (f *vmFake) set(op string, r reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[op] = r
}

func (f *vmFake) factory(c Connection) (target.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conns = append(f.conns, c)
	return vmConn{f: f, c: c}, nil
}

// accepts says whether c accepts the key whose base64 is key.
func (c Connection) accepts(key string) bool {
	for _, l := range append([]string{c.KnownHostsLine}, c.AlsoKnownHostsLines...) {
		if strings.HasSuffix(l, " "+key) {
			return true
		}
	}
	return false
}

func (r vmConn) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	f := r.f
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call{argv: append([]string(nil), argv...), stdin: append([]byte(nil), stdin...)})
	f.callConns = append(f.callConns, r.c)
	if err := ctx.Err(); err != nil {
		return nil, nil, -1, err
	}
	served := edKey
	if f.retired {
		served = ownKey
	}
	if !r.c.accepts(served) {
		return nil, []byte("Host key verification failed."), 255, nil
	}
	op := vmOp(argv)
	rep, ok := f.replies[op]
	if q := f.queue[op]; len(q) > 0 {
		rep, ok, f.queue[op] = q[0], true, q[1:]
	}
	if !ok {
		return nil, []byte("unexpected call"), 99, nil
	}
	if rep.stderr == hang {
		f.mu.Unlock()
		<-ctx.Done()
		f.mu.Lock()
		return nil, nil, -1, fmt.Errorf("%s: %w", argv[0], ctx.Err())
	}
	switch {
	case op == "retire" && rep.exit == 0 && !f.retireNoop:
		f.retired = true
	case op == "status" && strings.Contains(rep.stdout, `"boot_id":"boot-`) && !strings.Contains(rep.stdout, `"boot_id":"boot-1"`):
		f.retired = true // a later boot than the install's
	}
	return []byte(rep.stdout), []byte(rep.stderr), rep.exit, nil
}

func (f *vmFake) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, vmOp(c.argv))
	}
	return out
}

func (f *vmFake) callsOf(op string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if vmOp(c.argv) == op {
			out = append(out, c)
		}
	}
	return out
}

func (f *vmFake) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.conns, f.callConns = nil, nil, nil
}

func (f *vmFake) lastConn() Connection {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns[len(f.conns)-1]
}

// newVMHarness is a harness whose targets are answered by a vmFake.
func newVMHarness(t *testing.T) (*harness, *vmFake) {
	t.Helper()
	return newVMHarnessPoll(t, time.Millisecond)
}

// newVMHarnessPoll is newVMHarness polling every poll.
func newVMHarnessPoll(t *testing.T, poll time.Duration) (*harness, *vmFake) {
	t.Helper()
	f := newVMFake()
	srv, err := providerserver.NewProtocol6WithError(newWithPoll("test", f.factory, poll)())()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := srv.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil || len(s.Diagnostics) > 0 {
		t.Fatalf("schema: %v %v", err, s.Diagnostics)
	}
	h := &harness{t: t, srv: srv, schema: s.ResourceSchemas["stowage_install"]}
	h.typ = h.schema.ValueType().(tftypes.Object)
	h.ptyp = s.Provider.ValueType().(tftypes.Object)
	cfg := h.dv(h.ptyp, fill(h.ptyp, nil))
	if resp, err := srv.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: &cfg}); err != nil || len(resp.Diagnostics) > 0 {
		t.Fatalf("configure: %v %s", err, diagText(resp.Diagnostics))
	}
	return h, f
}

func (h *harness) vmBlock(version string, extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine), "version": str(version), "short_name": str("notes")}
	for k, v := range extra {
		vals[k] = v
	}
	return h.block("vm", vals)
}

func (h *harness) vmConfig(version string, extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{"vm": h.vmBlock(version, nil), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})}
	for k, v := range extra {
		vals[k] = v
	}
	return fill(h.typ, vals)
}

func vmAttr(t *testing.T, h *harness, state *tfprotov6.DynamicValue, name string) string {
	t.Helper()
	var vm map[string]tftypes.Value
	if err := h.attrs(state)["vm"].As(&vm); err != nil {
		t.Fatal(err)
	}
	return asString(t, vm[name])
}

func TestVMCreate(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	// The VM boots: ssh fails, then it answers; its daemon is not up yet; the boot is done.
	f.then("describe", reply{exit: 255, stderr: "ssh: connect to host 192.0.2.7 port 22: Connection refused"})
	f.then("status", reply{exit: 1, stderr: "the host daemon does not answer"}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")},
		reply{exit: 1, stderr: "the host daemon restarts"}) // after the install: tried again
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{
		"secret_inputs_wo": strMap(map[string]string{"API_KEY": secretVal}), "secret_inputs_wo_version": num(1),
	})
	state := h.create(config)
	// A failed status is looked into (probe: the installer is there), then waited out.
	want := []string{"describe", "describe", "boot", "status", "probe", "describe", "boot", "status", "describe", "install", "status", "status", "retire", "hostkey"}
	if got := f.ops(); !slices.Equal(got, want) {
		t.Errorf("ops\n%q\nwant\n%q", got, want)
	}
	if b := f.callsOf("boot")[0]; !slices.Equal(b.argv, []string{"sudo", "-n", "systemctl", "start", "notes.service"}) {
		t.Errorf("boot %q", b.argv)
	}
	if r := f.callsOf("retire")[0]; !slices.Equal(r.argv, []string{"sudo", "-n", "notes-stow", "host-key", "--json"}) {
		t.Errorf("retire %q", r.argv)
	}
	in := f.callsOf("install")[0]
	if strings.Contains(strings.Join(in.argv, " "), secretVal) || !strings.Contains(string(in.stdin), secretVal) {
		t.Errorf("the secret input is not on standard input alone: %q", in.argv)
	}
	if !slices.Contains(in.argv, "ADMIN_EMAIL=a@b") || !slices.Contains(in.argv, "sudo") {
		t.Errorf("install argv %q", in.argv)
	}
	a := h.attrs(state)
	if asString(t, a["version"]) != "1.1.0" || asString(t, a["status"]) != "installed" || asString(t, a["id"]) != "vm:192.0.2.7/notes-stow" || asString(t, a["short_name"]) != "notes" {
		t.Errorf("state %v", a)
	}
	if got := vmAttr(t, h, state, "host_key_line"); got != ownLine {
		t.Errorf("host_key_line %q", got)
	}
	// The one-time key is retired: nothing accepts it any more.
	if got := vmAttr(t, h, state, "seed_boot_id"); got != "<null>" {
		t.Errorf("seed_boot_id after the retirement %q", got)
	}
	if !a["secret_inputs_wo"].IsNull() {
		t.Error("a write-only value in state")
	}
	f.reset()
	if r := h.read(state); errorsOf(r.Diagnostics) != "" {
		t.Fatal(errorsOf(r.Diagnostics))
	}
	if c := f.lastConn(); c.KnownHostsLine != ownLine || len(c.AlsoKnownHostsLines) != 0 {
		t.Errorf("a connection after the install %+v", c)
	}
	// known_hosts_line may go once the own key is in state.
	noLine := fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
		"host": str("192.0.2.7"), "user": str("ops"), "version": str("1.1.0"), "short_name": str("notes")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"}), "secret_inputs_wo_version": num(1), "deletion_protection": boolean(false)})
	if d := errorsOf(h.validate(noLine)); d != "" {
		t.Fatalf("no known_hosts_line after the install: %s", d)
	}
	a2 := h.update(state, noLine)
	if d := errorsOf(a2.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if vmAttr(t, h, a2.NewState, "host_key_line") != ownLine || vmAttr(t, h, a2.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("an update of the provider's own settings changed the VM's keys: %v", h.attrs(a2.NewState)["vm"])
	}
	// Destroying it forgets it: nothing runs on the VM.
	f.reset()
	if d := errorsOf(h.destroy(a2.NewState).Diagnostics); d != "" || len(f.ops()) != 0 {
		t.Errorf("destroy: %s %q", d, f.ops())
	}
}

// TestVMRetireFails: a retirement that fails is a warning; the one-time key is accepted beside
// the VM's own until a later boot is seen, then no more.
func TestVMRetireFails(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("retire", reply{exit: 1, stderr: "sudo: a terminal is required"})
	config := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if !strings.Contains(diagText(a.Diagnostics), "was not retired") {
		t.Errorf("no warning: %s", diagText(a.Diagnostics))
	}
	state := a.NewState
	if got := vmAttr(t, h, state, "seed_boot_id"); got != "boot-1" {
		t.Errorf("seed_boot_id %q", got)
	}
	f.reset()
	state = h.read(state).NewState
	if c := f.lastConn(); c.KnownHostsLine != ownLine || !slices.Equal(c.AlsoKnownHostsLines, []string{seedLine}) {
		t.Errorf("a connection before the reboot %+v", c)
	}
	// An upgrade's reboot retires it: the update's reads go on under the VM's own key.
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	up := h.update(state, h.vmConfig("1.2.0", nil))
	if d := errorsOf(up.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := vmAttr(t, h, up.NewState, "seed_boot_id"); got != "<null>" {
		t.Errorf("seed_boot_id after the upgrade %q", got)
	}
	f.reset()
	h.read(up.NewState)
	if c := f.lastConn(); len(c.AlsoKnownHostsLines) != 0 {
		t.Errorf("the one-time key is still accepted after the reboot %+v", c)
	}
}

func TestVMCreateRefused(t *testing.T) {
	t.Parallel()
	timeout := func(h *harness) tftypes.Value {
		return h.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("300ms")})})
	}
	for name, c := range map[string]struct {
		setup func(*vmFake)
		cfg   func(*harness) tftypes.Value
		want  string
		ran   string // an op that must not have run
	}{
		"the installer refuses": {
			setup: func(f *vmFake) {
				f.set("install", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"API_KEY is required","fix":"give it"}}`})
			},
			want: "API_KEY is required", ran: "hostkey",
		},
		"the install failed": {
			setup: func(f *vmFake) {
				f.set("status", reply{stdout: `{"format":1,"name":"notes","installed":null,"install_failed":"provision hook exited 1","boot":"active","boot_id":"b"}`})
			},
			want: "provision hook exited 1", ran: "hostkey",
		},
		"a version other than the disk's": {
			cfg:  func(h *harness) tftypes.Value { return h.vmConfig("1.2.0", nil) },
			want: `set vm.version = "1.1.0"`, ran: "install",
		},
		"a secret input in inputs": {
			cfg: func(h *harness) tftypes.Value {
				return h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"API_KEY": "x"})})
			},
			want: "secret input in inputs", ran: "install",
		},
		"no known_hosts_line": {
			cfg: func(h *harness) tftypes.Value {
				return fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
					"host": str("192.0.2.7"), "user": str("ops"), "version": str("1.1.0"), "short_name": str("notes")})})
			},
			want: "vm.known_hosts_line is required to install", ran: "describe",
		},
		"still on the recovery system": {
			setup: func(f *vmFake) {
				f.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"boot":"active"`, `"boot":"recovery"`, 1)})
			},
			cfg: timeout, want: "answers from its recovery system", ran: "install",
		},
		"never answering": {
			setup: func(f *vmFake) { f.set("describe", reply{exit: 255, stderr: "No route to host"}) },
			cfg:   timeout, want: "No route to host", ran: "boot",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			if c.setup != nil {
				c.setup(f)
			}
			cfg := h.vmConfig("1.1.0", nil)
			if c.cfg != nil {
				cfg = c.cfg(h)
			}
			a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
			if got := errorsOf(a.Diagnostics); !strings.Contains(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if slices.Contains(f.ops(), c.ran) {
				t.Errorf("%s ran: %q", c.ran, f.ops())
			}
			if h.attrs(a.NewState) != nil {
				t.Error("a failed create left state")
			}
		})
	}
}

// TestVMFirstBootSwitchesOS: a disk of another release writes this release's OS and reboots in its
// boot unit: the provider reads the OS that answers after it.
func TestVMFirstBootSwitchesOS(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.then("describe", reply{stdout: describeOf("1.0.0", "")})
	f.then("boot", reply{exit: 255, stderr: "Connection closed by remote host"})
	state := h.create(h.vmConfig("1.1.0", nil))
	if asString(t, h.attrs(state)["version"]) != "1.1.0" || len(f.callsOf("install")) != 1 {
		t.Errorf("state %v ops %q", h.attrs(state), f.ops())
	}
	// An OS that changes between the boot unit and the read is read again.
	h3, f3 := newVMHarness(t)
	f3.then("describe", reply{stdout: describeOf("1.0.0", "")}, reply{stdout: vmDescribe})
	if s := h3.create(h3.vmConfig("1.1.0", nil)); asString(t, h3.attrs(s)["version"]) != "1.1.0" || f3.ops()[3] != "describe" || f3.ops()[4] != "describe" {
		t.Errorf("state %v ops %q", h3.attrs(s), f3.ops())
	}
}

// TestVMUpgradeAddsInputs: an upgrade takes no input; what the new release adds goes to its
// re-run after the reboot, and an input it declares secret is refused there before it is given.
func TestVMUpgradeAddsInputs(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.then("status",
		reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")},
		reply{exit: 255, stderr: "Connection closed"},
		reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"on_trial":true`)}, // its upgrade not finished
	)
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	f.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", `{"key":"NEW_IN_12"},{"key":"NEW_TOKEN","secret":true}`)})
	cfg := h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_IN_12": "x"}),
		"domains": strMap(map[string]string{"admin": "admin.lan"})})
	a := h.update(state, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	installs := f.callsOf("install")
	if len(installs) != 1 || !slices.Contains(installs[0].argv, "NEW_IN_12=x") || !slices.Contains(installs[0].argv, "admin=admin.lan") {
		t.Errorf("installs %q", installs)
	}
	if i := slices.Index(f.ops(), "upgrade"); i < 0 || i > slices.Index(f.ops(), "install") {
		t.Errorf("the re-run is not after the upgrade: %q", f.ops())
	}
	if asString(t, h.attrs(a.NewState)["version"]) != "1.2.0" || vmAttr(t, h, a.NewState, "version") != "1.2.0" {
		t.Errorf("state %v", h.attrs(a.NewState))
	}

	// A secret input of the new release given in inputs: refused before the re-run, and kept
	// out of state.
	h2, f2 := newVMHarness(t)
	state = h2.create(h2.vmConfig("1.1.0", nil))
	f2.reset()
	f2.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f2.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	f2.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", `{"key":"NEW_TOKEN","secret":true}`)})
	a = h2.update(state, h2.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_TOKEN": secretVal})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "NEW_TOKEN is a secret input of notes 1.2.0") {
		t.Errorf("a secret input the release adds: %s", got)
	}
	if len(f2.callsOf("install")) != 0 {
		t.Errorf("the re-run ran: %q", f2.ops())
	}
	var inputs map[string]tftypes.Value
	_ = h2.attrs(a.NewState)["inputs"].As(&inputs)
	if _, ok := inputs["NEW_TOKEN"]; ok || vmAttr(t, h2, a.NewState, "version") != "1.2.0" {
		t.Errorf("state after the refusal: %v", h2.attrs(a.NewState))
	}
}

// TestVMUpgradeFallsBack: an upgrade whose boot fails its health gate is an error saving the
// release the VM runs, and the next plan proposes the upgrade again.
func TestVMUpgradeFallsBack(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.then("status",
		reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")},
		reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"on_trial":true`)},
	)
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-3", `,"fallback":true,"failed_boot":{"release":{"version":"1.2.0","revision":1},"at":"t","reason":"api not ready in 5m"}`)})
	config := h.vmConfig("1.2.0", nil)
	a := h.update(state, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "api not ready in 5m") || !strings.Contains(got, "went back to 1.1.0") {
		t.Errorf("a fallback: %s", got)
	}
	if v := vmAttr(t, h, a.NewState, "version"); v != "1.1.0" {
		t.Errorf("state's vm.version after a fallback: %s", v)
	}
	if p := h.plan(a.NewState, config); errorsOf(p.Diagnostics) != "" || vmAttr(t, h, p.PlannedState, "version") != "1.2.0" {
		t.Errorf("the next plan does not propose the upgrade again: %s", errorsOf(p.Diagnostics))
	}

	// A reboot never followed: the error, and state saved as before the upgrade.
	h2, f2 := newVMHarness(t)
	state = h2.create(h2.vmConfig("1.1.0", nil))
	f2.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	cfg := h2.vmConfig("1.2.0", map[string]tftypes.Value{"timeouts": h2.block("timeouts", map[string]tftypes.Value{"update": str("300ms")})})
	a = h2.update(state, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "had not come back from its reboot") || a.NewState == nil || vmAttr(t, h2, a.NewState, "version") != "1.1.0" {
		t.Errorf("no reboot: %s", got)
	}
}

// TestVMUpgradeLastReadFails: the release changed and the re-run went: a status that cannot be
// read at the end keeps the one read after the reboot, never the state before.
func TestVMUpgradeLastReadFails(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	f.set("status", reply{exit: 1, stderr: "the host daemon does not answer"})
	f.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", `{"key":"NEW_IN_12"}`)})
	a := h.update(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_IN_12": "x"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the host daemon does not answer") {
		t.Errorf("got %s", got)
	}
	var inputs map[string]tftypes.Value
	_ = h.attrs(a.NewState)["inputs"].As(&inputs)
	if vmAttr(t, h, a.NewState, "version") != "1.2.0" || inputs["NEW_IN_12"].IsNull() {
		t.Errorf("state %v", h.attrs(a.NewState))
	}
}

func TestVMRollback(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	withPrev := strings.Replace(vmStatusJSON("1.2.0", "boot-1", ""), `"previous":null`, `"previous":{"version":"1.1.0","revision":1}`, 1)
	f.set("describe", reply{stdout: describeOf("1.2.0", "")})
	f.set("status", reply{stdout: withPrev})
	state := h.create(h.vmConfig("1.2.0", nil))
	back := h.vmConfig("1.1.0", nil)
	if got := errorsOf(h.plan(state, back).Diagnostics); !strings.Contains(got, "a rollback needs allow_rollback") {
		t.Errorf("a rollback without allow_rollback: %s", got)
	}
	f.reset()
	f.then("status", reply{stdout: withPrev})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"boot":"passive","fallback":true`)})
	a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"allow_rollback": boolean(true)}))
	if got := errorsOf(a.Diagnostics); got != "" {
		t.Fatal(got)
	}
	if len(f.callsOf("rollback")) != 1 || len(f.callsOf("upgrade")) != 0 || asString(t, h.attrs(a.NewState)["version"]) != "1.1.0" {
		t.Errorf("ops %q, state %v", f.ops(), h.attrs(a.NewState))
	}
	both := h.vmConfig("1.1.0", map[string]tftypes.Value{"allow_rollback": boolean(true), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	if got := errorsOf(h.plan(state, both).Diagnostics); !strings.Contains(got, "a rollback goes alone") || !strings.Contains(got, "vm.version goes back to previous.version") {
		t.Errorf("a rollback with an input change: %s", got)
	}
}

func TestVMRerun(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if got := errorsOf(a.Diagnostics); got != "" {
		t.Fatal(got)
	}
	if in := f.callsOf("install"); len(in) != 1 || !slices.Contains(in[0].argv, "ADMIN_EMAIL=c@d") || len(f.callsOf("upgrade")) != 0 {
		t.Errorf("ops %q", f.ops())
	}
}

// secrets_wo goes to the VM's first install, a file on standard input (never an argument), for a
// seed whose first install awaits it; a re-run never gets it.
func TestVMCreateGivesTheSecretsExport(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", map[string]tftypes.Value{"secrets_wo": str("export-" + secretVal)}))
	in := f.callsOf("install")
	if len(in) != 1 || !slices.Contains(in[0].argv, "secrets") || untarAll(t, in[0].stdin)["secrets"] != "export-"+secretVal ||
		strings.Contains(strings.Join(in[0].argv, " "), secretVal) {
		t.Fatalf("install %q", in)
	}
	f.reset()
	h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"secrets_wo": str("export-" + secretVal), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if in := f.callsOf("install"); len(in) != 1 || in[0].stdin != nil && untarAll(t, in[0].stdin)["secrets"] != "" {
		t.Fatalf("a re-run given the export: %q", in)
	}
}

func TestVMValidation(t *testing.T) {
	t.Parallel()
	h, _ := newVMHarness(t)
	for name, c := range map[string]struct {
		cfg  tftypes.Value
		want string
	}{
		"no short_name": {fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
			"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine), "version": str("1.1.0")})}), "vm.short_name is required"},
		"a bad short_name": {fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("a/b")})}), "not an installer's short name"},
		"a bad line":       {fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"known_hosts_line": str("@revoked x ssh-ed25519 " + edKey)})}), "does not parse"},
	} {
		if got := errorsOf(h.validate(c.cfg)); !strings.Contains(got, c.want) {
			t.Errorf("%s: %s", name, got)
		}
	}
	if got := errorsOf(h.validate(h.vmConfig("1.1.0", nil))); got != "" {
		t.Errorf("the clean configuration: %s", got)
	}
}

func TestVMImport(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	for id, want := range map[string]string{
		"vm:192.0.2.7/notes-stow": "",
		"vm:192.0.2.7/notes":      "want vm:<host>/<short>-stow",
		"vm:-o/notes-stow":        "want vm:<host>/<short>-stow",
	} {
		resp, err := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: id})
		if err != nil {
			t.Fatal(err)
		}
		if got := errorsOf(resp.Diagnostics); (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%s: %s", id, got)
		}
	}
	// An imported VM's first apply: the configuration's own key, its inputs given to the re-run.
	f.retired = true
	resp, _ := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: "vm:192.0.2.7/notes-stow"})
	state := resp.ImportedResources[0].State
	if r := h.read(state); errorsOf(r.Diagnostics) != "" || len(f.ops()) != 0 {
		t.Fatalf("an imported state is read before its first apply: %s %q", errorsOf(r.Diagnostics), f.ops())
	}
	bare := fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
		"host": str("192.0.2.7"), "user": str("ops"), "version": str("1.1.0"), "short_name": str("notes")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	if got := errorsOf(h.update(state, bare).Diagnostics); !strings.Contains(got, "vm.known_hosts_line is required to install") {
		t.Errorf("an imported state's apply without a key: %s", got)
	}
	cfg := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"known_hosts_line": str(ownLine)}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	a := h.update(state, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if in := f.callsOf("install"); len(in) != 1 || !slices.Contains(in[0].argv, "ADMIN_EMAIL=a@b") {
		t.Errorf("ops %q", f.ops())
	}
	if asString(t, h.attrs(a.NewState)["version"]) != "1.1.0" {
		t.Errorf("state %v", h.attrs(a.NewState))
	}
}

// TestVMFirstBootWaits: a boot unit cut by the reboot it scheduled, or a status saying one is
// pending, is waited out: the release read is the one the VM answers from after it.
func TestVMFirstBootWaits(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	// The old OS answers twice, its boot unit cut by the reboot into the new one.
	f.then("describe", reply{stdout: describeOf("1.0.0", "")}, reply{stdout: describeOf("1.0.0", "")})
	f.then("boot", reply{exit: 255, stderr: "Connection closed by remote host"})
	if s := h.create(h.vmConfig("1.1.0", nil)); asString(t, h.attrs(s)["version"]) != "1.1.0" {
		t.Errorf("state %v", h.attrs(s))
	}
	h2, f2 := newVMHarness(t)
	f2.then("describe", reply{stdout: describeOf("1.0.0", "")}, reply{stdout: describeOf("1.0.0", "")})
	f2.then("status", reply{stdout: vmStatusJSON("1.0.0", "boot-1", `,"reboot_pending":true`)})
	if s := h2.create(h2.vmConfig("1.1.0", nil)); asString(t, h2.attrs(s)["version"]) != "1.1.0" {
		t.Errorf("state %v ops %q", h2.attrs(s), f2.ops())
	}
	// A second describe failing for another reason than ssh is an error at once.
	h3, f3 := newVMHarness(t)
	f3.then("describe", reply{stdout: vmDescribe}, reply{exit: 1, stderr: `Error: unknown command "describe"`})
	cfg := h3.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h3.block("timeouts", map[string]tftypes.Value{"create": str("10s")})})
	start := time.Now()
	a := h3.apply(nil, h3.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "unknown command") || time.Since(start) > 5*time.Second || slices.Contains(f3.ops(), "install") {
		t.Errorf("a failing describe: %s (after %s)", got, time.Since(start))
	}
}

// TestVMRetireNotHeld: a retirement whose verb exits 0 while the VM still serves the one-time key
// (sshd's reload not taken) is a warning, the key accepted beside the VM's own; a VM without a
// boot id keeps it so.
func TestVMRetireNotHeld(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", "")})
	config := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if !strings.Contains(diagText(a.Diagnostics), "was not retired") || !strings.Contains(diagText(a.Diagnostics), "its six polls") || vmAttr(t, h, a.NewState, "seed_boot_id") != unreadBoot {
		t.Errorf("warning %s seed_boot_id %s", diagText(a.Diagnostics), vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}) // the same boot, its id read now
	r := h.read(a.NewState)
	if errorsOf(r.Diagnostics) != "" || vmAttr(t, h, r.NewState, "seed_boot_id") != unreadBoot {
		t.Fatalf("a read under both keys: %s, seed_boot_id %s", errorsOf(r.Diagnostics), vmAttr(t, h, r.NewState, "seed_boot_id"))
	}
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", "")})
	// Dropping the line the one-time key is accepted by is a plan error while it is served.
	noLine := fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
		"host": str("192.0.2.7"), "user": str("ops"), "version": str("1.1.0"), "short_name": str("notes")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	if got := errorsOf(h.plan(a.NewState, noLine).Diagnostics); !strings.Contains(got, "vm.known_hosts_line is still needed") {
		t.Errorf("dropping known_hosts_line: %s", got)
	}
	// A release change on a VM reporting no boot id is refused before it runs.
	f.reset()
	a2 := h.update(a.NewState, h.vmConfig("1.2.0", nil))
	if got := errorsOf(a2.Diagnostics); !strings.Contains(got, "no boot_id") || len(f.callsOf("upgrade")) != 0 {
		t.Errorf("no boot id: %s %q", got, f.ops())
	}
}

// TestVMUpgradeReadsTheOwnKey: a create that could not read the VM's own key leaves it to the
// update, which reads it before a release change and follows the reboot under it; one that
// cannot read it refuses the change before it runs.
func TestVMUpgradeReadsTheOwnKey(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
	f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
	config := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" || vmAttr(t, h, a.NewState, "host_key_line") != "<null>" {
		t.Fatalf("%s %s", d, vmAttr(t, h, a.NewState, "host_key_line"))
	}
	state := a.NewState
	// The key still cannot be read: the upgrade is refused before it runs.
	f.reset()
	f.set("retire", reply{exit: 1, stderr: "sudo: a password is required"})
	f.set("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
	if got := errorsOf(h.update(state, h.vmConfig("1.2.0", nil)).Diagnostics); !strings.Contains(got, "The VM's own host key is not known") || len(f.callsOf("upgrade")) != 0 {
		t.Errorf("no own key: %s %q", got, f.ops())
	}
	// Read now: the reboot retires the one-time key, and the reads after it go under the own key.
	f.reset()
	f.set("hostkey", reply{stdout: "ssh-ed25519 " + ownKey + "\n"})
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	up := h.update(state, h.vmConfig("1.2.0", nil))
	if d := errorsOf(up.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if vmAttr(t, h, up.NewState, "host_key_line") != ownLine || asString(t, h.attrs(up.NewState)["version"]) != "1.2.0" {
		t.Errorf("state %v", h.attrs(up.NewState))
	}
	// An upgrade that falls back keeps the key it read: the reads after the reboot need it.
	h2, f2 := newVMHarness(t)
	f2.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
	f2.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
	a = h2.apply(nil, h2.plan(nil, config).PlannedState, config)
	f2.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f2.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"fallback":true,"failed_boot":{"release":{"version":"1.2.0","revision":1},"at":"t","reason":"not ready"}`)})
	fb := h2.update(a.NewState, h2.vmConfig("1.2.0", nil))
	if got := errorsOf(fb.Diagnostics); !strings.Contains(got, "not ready") || vmAttr(t, h2, fb.NewState, "host_key_line") != ownLine {
		t.Errorf("a fallback: %s, host_key_line %s", got, vmAttr(t, h2, fb.NewState, "host_key_line"))
	}
}

// TestVMUpdateLearnsTheKey: an update that reads the VM's own key for the first time retires the
// one-time key as a create does; when that fails, every state the update saves accepts both keys.
func TestVMUpdateLearnsTheKey(t *testing.T) {
	t.Parallel()
	noKey := func(t *testing.T) (*harness, *vmFake, *tfprotov6.DynamicValue) {
		h, f := newVMHarness(t)
		f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
		f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
		cfg := h.vmConfig("1.1.0", nil)
		a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
		if d := errorsOf(a.Diagnostics); d != "" {
			t.Fatal(d)
		}
		f.reset()
		return h, f, a.NewState
	}
	// A re-run alone: the key read, the one-time key retired, the VM's own alone accepted after.
	h, f, state := noKey(t)
	a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if !slices.Contains(f.ops(), "retire") || vmAttr(t, h, a.NewState, "host_key_line") != ownLine || vmAttr(t, h, a.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("ops %q vm %v", f.ops(), h.attrs(a.NewState)["vm"])
	}
	f.reset()
	if r := h.read(a.NewState); errorsOf(r.Diagnostics) != "" {
		t.Errorf("a read after: %s", errorsOf(r.Diagnostics))
	}
	// The retirement does not hold: the re-run's state accepts both keys, so the next read works.
	h, f, state = noKey(t)
	f.retireNoop = true
	a = h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if d := errorsOf(a.Diagnostics); d != "" || vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Fatalf("%s seed_boot_id %s", d, vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	if r := h.read(a.NewState); errorsOf(r.Diagnostics) != "" {
		t.Errorf("a read under both keys: %s", errorsOf(r.Diagnostics))
	}
	// The same, then an upgrade that fails before its reboot: the re-run's saved state accepts both.
	h, f, state = noKey(t)
	f.retireNoop = true
	f.set("upgrade", reply{exit: 1, stderr: "1.2.0 is not in the mirror"})
	a = h.update(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "not in the mirror") || vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Fatalf("%s seed_boot_id %s", got, vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	if r := h.read(a.NewState); errorsOf(r.Diagnostics) != "" {
		t.Errorf("a read after the failed upgrade: %s", errorsOf(r.Diagnostics))
	}
}

// TestVMUpgradeNothing: an upgrade with nothing to do makes no reboot: the one-time key a failed
// retirement left accepted stays accepted.
func TestVMUpgradeNothing(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	f.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1}}`})
	up := h.update(a.NewState, h.vmConfig("1.2.0", nil))
	if d := errorsOf(up.Diagnostics); d != "" || vmAttr(t, h, up.NewState, "seed_boot_id") != "boot-1" || !strings.Contains(diagText(up.Diagnostics), "still not retired") {
		t.Errorf("%s seed_boot_id %s", diagText(up.Diagnostics), vmAttr(t, h, up.NewState, "seed_boot_id"))
	}
	// The retirement holds this time: no reboot needed.
	f.retireNoop = false
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.3.0", "boot-1", "")})
	f.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.3.0","revision":1}}`})
	up = h.update(up.NewState, h.vmConfig("1.3.0", nil))
	if d := errorsOf(up.Diagnostics); d != "" || vmAttr(t, h, up.NewState, "seed_boot_id") != "<null>" || !slices.Contains(f.ops(), "retire") {
		t.Errorf("%s seed_boot_id %s ops %q", d, vmAttr(t, h, up.NewState, "seed_boot_id"), f.ops())
	}
}

// TestVMShortNameChange: vm.short_name is first-install only: the VM's installer is its seed's.
func TestVMShortNameChange(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	other := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("nt")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	if got := errorsOf(h.plan(state, other).Diagnostics); !strings.Contains(got, "vm.short_name is first-install only") {
		t.Errorf("got %s", got)
	}
	// Unknown at plan time (a replaced seed's): refused at apply, before anything runs.
	unknownShort := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"short_name": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	p := h.plan(state, unknownShort)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	f.reset()
	newShort := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"short_name": str("nt")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	a := h.apply(state, p.PlannedState, newShort)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "vm.short_name is first-install only") || len(f.ops()) != 0 {
		t.Errorf("at apply: %s ops %q", got, f.ops())
	}
	// An imported state's short name is its import ID's.
	resp, _ := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: "vm:192.0.2.7/notes-stow"})
	if got := errorsOf(h.plan(resp.ImportedResources[0].State, other).Diagnostics); !strings.Contains(got, "vm.short_name is first-install only") || !strings.Contains(got, "give vm.known_hosts_line the VM's own key") || !strings.Contains(got, "reboot the VM first") {
		t.Errorf("an imported state: %s", got)
	}
	// The import route it names: a vm.channel in the configuration is no refusal there.
	withChannel := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"channel": str("stable"), "known_hosts_line": str(ownLine)})})
	if got := errorsOf(h.plan(resp.ImportedResources[0].State, withChannel).Diagnostics); got != "" {
		t.Errorf("an imported state with vm.channel: %s", got)
	}
	// Its first apply compares it with the channel the VM follows, before anything runs.
	f.reset()
	f.retired = true
	beta := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"channel": str("beta"), "known_hosts_line": str(ownLine)})})
	imp := resp.ImportedResources[0].State
	if got := errorsOf(h.apply(imp, h.plan(imp, beta).PlannedState, beta).Diagnostics); !strings.Contains(got, "vm.channel is not the VM's") || slices.Contains(f.ops(), "install") || slices.Contains(f.ops(), "hostkey") || slices.Contains(f.ops(), "retire") {
		t.Errorf("an imported state's wrong channel: %s %q", got, f.ops())
	}
	if a := h.apply(imp, h.plan(imp, withChannel).PlannedState, withChannel); errorsOf(a.Diagnostics) != "" {
		t.Errorf("an imported state's own channel: %s", errorsOf(a.Diagnostics))
	}
}

// TestVMNoBootIDRefusesFirst: a release change on a VM reporting no boot id is refused before
// anything runs, the re-run of the same apply included.
func TestVMNoBootIDRefusesFirst(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", "")})
	a := h.update(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "no boot_id") || slices.Contains(f.ops(), "install") || slices.Contains(f.ops(), "upgrade") {
		t.Errorf("%s ops %q", got, f.ops())
	}
}

// TestImportFirstApplyIsConsistent: a Docker install's import and first apply, held to
// OpenTofu's rule: the registry it records is planned unknown.
func TestImportFirstApplyIsConsistent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
}

// TestVMUnreadSeedAfterReboot: a one-time key left accepted with no boot id is retired by the
// reboot an upgrade made, even when that upgrade falls back.
func TestVMUnreadSeedAfterReboot(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "", "")}, reply{stdout: vmStatusJSON("1.1.0", "", "")})
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if vmAttr(t, h, a.NewState, "seed_boot_id") != unreadBoot {
		t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"fallback":true,"failed_boot":{"release":{"version":"1.2.0","revision":1},"at":"t","reason":"not ready"}`)})
	up := h.apply(a.NewState, h.plan(a.NewState, h.vmConfig("1.2.0", nil)).PlannedState, h.vmConfig("1.2.0", nil))
	if got := errorsOf(up.Diagnostics); !strings.Contains(got, "not ready") || vmAttr(t, h, up.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("%s seed_boot_id %s", got, vmAttr(t, h, up.NewState, "seed_boot_id"))
	}
}

// TestVMKeySavedFirst: an update that learns the VM's own key saves it before anything can fail:
// its retirement means the VM no longer serves the key state accepted.
func TestVMKeySavedFirst(t *testing.T) {
	t.Parallel()
	noKey := func(t *testing.T) (*harness, *vmFake, *tfprotov6.DynamicValue) {
		h, f := newVMHarness(t)
		f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
		f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
		cfg := h.vmConfig("1.1.0", nil)
		a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
		f.reset()
		return h, f, a.NewState
	}
	// The upgrade fails after the key was learnt: state holds it, and the next read works.
	h, f, state := noKey(t)
	f.set("upgrade", reply{exit: 1, stderr: "1.2.0 is not in the mirror"})
	a := h.apply(state, h.plan(state, h.vmConfig("1.2.0", nil)).PlannedState, h.vmConfig("1.2.0", nil))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "not in the mirror") || vmAttr(t, h, a.NewState, "host_key_line") != ownLine {
		t.Fatalf("%s host_key_line %s", got, vmAttr(t, h, a.NewState, "host_key_line"))
	}
	if r := h.read(a.NewState); errorsOf(r.Diagnostics) != "" {
		t.Errorf("a read after: %s", errorsOf(r.Diagnostics))
	}
	// A refusal that needs no change comes before the key is learnt: nothing runs on the VM.
	h, f, state = noKey(t)
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", "")})
	a = h.apply(state, h.plan(state, h.vmConfig("1.2.0", nil)).PlannedState, h.vmConfig("1.2.0", nil))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "no boot_id") || slices.Contains(f.ops(), "retire") || slices.Contains(f.ops(), "hostkey") {
		t.Errorf("%s ops %q", got, f.ops())
	}
}

// TestVMRetireTriedAgain: a retirement a create could not make is tried again by the next update
// that changes no release (a re-run here), and its success saved.
func TestVMRetireTriedAgain(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "", "")}, reply{stdout: vmStatusJSON("1.1.0", "", "")})
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if vmAttr(t, h, a.NewState, "seed_boot_id") != unreadBoot {
		t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.retireNoop = false
	up := h.update(a.NewState, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if d := errorsOf(up.Diagnostics); d != "" || !slices.Contains(f.ops(), "retire") || vmAttr(t, h, up.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("%s ops %q seed_boot_id %s", d, f.ops(), vmAttr(t, h, up.NewState, "seed_boot_id"))
	}
	f.reset()
	h.read(up.NewState)
	if c := f.lastConn(); len(c.AlsoKnownHostsLines) != 0 {
		t.Errorf("the one-time key is still accepted %+v", c)
	}
}

// TestVMImportedSavesItsConnection: an imported state's first apply that fails after the key was
// learnt saves the configuration's connection with it, so the next refresh reaches the VM.
func TestVMImportedSavesItsConnection(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*vmFake){
		"the upgrade fails": func(f *vmFake) { f.set("upgrade", reply{exit: 1, stderr: "1.2.0 is not in the mirror"}) },
		"the upgrade falls back": func(f *vmFake) {
			f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
			f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", `,"fallback":true,"failed_boot":{"release":{"version":"1.2.0","revision":1},"at":"t","reason":"not ready"}`)})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			resp, _ := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: "vm:192.0.2.7/notes-stow"})
			state := resp.ImportedResources[0].State
			setup(f)
			cfg := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"port": num(2222)})})
			a := h.apply(state, h.plan(state, cfg).PlannedState, cfg)
			if errorsOf(a.Diagnostics) == "" {
				t.Fatal("no error")
			}
			if vmAttr(t, h, a.NewState, "user") != "ops" || vmAttr(t, h, a.NewState, "host_key_line") != ownLine {
				t.Errorf("state %v", h.attrs(a.NewState)["vm"])
			}
			f.reset()
			if r := h.read(a.NewState); errorsOf(r.Diagnostics) != "" {
				t.Fatalf("a read after: %s", errorsOf(r.Diagnostics))
			}
			if c := f.lastConn(); c.Port != 2222 || c.User != "ops" {
				t.Errorf("connection %+v", c)
			}
		})
	}
}

// TestVMRetireRetry: an update tries a failed retirement again only when nothing else retires the
// key: a later boot did (no SSH), a release change's reboot will (none); a re-run alone tries, and
// says when it fails again.
func TestVMRetireRetry(t *testing.T) {
	t.Parallel()
	withSeed := func(t *testing.T) (*harness, *vmFake, *tfprotov6.DynamicValue) {
		h, f := newVMHarness(t)
		f.retireNoop = true
		cfg := h.vmConfig("1.1.0", nil)
		a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
		if vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
			t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
		}
		f.reset()
		return h, f, a.NewState
	}
	rerun := func(h *harness) tftypes.Value {
		return h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	}
	// A re-run alone, the retirement failing again: a warning, the key still accepted.
	h, f, state := withSeed(t)
	a := h.update(state, rerun(h))
	if d := errorsOf(a.Diagnostics); d != "" || !slices.Contains(f.ops(), "retire") || !strings.Contains(diagText(a.Diagnostics), "still not retired") || vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Errorf("%s seed_boot_id %s", diagText(a.Diagnostics), vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	// A later boot already retired it: cleared without a retirement run.
	h, f, state = withSeed(t)
	f.retired = true
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
	a = h.update(state, rerun(h))
	if d := errorsOf(a.Diagnostics); d != "" || slices.Contains(f.ops(), "retire") || vmAttr(t, h, a.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("%s ops %q seed_boot_id %s", d, f.ops(), vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	// A release change: its reboot retires it, no retirement run.
	h, f, state = withSeed(t)
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	a = h.update(state, h.vmConfig("1.2.0", nil))
	if d := errorsOf(a.Diagnostics); d != "" || slices.Contains(f.ops(), "retire") || vmAttr(t, h, a.NewState, "seed_boot_id") != "<null>" {
		t.Errorf("%s ops %q", d, f.ops())
	}
}

// TestVMNoUpgradeWithoutPlan: a VM that runs another release than state says (a refresh skipped)
// is not upgraded by an apply whose plan does not change vm.version.
func TestVMNoUpgradeWithoutPlan(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("describe", reply{stdout: describeOf("1.2.0", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	state := h.create(h.vmConfig("1.2.0", nil))
	f.reset()
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}) // fell back since
	f.set("describe", reply{stdout: vmDescribe})
	a := h.update(state, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if d := errorsOf(a.Diagnostics); d != "" || slices.Contains(f.ops(), "upgrade") || asString(t, h.attrs(a.NewState)["version"]) != "1.1.0" {
		t.Errorf("%s ops %q version %s", d, f.ops(), asString(t, h.attrs(a.NewState)["version"]))
	}
}

// TestVMRetireCut: a retirement the resource's timeout cuts says so.
func TestVMRetireCut(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("retire", reply{stderr: hang})
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("200ms")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := diagText(a.Diagnostics); !strings.Contains(got, "cut when the timeout (the resource's timeouts block) ran out") {
		t.Errorf("got %s", got)
	}
}

// TestInconsistencies: the harness's check fires on what OpenTofu refuses: a known planned value
// applied otherwise, at the top or nested, and an unknown left in the applied state.
func TestInconsistencies(t *testing.T) {
	t.Parallel()
	h, _ := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	vals := h.attrs(state)
	with := func(name string, v tftypes.Value) *tfprotov6.DynamicValue {
		m := map[string]tftypes.Value{}
		for k, x := range vals {
			m[k] = x
		}
		m[name] = v
		d := h.dv(h.typ, tftypes.NewValue(h.typ, m))
		return &d
	}
	if got := h.inconsistencies(state, state, true); len(got) != 0 {
		t.Errorf("a state against itself: %q", got)
	}
	if got := h.inconsistencies(state, with("status", str("failed: x")), true); len(got) != 1 || !strings.Contains(got[0], `AttributeName("status") planned`) {
		t.Errorf("a top-level value: %q", got)
	}
	nested := h.vmBlock("1.1.0", map[string]tftypes.Value{"host_key_line": str("other"), "seed_boot_id": tftypes.NewValue(tftypes.String, nil)})
	if got := h.inconsistencies(state, with("vm", nested), true); len(got) == 0 || !strings.Contains(strings.Join(got, " "), `AttributeName("vm").AttributeName("host_key_line") planned`) {
		t.Errorf("a nested value: %q", got)
	}
	if got := h.inconsistencies(state, with("vm", tftypes.NewValue(h.typ.AttributeTypes["vm"], nil)), true); len(got) == 0 || !strings.Contains(strings.Join(got, " "), "absent") {
		t.Errorf("a block gone: %q", got)
	}
	// A planned block known in part: its known leaves are still held.
	partly := with("vm", h.vmBlock("1.1.0", map[string]tftypes.Value{"host_key_line": str(ownLine), "seed_boot_id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
	if got := h.inconsistencies(partly, with("vm", nested), true); len(got) == 0 || !strings.Contains(strings.Join(got, " "), "host_key_line") {
		t.Errorf("a known leaf of a partly known block: %q", got)
	}
	mapOf := func(m map[string]tftypes.Value) tftypes.Value {
		return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, m)
	}
	planIn := with("inputs", mapOf(map[string]tftypes.Value{"A": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}))
	if got := h.inconsistencies(planIn, with("inputs", mapOf(map[string]tftypes.Value{"A": str("x"), "B": str("y")})), true); len(got) == 0 || !strings.Contains(strings.Join(got, " "), "planned 1 elements, then 2") {
		t.Errorf("a collection grown: %q", got)
	}
	if got := h.inconsistencies(state, with("status", tftypes.NewValue(tftypes.String, tftypes.UnknownValue)), true); len(got) == 0 || !strings.Contains(strings.Join(got, " "), "unknown value") {
		t.Errorf("an unknown applied: %q", got)
	}
}

// TestVMRetiredKeyDropped: once a followed reboot retired the one-time key, the rest of the
// update — the new release's describe and its re-run, which may carry secret inputs — goes over
// a connection that no longer accepts it.
func TestVMRetiredKeyDropped(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	f.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", `{"key":"NEW_IN_12"}`)})
	up := h.update(a.NewState, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_IN_12": "x"})}))
	if d := errorsOf(up.Diagnostics); d != "" {
		t.Fatal(d)
	}
	after := slices.Index(f.ops(), "upgrade")
	for i, op := range f.ops() {
		if i > after && (op == "describe" || op == "install") && len(f.callConns[i].AlsoKnownHostsLines) != 0 {
			t.Errorf("%s after the reboot over %+v", op, f.callConns[i])
		}
	}
}

// TestVMOneRetryPerApply: a retirement that failed in the update's key step is not tried again by
// an upgrade with nothing to do in the same apply.
func TestVMOneRetryPerApply(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
	f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	f.reset()
	f.retireNoop = true
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	f.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1}}`})
	up := h.update(a.NewState, h.vmConfig("1.2.0", nil))
	retires := 0
	for _, op := range f.ops() {
		if op == "retire" {
			retires++
		}
	}
	if d := errorsOf(up.Diagnostics); d != "" || retires != 1 || strings.Count(diagText(up.Diagnostics), "not retired") != 1 {
		t.Errorf("%s retires %d", diagText(up.Diagnostics), retires)
	}
}

// TestVMRetiredKeyDroppedNoReboot: a retirement an upgrade with nothing to do makes drops the
// one-time key from the connection before the new release's describe and re-run.
func TestVMRetiredKeyDroppedNoReboot(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	f.reset()
	f.retireNoop = false
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	f.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1}}`})
	f.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", `{"key":"NEW_IN_12"}`)})
	// The key step leaves the retirement to the release change: release() makes it.
	up := h.update(a.NewState, h.vmConfig("1.2.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b", "NEW_IN_12": "x"})}))
	if d := errorsOf(up.Diagnostics); d != "" || vmAttr(t, h, up.NewState, "seed_boot_id") != "<null>" {
		t.Fatalf("%s seed_boot_id %s", d, vmAttr(t, h, up.NewState, "seed_boot_id"))
	}
	retired := slices.Index(f.ops(), "retire")
	for i, op := range f.ops() {
		if i > retired && (op == "describe" || op == "install") && len(f.callConns[i].AlsoKnownHostsLines) != 0 {
			t.Errorf("%s after the retirement over %+v", op, f.callConns[i])
		}
	}
}

// TestBreachesSet: a set planned in part may shrink (its unknown elements coalescing) but never
// grow, and each element planned wholly known is in it; a list's elements are held one by one.
func TestBreachesSet(t *testing.T) {
	t.Parallel()
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"s": tftypes.Set{ElementType: tftypes.String}, "l": tftypes.List{ElementType: tftypes.String}}}
	obj := func(s, l []tftypes.Value) tftypes.Value {
		return tftypes.NewValue(typ, map[string]tftypes.Value{
			"s": tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, s),
			"l": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, l),
		})
	}
	unknown := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	list := []tftypes.Value{str("a"), str("x")}
	planned := obj([]tftypes.Value{str("a"), unknown}, []tftypes.Value{str("a"), unknown})
	for name, c := range map[string]struct {
		s    []tftypes.Value
		l    []tftypes.Value
		want string
	}{
		"the known element and another":  {[]tftypes.Value{str("a"), str("c")}, list, ""},
		"coalesced to the known element": {[]tftypes.Value{str("a")}, list, ""},
		"the known element gone":         {[]tftypes.Value{str("b"), str("c")}, list, "does not correlate"},
		"grown":                          {[]tftypes.Value{str("a"), str("b"), str("c")}, list, "planned 2 elements, then 3"},
		"a list's known element changed": {[]tftypes.Value{str("a")}, []tftypes.Value{str("z"), str("x")}, "ElementKeyInt(0)"},
		"an element not planned":         {[]tftypes.Value{str("a"), str("b")}, list, ""},
	} {
		got := strings.Join(breaches(planned, obj(c.s, c.l), true), " ")
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// TestApplyBreaches: an apply that fails still returns a wholly known state, and a destroy that
// succeeds returns none.
func TestApplyBreaches(t *testing.T) {
	t.Parallel()
	h, _ := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	vals := h.attrs(state)
	vals["status"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	withUnknown := h.dv(h.typ, tftypes.NewValue(h.typ, vals))
	if got := h.applyBreaches(state, &withUnknown, false); len(got) != 1 || !strings.Contains(got[0], "unknown value") {
		t.Errorf("an unknown on an error path: %q", got)
	}
	null := h.dv(h.typ, tftypes.NewValue(h.typ, nil))
	if got := h.applyBreaches(&null, state, true); len(got) == 0 {
		t.Error("a destroy returning a state passes")
	}
	if got := h.applyBreaches(&null, &null, true); len(got) != 0 {
		t.Errorf("a destroy returning none: %q", got)
	}
	if got := h.applyBreaches(state, nil, true); len(got) == 0 {
		t.Error("an update succeeding with no state passes")
	}
	if got := h.applyBreaches(state, &null, true); len(got) != 1 || !strings.Contains(got[0], "succeeded with no state") {
		t.Errorf("an update succeeding with a null state: %q", got)
	}
	// A set's element known in part correlates by its known part.
	et := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"x": tftypes.String, "y": tftypes.String}}
	st := tftypes.Set{ElementType: et}
	el := func(x, y tftypes.Value) tftypes.Value {
		return tftypes.NewValue(et, map[string]tftypes.Value{"x": x, "y": y})
	}
	planned := tftypes.NewValue(st, []tftypes.Value{el(str("1"), tftypes.NewValue(tftypes.String, tftypes.UnknownValue))})
	if got := breaches(planned, tftypes.NewValue(st, []tftypes.Value{el(str("2"), str("3"))}), true); len(got) == 0 {
		t.Error("a set element whose known part changed passes")
	}
	if got := breaches(planned, tftypes.NewValue(st, []tftypes.Value{el(str("1"), str("3"))}), true); len(got) != 0 {
		t.Errorf("a set element whose unknown part was filled: %q", got)
	}
	// Each planned element matched, one applied element matching none.
	two := tftypes.NewValue(st, []tftypes.Value{el(str("1"), tftypes.NewValue(tftypes.String, tftypes.UnknownValue)), el(str("1"), str("9"))})
	if got := breaches(two, tftypes.NewValue(st, []tftypes.Value{el(str("1"), str("9")), el(str("2"), str("3"))}), true); len(got) != 1 || !strings.Contains(got[0], "was not planned") {
		t.Errorf("an applied element not planned: %q", got)
	}
	// The proposal OpenTofu makes: a computed attribute config leaves null takes prior's.
	cfg := h.vmConfig("1.1.0", nil)
	prop := h.proposedNew(h.value(state), cfg)
	var pa, va map[string]tftypes.Value
	_ = prop.As(&pa)
	_ = pa["vm"].As(&va)
	if !pa["id"].Equal(str("vm:192.0.2.7/notes-stow")) || !va["host_key_line"].Equal(str(ownLine)) || !pa["inputs"].Equal(strMap(map[string]string{"ADMIN_EMAIL": "a@b"})) {
		t.Errorf("proposed %v", prop)
	}
}

// TestApplyRevalidates: an apply validates and plans its configuration again, as OpenTofu does
// once every value is known: a refusal there stops it, and the warnings of both reach the apply.
func TestApplyRevalidates(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	planned := h.plan(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})).PlannedState
	f.reset()
	bad := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("a/b")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	if got := errorsOf(h.apply(state, planned, bad).Diagnostics); !strings.Contains(got, "not an installer's short name") || len(f.ops()) != 0 {
		t.Errorf("an apply config refused by validation: %s %q", got, f.ops())
	}
	// A final plan's refusal keeps the validation's warnings.
	warnedBad := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("nt")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"}), "secret_inputs_wo": strMap(map[string]string{"API_KEY": "k"})})
	if got := diagText(h.apply(state, planned, warnedBad).Diagnostics); !strings.Contains(got, "first-install only") || !strings.Contains(got, "secret_inputs_wo_version is not set") {
		t.Errorf("a final plan's refusal: %s", got)
	}
	warned := h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"}),
		"secret_inputs_wo": strMap(map[string]string{"API_KEY": "k"})})
	a := h.apply(state, h.plan(state, warned).PlannedState, warned)
	if errorsOf(a.Diagnostics) != "" || !strings.Contains(diagText(a.Diagnostics), "secret_inputs_wo_version is not set") {
		t.Errorf("the validation's warning: %s", diagText(a.Diagnostics))
	}
}

// TestVMCreateChannel: a create whose vm.channel is not the VM's is refused before the key is
// learnt, saving no state; a VM reporting no channel asks to leave it out.
func TestVMCreateChannel(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	cfg := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"channel": str("beta")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, `Set vm.channel to "stable"`) || slices.Contains(f.ops(), "hostkey") || h.attrs(a.NewState) != nil {
		t.Errorf("%s ops %q", got, f.ops())
	}
	ok := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"channel": str("stable")})})
	if s := h.create(ok); asString(t, h.attrs(s)["channel"]) != "stable" {
		t.Errorf("state %v", h.attrs(s))
	}
	h2, f2 := newVMHarness(t)
	f2.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"channel":"stable",`, "", 1)})
	a = h2.apply(nil, h2.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "reports no channel") || !strings.Contains(got, "Leave vm.channel out") || slices.Contains(f2.ops(), "hostkey") || h2.attrs(a.NewState) != nil {
		t.Errorf("no channel: %s ops %q", got, f2.ops())
	}
	// The same on an imported state's first apply, before anything runs.
	h3, f3 := newVMHarness(t)
	f3.retired = true
	f3.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"channel":"stable",`, "", 1)})
	resp, _ := h3.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: "vm:192.0.2.7/notes-stow"})
	imp := resp.ImportedResources[0].State
	icfg := fill(h3.typ, map[string]tftypes.Value{"vm": h3.vmBlock("1.1.0", map[string]tftypes.Value{"channel": str("beta"), "known_hosts_line": str(ownLine)})})
	if got := errorsOf(h3.apply(imp, h3.plan(imp, icfg).PlannedState, icfg).Diagnostics); !strings.Contains(got, "reports no channel") || slices.Contains(f3.ops(), "hostkey") || slices.Contains(f3.ops(), "install") {
		t.Errorf("an imported state, no channel: %s ops %q", got, f3.ops())
	}
}

// TestVMRenamedInstaller: a VM whose shell does not find the installer state names (a release
// that renamed it, an import under a wrong name) fails its read with the route to take the install
// over; a program the installer runs missing (exit 127 too, the installer found) is no rename.
func TestVMRenamedInstaller(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	f.set("status", reply{exit: 127, stderr: "zsh:1: command not found: notes-stow"})
	f.set("probe", reply{exit: 1})
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "The VM has no notes-stow") || !strings.Contains(got, "tofu import") || !strings.Contains(got, "/usr/sbin/*-stow") {
		t.Errorf("got %s", got)
	}
	if p := f.callsOf("probe"); len(p) != 1 || p[0].argv[len(p[0].argv)-1] != "notes-stow" {
		t.Errorf("probe %q", p)
	}
	// tcsh gives a missing command exit 1: the probe tells it the same way.
	f.set("status", reply{exit: 1, stderr: "notes-stow: Command not found."})
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "The VM has no notes-stow") {
		t.Errorf("tcsh: %s", got)
	}
	// ssh's own failure is not looked into: no probe.
	f.reset()
	f.set("status", reply{exit: 255, stderr: "Connection refused"})
	if got := errorsOf(h.read(state).Diagnostics); strings.Contains(got, "tofu import") || slices.Contains(f.ops(), "probe") {
		t.Errorf("ssh's failure: %s ops %q", got, f.ops())
	}
	// A probe that fails says so.
	f.set("status", reply{exit: 127, stderr: ""})
	f.set("probe", reply{exit: 255, stderr: "Connection closed"})
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "could not be checked") || strings.Contains(got, "tofu import") {
		t.Errorf("a failed probe: %s", got)
	}
	f.set("probe", reply{})
	f.set("status", reply{exit: 127, stderr: "sh: jq: not found"})
	if got := errorsOf(h.read(state).Diagnostics); strings.Contains(got, "tofu import") || !strings.Contains(got, "exit 127") {
		t.Errorf("another program missing: %s", got)
	}
	// An import under a wrong name: its first apply's read says so, before any key or install.
	h2, f2 := newVMHarness(t)
	f2.retired = true
	resp, _ := h2.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: "stowage_install", ID: "vm:192.0.2.7/notse-stow"})
	f2.set("status", reply{exit: 127, stderr: "sh: notse-stow: not found"})
	f2.set("probe", reply{exit: 1})
	cfg := fill(h2.typ, map[string]tftypes.Value{"vm": h2.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("notse"), "known_hosts_line": str(ownLine)})})
	imp := resp.ImportedResources[0].State
	got := errorsOf(h2.apply(imp, h2.plan(imp, cfg).PlannedState, cfg).Diagnostics)
	if !strings.Contains(got, "The VM has no notse-stow") || !strings.Contains(got, "tofu import") || slices.Contains(f2.ops(), "hostkey") || slices.Contains(f2.ops(), "install") {
		t.Errorf("an import under a wrong name: %s ops %q", got, f2.ops())
	}
	// A create given another short name than the seed's: named so, nothing run after, no state.
	h3, f3 := newVMHarness(t)
	f3.set("describe", reply{exit: 127})
	f3.set("probe", reply{exit: 1})
	bad := fill(h3.typ, map[string]tftypes.Value{"vm": h3.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("nt")})})
	a := h3.apply(nil, h3.plan(nil, bad).PlannedState, bad)
	got = errorsOf(a.Diagnostics)
	if !strings.Contains(got, "stowage_vm_seed's short_name") || strings.Contains(got, "made by Stowage before") || strings.Contains(got, "The VM's shell:") ||
		!slices.Equal(f3.ops(), []string{"describe", "probe"}) || h3.attrs(a.NewState) != nil {
		t.Errorf("a create with another short name: %s ops %q", got, f3.ops())
	}
	// A describe exiting 127 with the installer found: neither a rename nor an old installer.
	h4, f4 := newVMHarness(t)
	f4.set("describe", reply{exit: 127, stderr: "sh: jq: not found"})
	a = h4.apply(nil, h4.plan(nil, h4.vmConfig("1.1.0", nil)).PlannedState, h4.vmConfig("1.1.0", nil))
	if got := errorsOf(a.Diagnostics); strings.Contains(got, "made by Stowage before") || strings.Contains(got, "short_name") || !strings.Contains(got, "jq: not found") {
		t.Errorf("a describe exiting 127: %s", got)
	}
}

// TestMergeComputed: the proposal takes prior's computed values in single and list blocks and in
// nested attributes; config's own values stand.
func TestMergeComputed(t *testing.T) {
	t.Parallel()
	inner := &tfprotov6.SchemaBlock{Attributes: []*tfprotov6.SchemaAttribute{
		{Name: "c", Type: tftypes.String, Computed: true}, {Name: "o", Type: tftypes.String, Optional: true}}}
	it := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"c": tftypes.String, "o": tftypes.String}}
	b := &tfprotov6.SchemaBlock{
		Attributes: []*tfprotov6.SchemaAttribute{{Name: "n", NestedType: &tfprotov6.SchemaObject{Nesting: tfprotov6.SchemaObjectNestingModeSingle, Attributes: inner.Attributes}}},
		BlockTypes: []*tfprotov6.SchemaNestedBlock{{TypeName: "l", Nesting: tfprotov6.SchemaNestedBlockNestingModeList, Block: inner}},
	}
	typ := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"n": it, "l": tftypes.List{ElementType: it}}}
	el := func(c, o tftypes.Value) tftypes.Value {
		return tftypes.NewValue(it, map[string]tftypes.Value{"c": c, "o": o})
	}
	null := tftypes.NewValue(tftypes.String, nil)
	all := func(n tftypes.Value, l ...tftypes.Value) tftypes.Value {
		return tftypes.NewValue(typ, map[string]tftypes.Value{"n": n, "l": tftypes.NewValue(tftypes.List{ElementType: it}, l)})
	}
	prior := all(el(str("pn"), str("x")), el(str("p0"), str("a")), el(str("p1"), str("b")))
	config := all(el(null, str("y")), el(null, str("a")), el(null, str("b")), el(null, str("c")))
	want := all(el(str("pn"), str("y")), el(str("p0"), str("a")), el(str("p1"), str("b")), el(null, str("c")))
	if got, err := mergeComputed(b, prior, config); err != nil || !got.Equal(want) {
		t.Errorf("got %v %v\nwant %v", got, err, want)
	}
	set := &tfprotov6.SchemaBlock{BlockTypes: []*tfprotov6.SchemaNestedBlock{{TypeName: "l", Nesting: tfprotov6.SchemaNestedBlockNestingModeSet, Block: inner}}}
	if _, err := mergeComputed(set, prior, config); err == nil {
		t.Error("a set block is merged as if modelled")
	}
}

// TestVMFirstBootMissingCtl: a status that fails in the first-boot wait because the installer is
// missing stops the wait at once, as the vm.short_name error.
func TestVMFirstBootMissingCtl(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("status", reply{exit: 127, stderr: "sh: notes-stow: not found"})
	f.set("probe", reply{exit: 1})
	start := time.Now()
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("10s")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "The VM has no notes-stow") || time.Since(start) > 5*time.Second {
		t.Errorf("%s (after %s)", got, time.Since(start))
	}
}

// TestHarnessAppliesOnlyPlans: the harness applies no plan that failed, nor none, as OpenTofu.
func TestHarnessAppliesOnlyPlans(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	bad := fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"short_name": str("nt")})})
	p := h.plan(state, bad)
	if errorsOf(p.Diagnostics) == "" {
		t.Fatal("the plan did not fail")
	}
	f.reset()
	if got := errorsOf(h.apply(state, p.PlannedState, h.vmConfig("1.1.0", nil)).Diagnostics); !strings.Contains(got, "the plan applied failed") || len(f.ops()) != 0 {
		t.Errorf("a failed plan: %s %q", got, f.ops())
	}
	if got := errorsOf(h.apply(state, nil, h.vmConfig("1.1.0", nil)).Diagnostics); !strings.Contains(got, "no plan to apply") {
		t.Errorf("no plan: %s", got)
	}
}

// A VM's status leaves registry_ca out when it cannot read it: a read keeps state's digest, one
// that gives it records it.
func TestVMRegistryCADigest(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	if v := h.attrs(state)["registry_ca_digest"]; !v.IsNull() {
		t.Fatalf("no CA: %v", v)
	}
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", `,"registry_ca":"sha256:bb"`)})
	read := h.read(state).NewState
	if got := asString(t, h.attrs(read)["registry_ca_digest"]); got != "sha256:bb" {
		t.Fatalf("given: %q", got)
	}
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "", "")})
	if got := asString(t, h.attrs(h.read(read).NewState)["registry_ca_digest"]); got != "sha256:bb" {
		t.Fatalf("left out: %q", got)
	}
}

// TestVMRetireProofCut: a retirement proof the resource's timeout cuts says so, not its own bound.
func TestVMRetireProofCut(t *testing.T) {
	t.Parallel()
	h, f := newVMHarnessPoll(t, 100*time.Millisecond)
	f.retireNoop = true
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("250ms")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := diagText(a.Diagnostics); !strings.Contains(got, "the resource's timeouts block") || strings.Contains(got, "six polls") {
		t.Errorf("got %s", got)
	}
}

// A retry of the retirement compares the VM's key with state's by the key alone (a vm.host
// changed in place is the same key); a VM serving another key is an error that stops the update,
// as neither key state accepts is served any more.
func TestVMRetireRetryComparesTheKey(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*harness, *vmFake, *tfprotov6.DynamicValue) {
		h, f := newVMHarness(t)
		f.retireNoop = true
		f.then("status", reply{stdout: vmStatusJSON("1.1.0", "", "")}, reply{stdout: vmStatusJSON("1.1.0", "", "")})
		cfg := h.vmConfig("1.1.0", nil)
		a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
		if vmAttr(t, h, a.NewState, "seed_boot_id") != unreadBoot {
			t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
		}
		f.reset()
		f.retireNoop = false
		return h, f, a.NewState
	}
	h, f, state := setup(t)
	f.set("retire", reply{stdout: `{"format":1,"known_hosts_line":"NOTES.example. ssh-ed25519 ` + ownKey + `","fingerprint":"SHA256:own"}`})
	moved := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"host": str("vm.example")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})
	up := h.update(state, moved)
	if d := errorsOf(up.Diagnostics); d != "" || vmAttr(t, h, up.NewState, "seed_boot_id") != "<null>" {
		t.Fatalf("another host field, the same key: %s seed_boot_id %s", d, vmAttr(t, h, up.NewState, "seed_boot_id"))
	}
	h, f, state = setup(t)
	f.set("retire", reply{stdout: `{"format":1,"known_hosts_line":"notes ssh-ed25519 ` + edKey + `","fingerprint":"SHA256:x"}`})
	got := errorsOf(h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})})).Diagnostics)
	if !strings.Contains(got, "The VM's own host key changed") || len(f.callsOf("install")) != 0 || slices.Contains(f.ops(), "hostkey") {
		t.Fatalf("another key: %q, calls %q", got, f.ops())
	}
}

// When only the proof fails, the verb's key is state's and the warning says the files are retired
// (no reboot needed); an installer without the verb is named, the key read alone.
func TestVMRetireWarnings(t *testing.T) {
	t.Parallel()
	h2, f2 := newVMHarness(t)
	f2.retireNoop = true
	cfg := h2.vmConfig("1.1.0", nil)
	ap := h2.apply(nil, h2.plan(nil, cfg).PlannedState, cfg)
	if d := diagText(ap.Diagnostics); !strings.Contains(d, "host-key retired the one-time key's files") || vmAttr(t, h2, ap.NewState, "host_key_line") != ownLine {
		t.Fatalf("proof alone: %s %s", d, vmAttr(t, h2, ap.NewState, "host_key_line"))
	}
	h3, f3 := newVMHarness(t)
	f3.set("retire", reply{exit: 1, stderr: `Error: unknown command "host-key" for "notes-stow"`})
	ap = h3.apply(nil, h3.plan(nil, cfg).PlannedState, cfg)
	if d := diagText(ap.Diagnostics); !strings.Contains(d, "has no host-key verb") || vmAttr(t, h3, ap.NewState, "host_key_line") != ownLine || vmAttr(t, h3, ap.NewState, "seed_boot_id") == "<null>" {
		t.Fatalf("no verb: %s %s", d, vmAttr(t, h3, ap.NewState, "host_key_line"))
	}
}

// An upgrade with nothing to do retries the retirement after it; a VM serving another key then
// stops the update with an error, nothing more run, state as before.
func TestVMUpgradeNothingStopsOnAnotherKey(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.retireNoop = true
	cfg := h.vmConfig("1.1.0", nil)
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" {
		t.Fatalf("seed_boot_id %s", vmAttr(t, h, a.NewState, "seed_boot_id"))
	}
	f.reset()
	f.retireNoop = false
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-1", "")})
	f.set("upgrade", reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1}}`})
	f.set("retire", reply{stdout: `{"format":1,"known_hosts_line":"notes ssh-ed25519 ` + edKey + `","fingerprint":"SHA256:x"}`})
	up := h.update(a.NewState, h.vmConfig("1.2.0", nil))
	if got := errorsOf(up.Diagnostics); !strings.Contains(got, "The VM's own host key changed") || !strings.Contains(got, "vm:<host>/<short>-stow") || len(f.callsOf("install")) != 0 {
		t.Fatalf("got %q, calls %q", got, f.ops())
	}
}

// A state line that does not parse is said so, not taken for another key.
func TestSameKeyAndAMalformedStateLine(t *testing.T) {
	t.Parallel()
	if !sameKey("a ssh-ed25519 "+ownKey, "B.example ssh-ed25519 "+ownKey) || sameKey("a ssh-ed25519 "+ownKey, "a ssh-ed25519 "+edKey) {
		t.Fatal("sameKey")
	}
	f := newVMFake()
	m := installModel{VM: &vmModel{Host: types.StringValue("192.0.2.7"), User: types.StringValue("ops"), KnownHostsLine: types.StringValue(seedLine), ShortName: types.StringValue("notes")}}
	r := &installResource{data: &Data{NewRunner: f.factory, Poll: time.Millisecond}}
	v, err := r.vm(&m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.retire(context.Background(), &m, v, "192.0.2.7", types.StringValue("not a line")); err == nil || !strings.Contains(err.Error(), "state's vm.host_key_line") || errors.Is(err, errOtherKey) {
		t.Fatalf("%v", err)
	}
}

// A create whose timeout runs out during host-key reads nothing more on the dead context, and its
// warning names the fix.
func TestVMRetireCutByTheTimeout(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("retire", reply{stderr: hang})
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("300ms")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	d := diagText(a.Diagnostics)
	if !strings.Contains(d, "The VM's own host key was not read") || !strings.Contains(d, "ssh-keygen -lf") || strings.Contains(d, "reading it alone") {
		t.Fatalf("%s, calls %q", d, f.ops())
	}
}
