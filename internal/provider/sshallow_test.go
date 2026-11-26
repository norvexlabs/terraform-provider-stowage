package provider

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// strSet is a set of strings.
func strSet(vs ...string) tftypes.Value {
	typ := tftypes.Set{ElementType: tftypes.String}
	if vs == nil {
		return tftypes.NewValue(typ, nil)
	}
	elems := make([]tftypes.Value, 0, len(vs))
	for _, v := range vs {
		elems = append(elems, str(v))
	}
	return tftypes.NewValue(typ, elems)
}

// sshAnswer is the VM's `ssh-allow --json` answer accepting sources; pending, when not "", its
// pending object.
func sshAnswer(from string, pending string, sources ...string) reply {
	q := make([]string, 0, len(sources))
	for _, s := range sources {
		q = append(q, fmt.Sprintf("%q", s))
	}
	out := fmt.Sprintf(`{"sources":[%s],"from":%q`, strings.Join(q, ","), from)
	if pending != "" {
		out += `,"pending":` + pending
	}
	return reply{stdout: out + "}\n"}
}

const sshPendingJSON = `{"previous":["0.0.0.0/0","::/0"],"deadline":"2026-11-26T10:00:00Z","secs_left":90}`

// sshConfig is a VM's configuration with ssh_allow sources (none: null).
func (h *harness) sshConfig(sources ...string) tftypes.Value {
	return h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"ssh_allow": strSet(sources...)})})
}

// sshAllowOf is state's vm.ssh_allow, sorted; nil when null.
func sshAllowOf(t *testing.T, h *harness, state *tfprotov6.DynamicValue) []string {
	t.Helper()
	var vm map[string]tftypes.Value
	if err := h.attrs(state)["vm"].As(&vm); err != nil {
		t.Fatal(err)
	}
	if vm["ssh_allow"].IsNull() {
		return nil
	}
	var elems []tftypes.Value
	if err := vm["ssh_allow"].As(&elems); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range elems {
		out = append(out, asString(t, e))
	}
	slices.Sort(out)
	return out
}

// sshOps are ops's ssh-allow runs, in order.
func sshOps(ops []string) []string {
	return slices.DeleteFunc(slices.Clone(ops), func(o string) bool { return !strings.HasPrefix(o, "ssh-allow") })
}

func TestSSHAllowValidation(t *testing.T) {
	t.Parallel()
	h, _ := newVMHarness(t)
	for _, c := range []struct {
		sources []string
		want    string // "" none
	}{
		{[]string{"192.0.2.0/24", "2001:db8::/32", "0.0.0.0/0", "::/0", "203.0.113.7/32"}, ""},
		{[]string{"192.0.2.9/24"}, `write "192.0.2.0/24"`},
		{[]string{"2001:DB8::/32"}, `write "2001:db8::/32"`},
		{[]string{"192.0.2.7"}, "not a source with its prefix"},
		{[]string{"192.0.2.0/33"}, "not a source with its prefix"},
		{[]string{"::ffff:203.0.113.0/120"}, `IPv4-mapped network, which the VM takes as IPv4: write "203.0.113.0/24"`},
		{[]string{"::ffff:0.0.0.0/64"}, "write the IPv4 network"},
		{[]string{}, "at least 1"},
	} {
		got := errorsOf(h.validate(h.sshConfig(c.sources...)))
		if c.want == "" && got != "" || c.want != "" && !strings.Contains(got, c.want) {
			t.Errorf("%q: %q, want %q", c.sources, got, c.want)
		}
	}
	// Null is valid: not managed.
	if got := errorsOf(h.validate(h.vmConfig("1.1.0", nil))); got != "" {
		t.Errorf("null: %s", got)
	}
}

// TestSSHAllowNullNeverCalls: with ssh_allow left out, no create, read or update runs ssh-allow.
func TestSSHAllowNullNeverCalls(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.create(h.vmConfig("1.1.0", nil))
	if r := h.read(state); errorsOf(r.Diagnostics) != "" {
		t.Fatal(errorsOf(r.Diagnostics))
	}
	a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "c@d"})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := sshOps(f.ops()); len(got) != 0 {
		t.Errorf("ssh-allow run with ssh_allow null: %q", got)
	}
}

