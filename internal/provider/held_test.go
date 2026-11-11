package provider

import (
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const statusHeld = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,"inputs":["ADMIN_EMAIL"],` +
	`"registry":"mirror.lan/notes","short_name":"notes","address":"127.0.0.1","registry_ca":"sha256:aa"}`

// Read records what the install holds that status reports: the registry CA's digest, the short
// name, and an address or a registry state names otherwise (the next plan then shows it).
func TestReadRecordsWhatTheInstallHolds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes")})
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2")}))
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"short_name":"notes"`, `"short_name":"nt"`, 1)})
	at := h.attrs(h.read(state).NewState)
	if asString(t, at["registry_ca_digest"]) != "sha256:aa" || asString(t, at["short_name"]) != "nt" || asString(t, at["address"]) != "127.0.0.1" {
		t.Fatalf("state %v", at)
	}
	// The install's registry, when state names another.
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, "mirror.lan/notes", "other.lan/notes", 1)})
	if at := h.attrs(h.read(state).NewState); asString(t, at["registry"]) != "other.lan/notes" {
		t.Fatalf("registry %v", at["registry"])
	}
	// An address left out is the default's: only another is recorded.
	plain := h.create(h.dockerConfig(nil))
	h.fake.set("status", reply{stdout: statusHeld})
	if at := h.attrs(h.read(plain).NewState); !at["address"].IsNull() {
		t.Fatalf("the default address recorded: %v", at["address"])
	}
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, "127.0.0.1", "127.0.0.9", 1)})
	if at := h.attrs(h.read(plain).NewState); asString(t, at["address"]) != "127.0.0.9" {
		t.Fatalf("another address not recorded: %v", at["address"])
	}
}

// The provider's registry_ca is first-install only on Docker: a plan changing it is an error.
func TestPlanRefusesAnotherRegistryCA(t *testing.T) {
	t.Parallel()
	ca := "-----BEGIN CERTIFICATE-----\nA\n"
	h := newHarness(t, map[string]tftypes.Value{"registry_ca": str(ca)})
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, "sha256:aa", caDigest(ca), 1)})
	state := h.create(h.dockerConfig(nil))
	if p := h.plan(state, h.dockerConfig(nil)); errorsOf(p.Diagnostics) != "" {
		t.Fatalf("the same CA: %s", errorsOf(p.Diagnostics))
	}
	for name, cfg := range map[string]map[string]tftypes.Value{
		"another CA": {"registry_ca": str("-----BEGIN CERTIFICATE-----\nB\n")},
		"none":       nil,
	} {
		o := newHarness(t, cfg)
		if got := errorsOf(o.plan(state, o.dockerConfig(nil)).Diagnostics); !strings.Contains(got, "the provider's registry_ca is first-install only") {
			t.Errorf("%s: got %q", name, got)
		}
	}
	// A create with no CA records none, and a plan giving none again passes.
	n := newHarness(t, nil)
	n.fake.set("status", reply{stdout: strings.Replace(statusHeld, `,"registry_ca":"sha256:aa"`, "", 1)})
	none := n.create(n.dockerConfig(nil))
	if got := errorsOf(n.plan(none, n.dockerConfig(nil)).Diagnostics); got != "" {
		t.Fatalf("no CA twice: %q", got)
	}
}