// TestSSHAllowCreate: a create sets the sources last, then confirms them in a run of its own;
// sources the VM holds already are not set again.
func TestSSHAllowCreate(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("ssh-allow", sshAnswer("cloud-config", "", "0.0.0.0/0", "::/0"))
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, "198.51.100.0/24", "203.0.113.0/24"))
	f.set("ssh-allow-confirm", sshAnswer("set", "", "198.51.100.0/24", "203.0.113.0/24"))
	state := h.create(h.sshConfig("203.0.113.0/24", "198.51.100.0/24"))
	ops := f.ops()
	if got, want := ops[len(ops)-3:], []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm"}; !slices.Equal(got, want) {
		t.Errorf("ops %q: the last are not %q", ops, want)
	}
	if got, want := f.callsOf("ssh-allow")[0].argv, []string{"sudo", "-n", "notes-stow", "ssh-allow", "--json"}; !slices.Equal(got, want) {
		t.Errorf("read %q", got)
	}
	if got, want := f.callsOf("ssh-allow-set")[0].argv, []string{"sudo", "-n", "notes-stow", "ssh-allow", "set", "198.51.100.0/24", "203.0.113.0/24", "--json"}; !slices.Equal(got, want) {
		t.Errorf("set %q", got)
	}
	if got, want := f.callsOf("ssh-allow-confirm")[0].argv, []string{"sudo", "-n", "notes-stow", "ssh-allow", "confirm", "--json"}; !slices.Equal(got, want) {
		t.Errorf("confirm %q", got)
	}
	if got := sshAllowOf(t, h, state); !slices.Equal(got, []string{"198.51.100.0/24", "203.0.113.0/24"}) {
		t.Errorf("state %q", got)
	}
	if p := h.plan(state, h.sshConfig("203.0.113.0/24", "198.51.100.0/24")); len(p.Diagnostics) != 0 || !h.attrs(p.PlannedState)["status"].IsKnown() {
		t.Errorf("a plan after the create: %s, status %v", diagText(p.Diagnostics), h.attrs(p.PlannedState)["status"])
	}

	// The VM holds them already: read, nothing set.
	h2, f2 := newVMHarness(t)
	f2.set("ssh-allow", sshAnswer("cloud-config", "", "198.51.100.0/24", "203.0.113.0/24"))
	h2.create(h2.sshConfig("203.0.113.0/24", "198.51.100.0/24"))
	if got := sshOps(f2.ops()); !slices.Equal(got, []string{"ssh-allow"}) {
		t.Errorf("equal sources: %q", got)
	}
}

// TestSSHAllowCreateFails: a create that cannot set the sources warns, records them as planned and
// marks them pending; the next plan is an update that sets them.
func TestSSHAllowCreateFails(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("ssh-allow", reply{exit: 1, stderr: `Error: unknown command "ssh-allow" for "notes-stow"`})
	cfg := h.sshConfig("198.51.100.0/24")
	p := h.plan(nil, cfg)
	a := h.apply(nil, p.PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := diagText(a.Diagnostics); !strings.Contains(got, "SSH's sources are not set on the VM") || !strings.Contains(got, "Stowage 0.50.0") {
		t.Errorf("the warning: %s", got)
	}
	if got := sshAllowOf(t, h, a.NewState); !slices.Equal(got, []string{"198.51.100.0/24"}) {
		t.Errorf("state %q", got)
	}
	next := h.plan(a.NewState, cfg)
	if got := diagText(next.Diagnostics); !strings.Contains(got, "SSH's sources not set yet") || h.attrs(next.PlannedState)["status"].IsKnown() {
		t.Errorf("the next plan: %s, status %v", got, h.attrs(next.PlannedState)["status"])
	}
	f.reset()
	f.set("ssh-allow", sshAnswer("cloud-config", "", "0.0.0.0/0", "::/0"))
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, "198.51.100.0/24"))
	f.set("ssh-allow-confirm", sshAnswer("set", "", "198.51.100.0/24"))
	u := h.apply(a.NewState, next.PlannedState, cfg)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := sshOps(f.ops()); !slices.Equal(got, []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm"}) {
		t.Errorf("the update: %q", got)
	}
	if p := h.plan(u.NewState, cfg); strings.Contains(diagText(p.Diagnostics), "not set yet") || !h.attrs(p.PlannedState)["status"].IsKnown() {
		t.Errorf("still pending after the update: %s", diagText(p.Diagnostics))
	}
}

// sshState is a VM install created with ssh_allow sources the VM holds.
func sshState(t *testing.T, sources ...string) (*harness, *vmFake, *tfprotov6.DynamicValue) {
	t.Helper()
	h, f := newVMHarness(t)
	f.set("ssh-allow", sshAnswer("cloud-config", "", sources...))
	state := h.create(h.sshConfig(sources...))
	f.reset()
	return h, f, state
}

// TestSSHAllowUpdate: a change is set and confirmed in place, after the release change's reboot.
func TestSSHAllowUpdate(t *testing.T) {
	t.Parallel()
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, "198.51.100.0/24", "203.0.113.0/24"))
	f.set("ssh-allow-confirm", sshAnswer("set", "", "198.51.100.0/24", "203.0.113.0/24"))
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("status", reply{stdout: vmStatusJSON("1.2.0", "boot-2", "")})
	f.then("describe", reply{stdout: vmDescribe}, reply{stdout: describeOf("1.2.0", "")})
	cfg := h.vmConfig("1.2.0", map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"ssh_allow": strSet("198.51.100.0/24", "203.0.113.0/24")})})
	p := h.plan(state, cfg)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	a := h.apply(state, p.PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	ops := f.ops()
	if got := ops[len(ops)-3:]; !slices.Equal(got, []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm"}) || slices.Index(ops, "upgrade") < 0 || slices.Index(ops, "events") > slices.Index(ops, "ssh-allow") {
		t.Errorf("ops %q: the sources are not set last, after the reboot", ops)
	}
	if got := sshAllowOf(t, h, a.NewState); !slices.Equal(got, []string{"198.51.100.0/24", "203.0.113.0/24"}) || vmAttr(t, h, a.NewState, "version") != "1.2.0" {
		t.Errorf("state %q %s", got, vmAttr(t, h, a.NewState, "version"))
	}

	// Taken out: nothing runs, state forgets them.
	f.reset()
	u := h.update(a.NewState, h.vmConfig("1.2.0", map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", nil)}))
	if d := errorsOf(u.Diagnostics); d != "" || len(sshOps(f.ops())) != 0 || sshAllowOf(t, h, u.NewState) != nil {
		t.Errorf("ssh_allow taken out: %s, ops %q", d, f.ops())
	}
}

// TestSSHAllowConfirmFails: a confirm the VM does not let in names the window and the address;
// one refused otherwise is followed by a revert; either keeps state's sources.
func TestSSHAllowConfirmFails(t *testing.T) {
	t.Parallel()
	next := []string{"203.0.113.0/24"}
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, next...))
	f.set("ssh-allow-confirm", reply{exit: 255, stderr: "ssh: connect to host 192.0.2.7 port 22: Connection timed out"})
	a := h.update(state, h.sshConfig(next...))
	got := errorsOf(a.Diagnostics)
	for _, w := range []string{"refused the new SSH connection", "within 90 s", "address OpenTofu runs from", "0.0.0.0/0, ::/0"} {
		if !strings.Contains(got, w) {
			t.Errorf("an ssh failure at confirm: %q lacks %q", got, w)
		}
	}
	if ops := sshOps(f.ops()); !slices.Equal(ops, []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm"}) {
		t.Errorf("ops %q: a revert tried over a connection the VM refuses", ops)
	}
	if got := sshAllowOf(t, h, a.NewState); !slices.Equal(got, []string{"198.51.100.0/24"}) {
		t.Errorf("state after a failed confirm %q", got)
	}

	h, f, state = sshState(t, "198.51.100.0/24")
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, next...))
	f.set("ssh-allow-confirm", reply{exit: 3, stderr: "error: the change's window ended: the sources before it are back\n"})
	f.set("ssh-allow-revert", sshAnswer("cloud-config", "", "198.51.100.0/24"))
	a = h.update(state, h.sshConfig(next...))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the change's window ended") || !strings.Contains(got, "it was undone") {
		t.Errorf("a refused confirm: %q", got)
	}
	if ops := sshOps(f.ops()); !slices.Equal(ops, []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm", "ssh-allow-revert"}) {
		t.Errorf("ops %q", ops)
	}
	if got, want := f.callsOf("ssh-allow-revert")[0].argv, []string{"sudo", "-n", "notes-stow", "ssh-allow", "revert", "--json"}; !slices.Equal(got, want) {
		t.Errorf("revert %q", got)
	}
	if got := sshAllowOf(t, h, a.NewState); !slices.Equal(got, []string{"198.51.100.0/24"}) {
		t.Errorf("state after a refused confirm %q", got)
	}
	// The revert fails too: both said.
	f.reset()
	f.set("ssh-allow-revert", reply{exit: 1, stderr: "error: the host daemon is not reachable"})
	if got := errorsOf(h.update(state, h.sshConfig(next...)).Diagnostics); !strings.Contains(got, "the change's window ended") || !strings.Contains(got, "Undoing it failed too") || !strings.Contains(got, "not reachable") {
		t.Errorf("a failed revert: %q", got)
	}
}