// An imported state's first apply refuses a configuration whose first-install settings are not
// the install's, before anything runs.
func TestImportedFirstApplyComparesTheHeldSettings(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.importState("app.example:/srv/notes/notes-stow").ImportedResources[0].State
	h.fake.set("status", reply{stdout: statusHeld})
	config := h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.5")})
	p := h.plan(state, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	got := errorsOf(h.apply(state, p.PlannedState, config).Diagnostics)
	if !strings.Contains(got, `address is "127.0.0.5", and the install holds "127.0.0.1"`) || slices.Contains(h.fake.ops(), "rerun") {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// A port for a route the running release already has goes to the re-run, even when an upgrade
// follows; a route only the new release has goes to the upgrade.
func TestPortsSplitByTheReleasesRoutes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.reset()
	h.fake.set("describe", reply{stdout: describe2})
	h.fake.then("status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","inputs":[]}`})
	h.fake.set("status", reply{stdout: status2})
	config := h.dockerConfig(map[string]tftypes.Value{"installer": str(image2),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(8081), "api": num(9000)})})
	if d := errorsOf(h.update(state, config).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if f := flags(h.fake.callsOf("rerun")[0].argv); f["--ports"] != "web=8081" {
		t.Fatalf("re-run %q", f)
	}
	if f := flags(h.fake.callsOf("upgrade")[0].argv); f["--ports"] != "api=9000" {
		t.Fatalf("upgrade %q", f)
	}
}

// work_dir left out is the SSH user's home, which the create resolves once (state's ID): a later
// plan naming that directory changes nothing, another is a first-install-only error, and so is
// leaving out a work_dir that was given (the home was never resolved).
func TestWorkDirLeftOutIsTheResolvedHome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	home := func(w tftypes.Value) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"docker": h.block("docker", map[string]tftypes.Value{
			"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "work_dir": w})})
	}
	state := h.create(home(tftypes.NewValue(tftypes.String, nil)))
	if got := asString(t, h.attrs(state)["id"]); got != "/home/ops/notes" || !slices.Contains(h.fake.ops(), "resolve") {
		t.Fatalf("id %q, calls %q", got, h.fake.ops())
	}
	if got := errorsOf(h.plan(state, home(str("/home/ops"))).Diagnostics); got != "" {
		t.Fatalf("the resolved home named: %q", got)
	}
	if got := errorsOf(h.plan(state, home(str("/srv"))).Diagnostics); !strings.Contains(got, `docker.work_dir was "/home/ops" at the install and is now "/srv"`) {
		t.Fatalf("another directory: %q", got)
	}
	given := h.create(h.dockerConfig(nil))
	if got := errorsOf(h.plan(given, home(tftypes.NewValue(tftypes.String, nil))).Diagnostics); !strings.Contains(got, `docker.work_dir was "/srv" at the install and is now "."`) {
		t.Fatalf("a given work_dir left out: %q", got)
	}
	// A home the target does not resolve is an error before anything runs, and saves no state.
	o := newHarness(t, nil)
	o.fake.set("resolve", reply{exit: 1, stderr: "cd: no such directory"})
	cfg := home(tftypes.NewValue(tftypes.String, nil))
	a := o.apply(nil, o.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Reading the work directory") || o.attrs(a.NewState) != nil || slices.Contains(o.fake.ops(), "install") {
		t.Fatalf("got %q", got)
	}
}

// Left out, a first-install-only setting means its default: a create over an install that
// recorded another one is refused too, each named, before anything runs.
func TestCreateOverAnInstallMadeWithOtherDefaults(t *testing.T) {
	t.Parallel()
	desc := strings.Replace(describe1, `"routes":`, `"registry":"ghcr.io/acme/notes","routes":`, 1)
	for name, c := range map[string]struct{ status, want string }{
		"a CA kept":           {`"registry_ca":"sha256:aa"`, "the provider's registry_ca is none, and the install keeps sha256:aa"},
		"another address":     {`"address":"127.0.0.5"`, `address is "127.0.0.1", and the install holds "127.0.0.5"`},
		"a mirror":            {`"registry":"mirror.lan/notes"`, `the provider's registry is "ghcr.io/acme/notes", and the install pulls from "mirror.lan/notes"`},
		"the defaults (pass)": {`"registry":"ghcr.io/acme/notes","address":"127.0.0.1"`, ""},
	} {
		h := newHarness(t, nil)
		h.fake.set("describe", reply{stdout: desc})
		h.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},` + c.status + `}`})
		cfg := h.dockerConfig(nil)
		got := errorsOf(h.apply(nil, h.plan(nil, cfg).PlannedState, cfg).Diagnostics)
		switch {
		case c.want == "" && got != "":
			t.Errorf("%s: %q", name, got)
		case c.want != "" && (!strings.Contains(got, c.want) || slices.Contains(h.fake.ops(), "install")):
			t.Errorf("%s: got %q, calls %q", name, got, h.fake.ops())
		}
	}
}

// An update of an install whose work_dir was left out runs in the home the create resolved, and
// keeps its id.
func TestUpdateKeepsTheResolvedHome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	cfg := func(inputs map[string]string) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(inputs), "docker": h.block("docker", map[string]tftypes.Value{
			"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine)})})
	}
	state := h.create(cfg(map[string]string{"ADMIN_EMAIL": "a@example.com"}))
	h.fake.reset()
	a := h.update(state, cfg(map[string]string{"ADMIN_EMAIL": "b@example.com"}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := asString(t, h.attrs(a.NewState)["id"]); got != "/home/ops/notes" {
		t.Fatalf("id %q", got)
	}
	if rerun := h.fake.callsOf("rerun"); len(rerun) != 1 || rerun[0].argv[4] != "/home/ops" {
		t.Fatalf("re-run %q", rerun)
	}
}