// TestSSHAllowPending: a change awaiting confirmation on the VM, read or refused at set, fails the
// apply naming confirm and revert, and nothing is set.
func TestSSHAllowPending(t *testing.T) {
	t.Parallel()
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow-set", reply{exit: 1, stderr: "error: a change of SSH's sources awaits confirmation: `notes-stow ssh-allow confirm` keeps it, `notes-stow ssh-allow revert` undoes it\n"})
	a := h.update(state, h.sshConfig("203.0.113.0/24"))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "awaits confirmation") || !strings.Contains(got, "sudo notes-stow ssh-allow confirm") || !strings.Contains(got, "sudo notes-stow ssh-allow revert") {
		t.Errorf("pending at set: %q", got)
	}
	if n := len(f.callsOf("ssh-allow-confirm")); n != 0 {
		t.Errorf("a confirm after a refused set")
	}
	f.reset()
	f.set("ssh-allow", sshAnswer("set", sshPendingJSON, "203.0.113.0/24"))
	a = h.update(state, h.sshConfig("203.0.113.0/24"))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "awaits confirmation on the VM") || !strings.Contains(got, "ssh-allow revert") {
		t.Errorf("pending at the read: %q", got)
	}
	if ops := sshOps(f.ops()); !slices.Equal(ops, []string{"ssh-allow"}) {
		t.Errorf("ops %q", ops)
	}
}

// TestSSHAllowOldStowage: an OS without the verb fails the apply naming the release needed.
func TestSSHAllowOldStowage(t *testing.T) {
	t.Parallel()
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow", reply{exit: 1, stderr: `Error: unknown command "ssh-allow" for "notes-stow"`})
	a := h.update(state, h.sshConfig("203.0.113.0/24"))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "no ssh-allow verb") || !strings.Contains(got, "Stowage 0.50.0") {
		t.Errorf("an old OS: %q", got)
	}
	if got := sshAllowOf(t, h, a.NewState); !slices.Equal(got, []string{"198.51.100.0/24"}) {
		t.Errorf("state %q", got)
	}
	// A refresh warns and keeps state's.
	r := h.read(state)
	if got := diagText(r.Diagnostics); errorsOf(r.Diagnostics) != "" || !strings.Contains(got, "Reading SSH's sources on the VM failed") {
		t.Errorf("a refresh on an old OS: %s", got)
	}
}

// TestSSHAllowRead: a refresh reads the VM's sources (drift), warns on a pending change, and keeps
// state's when the rule file does not read.
func TestSSHAllowRead(t *testing.T) {
	t.Parallel()
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow", sshAnswer("set", "", "192.0.2.0/24", "198.51.100.0/24"))
	r := h.read(state)
	if got := sshAllowOf(t, h, r.NewState); len(r.Diagnostics) != 0 || !slices.Equal(got, []string{"192.0.2.0/24", "198.51.100.0/24"}) {
		t.Errorf("drift: %q %s", got, diagText(r.Diagnostics))
	}
	f.set("ssh-allow", sshAnswer("set", sshPendingJSON, "192.0.2.0/24"))
	r = h.read(state)
	if got := sshAllowOf(t, h, r.NewState); !slices.Equal(got, []string{"192.0.2.0/24"}) || !strings.Contains(diagText(r.Diagnostics), "awaits confirmation on the VM") || errorsOf(r.Diagnostics) != "" {
		t.Errorf("pending: %q %s", got, diagText(r.Diagnostics))
	}
	f.set("ssh-allow", reply{stdout: `{"sources":null,"from":"cloud-config","rules":"/etc/nftables.d/ssh.nft holds lines other than SSH's sources"}`})
	r = h.read(state)
	if got := sshAllowOf(t, h, r.NewState); !slices.Equal(got, []string{"198.51.100.0/24"}) || !strings.Contains(diagText(r.Diagnostics), "holds lines other than SSH's sources") {
		t.Errorf("unread rules: %q %s", got, diagText(r.Diagnostics))
	}
	// Equal: state's kept, no diagnostic.
	f.set("ssh-allow", sshAnswer("set", "", "198.51.100.0/24"))
	if r = h.read(state); len(r.Diagnostics) != 0 || !slices.Equal(sshAllowOf(t, h, r.NewState), []string{"198.51.100.0/24"}) {
		t.Errorf("equal: %s", diagText(r.Diagnostics))
	}
}

// TestSSHAllowPendingAtSetRefused: the VM's refusal (exit 3) of a set while a change awaits
// confirmation is read as that change, naming confirm and revert.
func TestSSHAllowPendingAtSetRefused(t *testing.T) {
	t.Parallel()
	h, f, state := sshState(t, "198.51.100.0/24")
	f.set("ssh-allow-set", reply{exit: 3, stderr: "error: a change of SSH's sources awaits confirmation: `notes-stow ssh-allow confirm` keeps it, `notes-stow ssh-allow revert` undoes it\n"})
	a := h.update(state, h.sshConfig("203.0.113.0/24"))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "awaits confirmation on the VM") || !strings.Contains(got, "sudo notes-stow ssh-allow confirm") || !strings.Contains(got, "sudo notes-stow ssh-allow revert") || strings.Contains(got, "refused:") {
		t.Errorf("pending at set, exit 3: %q", got)
	}
	if len(f.callsOf("ssh-allow-confirm")) != 0 {
		t.Error("a confirm after a refused set")
	}
}

// TestSSHAllowPendingCleared: ssh_allow taken out while a create left it pending clears the mark:
// no plan forces an update for it any more, and nothing runs.
func TestSSHAllowPendingCleared(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("ssh-allow", reply{exit: 1, stderr: `Error: unknown command "ssh-allow" for "notes-stow"`})
	cfg := h.sshConfig("198.51.100.0/24")
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	f.reset()
	none := h.vmConfig("1.1.0", nil)
	u := h.update(a.NewState, none)
	if d := errorsOf(u.Diagnostics); d != "" || len(sshOps(f.ops())) != 0 || sshAllowOf(t, h, u.NewState) != nil {
		t.Fatalf("taken out: %s, ops %q", d, f.ops())
	}
	// Given back equal to the VM's: the mark is gone, so the plan is no update.
	f.set("ssh-allow", sshAnswer("set", "", "198.51.100.0/24"))
	back := h.update(u.NewState, cfg)
	if d := errorsOf(back.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if p := h.plan(back.NewState, cfg); strings.Contains(diagText(p.Diagnostics), "not set yet") || !h.attrs(p.PlannedState)["status"].IsKnown() {
		t.Errorf("pending after it was taken out and given back: %s", diagText(p.Diagnostics))
	}
	if strings.Contains(string(h.privateOf(u.NewState)), sshAllowPendingKey) {
		t.Errorf("the mark is kept in private state: %s", h.privateOf(u.NewState))
	}
}

// TestSSHAllowPendingAfterAttachFails: a create whose NICs' attach fails returns before it sets
// SSH's sources: they stay pending, and the next update attaches the NICs, then sets them.
func TestSSHAllowPendingAfterAttachFails(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	edge := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{
		"interfaces": edge, "ssh_allow": strSet("198.51.100.0/24")})})
	f.set("network", reply{exit: 1, stderr: "the NIC is not there"})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := diagText(a.Diagnostics); errorsOf(a.Diagnostics) != "" || !strings.Contains(got, "The VM's NICs are not attached") {
		t.Fatalf("create: %q", got)
	}
	if ops := sshOps(f.ops()); len(ops) != 0 {
		t.Fatalf("ssh-allow run after a failed attach: %q", ops)
	}
	p := h.plan(a.NewState, cfg)
	if got := diagText(p.Diagnostics); !strings.Contains(got, "SSH's sources not set yet") {
		t.Fatalf("plan: %q", got)
	}
	f.reset()
	f.set("network", reply{})
	f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	f.set("ssh-allow", sshAnswer("cloud-config", "", "0.0.0.0/0", "::/0"))
	f.set("ssh-allow-set", sshAnswer("set", sshPendingJSON, "198.51.100.0/24"))
	f.set("ssh-allow-confirm", sshAnswer("set", "", "198.51.100.0/24"))
	u := h.apply(a.NewState, p.PlannedState, cfg)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	ops := f.ops()
	if got := sshOps(ops); !slices.Equal(got, []string{"ssh-allow", "ssh-allow-set", "ssh-allow-confirm"}) || slices.Index(ops, "network") > slices.Index(ops, "ssh-allow") {
		t.Errorf("the update: %q", ops)
	}
}
