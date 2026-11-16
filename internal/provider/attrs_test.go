package provider

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const (
	loginAuth = "b3BzOnMzY3IzdC1ORVZFUi1JTi1BUkdWLU9SLVNUQVRF"
	loginJSON = `{"auths":{"registry.example.com":{"auth":"` + loginAuth + `"}}}`
	rootPEM   = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	kitCode   = "kit-code-NEVER-IN-ARGV-OR-STATE"
)

// stateHolds fails t when a state holds s anywhere.
func stateHolds(t *testing.T, h *harness, state *tfprotov6.DynamicValue, s string) {
	t.Helper()
	if strings.Contains(h.value(state).String(), s) {
		t.Errorf("state holds %q", s)
	}
}

// Each type's schema: the model's attributes it takes, the block's flattened, nothing of another
// shape's.
func TestShapeSchemas(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	has := func(kind string, names ...string) {
		t.Helper()
		for _, n := range names {
			if _, ok := h.outers[kind].typ.AttributeTypes[n]; !ok {
				t.Errorf("%s has no %s", kind, n)
			}
		}
	}
	hasNot := func(kind string, names ...string) {
		t.Helper()
		for _, n := range names {
			if _, ok := h.outers[kind].typ.AttributeTypes[n]; ok {
				t.Errorf("%s has %s", kind, n)
			}
		}
	}
	has("docker", "installer", "host", "work_dir", "known_hosts_line", "tls", "acme_ca", "acme_ca_root", "registry_login_wo", "kit_file", "kit_code_wo", "delete_data", "version", "short_name")
	hasNot("docker", "docker", "vm", "kubernetes", "namespace", "interfaces", "platform")
	has("kubernetes", "installer", "namespace", "release", "values", "image_pull_login", "registry_login_wo", "kit_file", "delete_data")
	hasNot("kubernetes", "tls", "domains", "ports", "address", "secrets_wo", "acme_ca", "host")
	has("vm", "host", "user", "version", "short_name", "channel", "host_key_line", "seed_boot_id", "platform", "interfaces", "acme_ca", "registry_login_wo", "kit_file")
	hasNot("vm", "installer", "delete_data", "address", "work_dir", "vm")
	for kind, o := range h.outers {
		for _, a := range o.schema.Block.Attributes {
			if a.Name == "registry_login_wo" || a.Name == "kit_code_wo" || a.Name == "secret_inputs_wo" || a.Name == "given_wo" {
				if !a.WriteOnly {
					t.Errorf("%s.%s is not write-only", kind, a.Name)
				}
			}
		}
	}
	for _, a := range h.outers["vm"].schema.Block.Attributes {
		if (a.Name == "host" || a.Name == "version" || a.Name == "short_name" || a.Name == "user") && !a.Required {
			t.Errorf("vm.%s is not required", a.Name)
		}
	}
}

// A model value goes to a type's and back unchanged; another shape's block is not carried.
func TestShapeRoundTrip(t *testing.T) {
	t.Parallel()
	h, _ := newVMHarness(t)
	in := h.vmConfig("1.1.0", map[string]tftypes.Value{"tls": str("acme"), "acme_ca": str("https://acme.example/dir"), "registry_login_wo_version": num(2)})
	out := h.out("vm", in)
	var o map[string]tftypes.Value
	_ = out.As(&o)
	if asString(t, o["version"]) != "1.1.0" || asString(t, o["host"]) != "192.0.2.7" || asString(t, o["acme_ca"]) != "https://acme.example/dir" {
		t.Fatalf("out %v", o)
	}
	back, err := shapes["vm"].toInner(out, h.typ)
	if err != nil || !back.Equal(in) {
		t.Fatalf("back %v %v", back, err)
	}
	if got := shapes["vm"].outerPath(path.Root("vm").AtName("interfaces").AtMapKey("edge").AtName("address")); !got.Equal(path.Root("interfaces").AtMapKey("edge").AtName("address")) {
		t.Errorf("a block's path: %s", got)
	}
	if got := shapes["vm"].outerPath(path.Root("installer")); len(got.Steps()) != 0 {
		t.Errorf("an attribute the type has not: %s", got)
	}
}

// Each type's import ID: Docker's install directory, Kubernetes's release, the VM's installer —
// another type's prefix is no ID.
func TestShapeImportIDs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for _, c := range []struct{ kind, id, want string }{
		{"docker", "app.example:/srv/notes/notes-stow", ""},
		{"kubernetes", "prod/notes", ""},
		{"kubernetes", "kubernetes:prod/notes", "want <namespace>/<release>"},
		{"vm", "192.0.2.7/notes-stow", ""},
		{"vm", "vm:192.0.2.7/notes-stow", "want <host>/<short>-stow"},
	} {
		resp, err := h.srv.ImportResourceState(context.Background(), &tfprotov6.ImportResourceStateRequest{TypeName: h.outers[c.kind].name, ID: c.id})
		if err != nil {
			t.Fatal(err)
		}
		if got := errorsOf(resp.Diagnostics); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s %s: %q", c.kind, c.id, got)
			continue
		}
		if c.want == "" {
			v, _ := resp.ImportedResources[0].State.Unmarshal(h.outers[c.kind].typ)
			var at map[string]tftypes.Value
			_ = v.As(&at)
			if id := asString(t, at["id"]); c.kind != "docker" && id != c.id {
				t.Errorf("%s: id %s", c.kind, id)
			}
		}
	}
}

// registry_login_wo goes to the first install on standard input alone (never on its command line
// or in state); a new version gives it to a re-run; a version bumped without it is refused.
func TestDockerRegistryLogin(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	with := func(v int64, login tftypes.Value) tftypes.Value {
		return h.dockerConfig(map[string]tftypes.Value{"registry_login_wo": login, "registry_login_wo_version": num(v)})
	}
	state := h.create(with(1, str(loginJSON)))
	in := h.fake.callsOf("install")[0]
	if strings.Contains(strings.Join(in.argv, " "), loginAuth) || untarAll(t, in.stdin)["registry-login-file"] != loginJSON || !strings.Contains(in.argv[5], "registry-login-file") {
		t.Fatalf("install argv %q", in.argv)
	}
	stateHolds(t, h, state, loginAuth)
	h.fake.reset()
	a := h.update(state, with(2, str(loginJSON)))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	rerun := h.fake.callsOf("rerun")
	if len(rerun) != 1 || untarAll(t, rerun[0].stdin)["registry-login-file"] != loginJSON {
		t.Fatalf("calls %q", h.fake.ops())
	}
	stateHolds(t, h, a.NewState, loginAuth)
	h.fake.reset()
	if got := errorsOf(h.update(a.NewState, with(3, tftypes.NewValue(tftypes.String, nil))).Diagnostics); !strings.Contains(got, "registry_login_wo_version without registry_login_wo") || len(h.fake.callsOf("rerun")) != 0 {
		t.Fatalf("a version without the login: %q %q", got, h.fake.ops())
	}
	if got := errorsOf(h.validate(with(1, tftypes.NewValue(tftypes.String, nil)))); !strings.Contains(got, "registry_login_wo_version without registry_login_wo") {
		t.Fatalf("validate: a version without the login: %q", got)
	}
	if got := errorsOf(h.validate(h.dockerConfig(nil))); got != "" {
		t.Fatalf("validate: neither: %q", got)
	}
	if got := errorsOf(h.update(a.NewState, with(3, str(""))).Diagnostics); !strings.Contains(got, "registry_login_wo is not given") || len(h.fake.callsOf("rerun")) != 0 {
		t.Fatalf("a version with an empty login: %q %q", got, h.fake.ops())
	}
	// A refresh reading no login held takes the version out: the next plan sends it again.
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"format":1,`, `"format":1,"values":{"pull_login":false},`, 1)})
	if r := h.read(a.NewState); !h.attrs(r.NewState)["registry_login_wo_version"].IsNull() {
		t.Fatalf("version kept: %v", h.attrs(r.NewState)["registry_login_wo_version"])
	}
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"format":1,`, `"format":1,"values":{"pull_login":true},`, 1)})
	if r := h.read(a.NewState); h.attrs(r.NewState)["registry_login_wo_version"].IsNull() {
		t.Fatal("version taken out with the login held")
	}
}

// acme_ca and acme_ca_root go to the first install (the root a file on standard input), a change
// to a re-run, a removal as none; a refresh reads them back.
func TestDockerAcmeCA(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	cfg := h.dockerConfig(map[string]tftypes.Value{"tls": str("acme"), "acme_ca": str("https://acme.example/dir"), "acme_ca_root": str(rootPEM)})
	state := h.create(cfg)
	in := h.fake.callsOf("install")[0]
	if !slices.Contains(in.argv, "https://acme.example/dir") || untarAll(t, in.stdin)["acme-ca-root"] != rootPEM {
		t.Fatalf("install argv %q", in.argv)
	}
	h.fake.reset()
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"tls": str("acme")}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	r := h.fake.callsOf("rerun")
	if len(r) != 1 || !hasSeq(r[0].argv, "--acme-ca", "none") || !hasSeq(r[0].argv, "--acme-ca-root", "none") {
		t.Fatalf("rerun %q", r)
	}
	// Read back: another root held shows its digest; none held, null.
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"format":1,`, `"format":1,"values":{"tls":"acme","acme_ca":"https://other.example/dir","acme_ca_root":"sha256:bb"},`, 1)})
	at := h.attrs(h.read(state).NewState)
	if asString(t, at["acme_ca"]) != "https://other.example/dir" || asString(t, at["acme_ca_root"]) != "sha256:bb" {
		t.Fatalf("read %v %v", at["acme_ca"], at["acme_ca_root"])
	}
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"format":1,`, `"format":1,"values":{"tls":"acme","acme_ca_root":"`+caDigest(rootPEM)+`"},`, 1)})
	at = h.attrs(h.read(state).NewState)
	if !at["acme_ca"].IsNull() || asString(t, at["acme_ca_root"]) != rootPEM {
		t.Fatalf("read the same root %v %v", at["acme_ca"], at["acme_ca_root"])
	}
}

// A kit and its code go to the first install as files on standard input; a later change of
// kit_file runs nothing, with a warning; the settings it needs are refused at validation.
func TestDockerKit(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	kit := t.TempDir() + "/notes.kit"
	writeTestFile(t, kit, "-----BEGIN AGE ENCRYPTED FILE-----\nx\n")
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"kit_file": str(kit), "kit_code_wo": str(kitCode)}))
	in := h.fake.callsOf("install")[0]
	files := untarAll(t, in.stdin)
	if strings.Contains(strings.Join(in.argv, " "), kitCode) || files["kit-code-file"] != kitCode || !strings.HasPrefix(files["kit"], "-----BEGIN AGE") {
		t.Fatalf("install %q %v", in.argv, files)
	}
	stateHolds(t, h, state, kitCode)
	h.fake.reset()
	other := t.TempDir() + "/other.kit"
	p := h.plan(state, h.dockerConfig(map[string]tftypes.Value{"kit_file": str(other), "kit_code_wo": str(kitCode)}))
	if !strings.Contains(diagText(p.Diagnostics), "kit_file is first-install only") {
		t.Fatalf("plan %s", diagText(p.Diagnostics))
	}
	a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"kit_file": str(other), "kit_code_wo": str(kitCode)}))
	if d := errorsOf(a.Diagnostics); d != "" || len(h.fake.calls) != 0 {
		t.Fatalf("%s %q", d, h.fake.ops())
	}
	for name, c := range map[string]struct {
		extra map[string]tftypes.Value
		want  string
	}{
		"no kit":       {map[string]tftypes.Value{"kit_code_wo": str(kitCode)}, "kit_code_wo without kit_file"},
		"with given":   {map[string]tftypes.Value{"kit_file": str(kit), "kit_code_wo": str(kitCode), "given_wo": str("project: notes\n")}, "kit_file with given_wo"},
		"root not PEM": {map[string]tftypes.Value{"acme_ca_root": str("root")}, "acme_ca_root is not PEM"},
	} {
		if got := diagText(h.validate(h.dockerConfig(c.extra))); !strings.Contains(got, c.want) {
			t.Errorf("%s: %q", name, got)
		}
	}
	h2 := newHarness(t, nil)
	cfg := h2.dockerConfig(map[string]tftypes.Value{"kit_file": str(t.TempDir() + "/missing.kit"), "kit_code_wo": str(kitCode)})
	if got := errorsOf(h2.apply(nil, h2.plan(nil, cfg).PlannedState, cfg).Diagnostics); !strings.Contains(got, "kit_file cannot be read") || len(h2.fake.callsOf("install")) != 0 {
		t.Fatalf("a kit not there: %q", got)
	}
	cfg = h2.dockerConfig(map[string]tftypes.Value{"kit_file": str(kit)})
	if got := errorsOf(h2.apply(nil, h2.plan(nil, cfg).PlannedState, cfg).Diagnostics); !strings.Contains(got, "kit_code_wo is not given") || len(h2.fake.callsOf("install")) != 0 {
		t.Fatalf("a kit without its code at create: %q", got)
	}
	// A create finding an install there re-runs it: the kit, a first install's, is not given.
	h3 := newHarness(t, nil)
	h3.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"address":"127.0.0.1"}`})
	cfg = h3.dockerConfig(map[string]tftypes.Value{"kit_file": str(kit), "kit_code_wo": str(kitCode)})
	a3 := h3.apply(nil, h3.plan(nil, cfg).PlannedState, cfg)
	if d := errorsOf(a3.Diagnostics); d != "" || !strings.Contains(diagText(a3.Diagnostics), "(describe's first_install_only: address, the provider's registry and registry_ca; secrets_wo; kit_file with kit_code_wo, as an install exists) are not given: the install keeps the ones it recorded, which the configuration's match.") ||
		strings.Contains(diagText(a3.Diagnostics), "is given, as nothing is installed yet") {
		t.Fatalf("%s", diagText(a3.Diagnostics))
	}
	if in := h3.fake.callsOf("install")[0]; strings.Contains(string(in.stdin), kitCode) || untarAll(t, in.stdin)["kit"] != "" {
		t.Fatalf("a re-run given the kit: %q %v", in.argv, untarAll(t, in.stdin))
	}
	// A first install left unfinished there (nothing installed yet): its re-run, which finishes
	// it, takes the kit and its code, as the installer's re-run takes --kit.
	h4 := newHarness(t, nil)
	h4.fake.set("new-status", reply{stdout: `{"format":1,"name":"notes","installed":null,"install_failed":"the hook failed","address":"127.0.0.1"}`})
	a4 := h4.apply(nil, h4.plan(nil, cfg).PlannedState, cfg)
	if d := errorsOf(a4.Diagnostics); d != "" || !strings.Contains(diagText(a4.Diagnostics), "(describe's first_install_only: address, the provider's registry and registry_ca; secrets_wo) are not given: the install keeps the ones it recorded, which the configuration's match. kit_file with kit_code_wo is given, as nothing is installed yet.") {
		t.Fatalf("%s", diagText(a4.Diagnostics))
	}
	in = h4.fake.callsOf("install")[0]
	if files := untarAll(t, in.stdin); strings.Contains(strings.Join(in.argv, " "), kitCode) || files["kit-code-file"] != kitCode || !strings.HasPrefix(files["kit"], "-----BEGIN AGE") {
		t.Fatalf("an unfinished install's re-run without the kit: %q %v", in.argv, files)
	}
	// Without kit_file, the warning names no kit, whether an install exists or not.
	for name, status := range map[string]string{
		"installed":  `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1},"address":"127.0.0.1"}`,
		"unfinished": `{"format":1,"name":"notes","installed":null,"install_failed":"the hook failed","address":"127.0.0.1"}`,
	} {
		h5 := newHarness(t, nil)
		h5.fake.set("new-status", reply{stdout: status})
		plain := h5.dockerConfig(nil)
		a5 := h5.apply(nil, h5.plan(nil, plain).PlannedState, plain)
		if got := diagText(a5.Diagnostics); errorsOf(a5.Diagnostics) != "" || !strings.Contains(got, "(describe's first_install_only: address, the provider's registry and registry_ca; secrets_wo) are not given: the install keeps the ones it recorded, which the configuration's match.") ||
			strings.Contains(got, "kit_file") {
			t.Errorf("%s, no kit_file: %s", name, got)
		}
	}
}

// On Kubernetes the login goes to `k8s install --registry-login-file`, a file on standard input;
// with image_pull_login it is refused.
func TestK8sRegistryLoginAndKit(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	kit := t.TempDir() + "/notes.kit"
	writeTestFile(t, kit, "kit")
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"registry_login_wo": str(loginJSON), "registry_login_wo_version": num(1), "kit_file": str(kit), "kit_code_wo": str(kitCode)}))
	in := h.fake.callsOf("k8s-install")[0]
	files := untarAll(t, in.stdin)
	if files["registry-login-file"] != loginJSON || files["kit-code-file"] != kitCode || files["kit"] != "kit" || strings.Contains(strings.Join(in.argv, " "), loginAuth) || contains(in.argv, "--image-pull-login") {
		t.Fatalf("install %q %v", in.argv, files)
	}
	stateHolds(t, h, state, loginAuth)
	h.fake.reset()
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"registry_login_wo": str(loginJSON), "registry_login_wo_version": num(2), "kit_file": str(kit)}))
	up := h.fake.callsOf("k8s-upgrade")
	if d := errorsOf(a.Diagnostics); d != "" || len(up) != 1 || untarAll(t, up[0].stdin)["registry-login-file"] != loginJSON {
		t.Fatalf("%s %q", d, h.fake.ops())
	}
	bad := h.k8sConfig(map[string]tftypes.Value{"registry_login_wo": str(loginJSON), "registry_login_wo_version": num(1),
		"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes"), "image_pull_login": boolean(true)})})
	if got := diagText(h.validate(bad)); !strings.Contains(got, "registry_login_wo with image_pull_login") {
		t.Fatalf("with image_pull_login: %q", got)
	}
}

// On a VM, the login is recorded by `registry-login` before the first install, a file on standard
// input; acme_ca and the kit go to the install.
func TestVMRegistryLoginAcmeKit(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("registry-login", reply{})
	kit := t.TempDir() + "/notes.kit"
	writeTestFile(t, kit, "kit")
	state := h.create(h.vmConfig("1.1.0", map[string]tftypes.Value{"registry_login_wo": str(loginJSON), "registry_login_wo_version": num(1),
		"tls": str("acme"), "acme_ca": str("https://acme.example/dir"), "kit_file": str(kit), "kit_code_wo": str(kitCode)}))
	ops := f.ops()
	li, ii := slices.Index(ops, "registry-login"), slices.Index(ops, "install")
	if li < 0 || ii < li {
		t.Fatalf("ops %q", ops)
	}
	login, in := f.callsOf("registry-login")[0], f.callsOf("install")[0]
	if untarAll(t, login.stdin)["file"] != loginJSON || strings.Contains(strings.Join(login.argv, " "), loginAuth) {
		t.Fatalf("registry-login %q", login.argv)
	}
	files := untarAll(t, in.stdin)
	if !slices.Contains(in.argv, "--acme-ca") || files["kit"] != "kit" || files["kit-code-file"] != kitCode {
		t.Fatalf("install %q %v", in.argv, files)
	}
	stateHolds(t, h, state, loginAuth)
	stateHolds(t, h, state, kitCode)
	f.reset()
	f.set("registry-login", reply{exit: 3, stderr: "error: the login is for registry.example.com, and the VM reads mirror.lan\n"})
	a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"registry_login_wo": str(loginJSON), "registry_login_wo_version": num(2),
		"tls": str("acme"), "acme_ca": str("https://acme.example/dir"), "kit_file": str(kit)}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the VM reads mirror.lan") || len(f.callsOf("install")) != 0 {
		t.Fatalf("a refused login: %q %q", got, f.ops())
	}
}

// interfaces: a NIC added runs `network attach` for it, then reboots the VM and follows the boot;
// with a release change it is refused; static needs its address.
func TestVMInterfaces(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("network", reply{})
	f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
	state := h.create(h.vmConfig("1.1.0", nil))
	f.reset()
	nic := func(mac, mode, addr string) tftypes.Value {
		null := tftypes.NewValue(tftypes.String, nil)
		v := func(s string) tftypes.Value {
			if s == "" {
				return null
			}
			return str(s)
		}
		ot := h.typ.AttributeTypes["vm"].(tftypes.Object).AttributeTypes["interfaces"].(tftypes.Map).ElementType
		return tftypes.NewValue(ot, map[string]tftypes.Value{"mac": str(mac), "mode": v(mode), "address": v(addr)})
	}
	ifaces := func(vals map[string]tftypes.Value) tftypes.Value {
		mt := h.typ.AttributeTypes["vm"].(tftypes.Object).AttributeTypes["interfaces"]
		return tftypes.NewValue(mt, vals)
	}
	with := func(version string, i tftypes.Value) tftypes.Value {
		return fill(h.typ, map[string]tftypes.Value{"vm": h.vmBlock(version, map[string]tftypes.Value{"interfaces": i}), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})})
	}
	edge := ifaces(map[string]tftypes.Value{"edge": nic("02:00:00:00:00:02", "static", "192.0.2.20/24")})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	a := h.update(state, with("1.1.0", edge))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	attach := f.callsOf("network")
	if len(attach) != 1 || !slices.Equal(attach[0].argv[9:], []string{"network", "attach", "--role", "edge", "--mac", "02:00:00:00:00:02", "--mode", "static", "--address", "192.0.2.20/24"}) ||
		len(f.callsOf("reboot")) != 1 || len(f.callsOf("events")) == 0 {
		t.Fatalf("ops %q attach %q", f.ops(), attach)
	}
	f.reset()
	if got := errorsOf(h.plan(a.NewState, with("1.2.0", ifaces(map[string]tftypes.Value{"edge": nic("02:00:00:00:00:03", "", "")}))).Diagnostics); !strings.Contains(got, "interfaces change with version") {
		t.Fatalf("with a release change: %q", got)
	}
	if got := diagText(h.validate(with("1.1.0", ifaces(map[string]tftypes.Value{"edge": nic("02:00:00:00:00:02", "static", "")})))); !strings.Contains(got, "address is required with static") {
		t.Fatalf("static without address: %q", got)
	}
	if got := diagText(h.validate(with("1.1.0", ifaces(map[string]tftypes.Value{"edge": nic("02:00:00:00:00:02", "dhcp", "192.0.2.20/24")})))); !strings.Contains(got, "address without static") {
		t.Fatalf("an address without static: %q", got)
	}
}

// nicValue and nicsValue are the vm block's interfaces of h's model.
func nicValue(h *harness, mac, mode, addr string) tftypes.Value {
	v := func(s string) tftypes.Value {
		if s == "" {
			return tftypes.NewValue(tftypes.String, nil)
		}
		return str(s)
	}
	ot := h.typ.AttributeTypes["vm"].(tftypes.Object).AttributeTypes["interfaces"].(tftypes.Map).ElementType
	return tftypes.NewValue(ot, map[string]tftypes.Value{"mac": str(mac), "mode": v(mode), "address": v(addr)})
}

func nicsValue(h *harness, vals map[string]tftypes.Value) tftypes.Value {
	return tftypes.NewValue(h.typ.AttributeTypes["vm"].(tftypes.Object).AttributeTypes["interfaces"], vals)
}

// A create whose attach fails records the install as planned (consistent with the plan, which
// the harness checks), its interfaces pending: the next plan is an update that attaches them, and
// once attached, no plan proposes them again.
func TestVMCreateAttachFailureIsPending(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	edge := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge})})
	f.set("network", reply{exit: 1, stderr: "the NIC is not there"})
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := diagText(a.Diagnostics); errorsOf(a.Diagnostics) != "" || !strings.Contains(got, "The VM's NICs are not attached") {
		t.Fatalf("create: %q", got)
	}
	if h.attrs(a.NewState)["vm"].IsNull() || len(f.callsOf("network")) != 1 {
		t.Fatalf("ops %q: %s", f.ops(), diagText(a.Diagnostics))
	}
	p := h.plan(a.NewState, config)
	if got := diagText(p.Diagnostics); !strings.Contains(got, "NICs not attached yet") || !strings.Contains(got, "edge") {
		t.Fatalf("plan: %q", got)
	}
	if st := h.attrs(p.PlannedState)["status"]; st.IsKnown() {
		t.Fatalf("no update planned: status %v", st)
	}
	f.reset()
	f.set("network", reply{})
	f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	u := h.apply(a.NewState, p.PlannedState, config)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if attach := f.callsOf("network"); len(attach) != 1 || !slices.Equal(attach[0].argv[9:13], []string{"network", "attach", "--role", "edge"}) || len(f.callsOf("reboot")) != 1 {
		t.Fatalf("ops %q", f.ops())
	}
	p2 := h.plan(u.NewState, config)
	if got := diagText(p2.Diagnostics); strings.Contains(got, "not attached yet") || !h.attrs(p2.PlannedState)["status"].IsKnown() {
		t.Fatalf("still pending: %q", got)
	}
}

// A create that cannot reach the VM under its own key for the attach records the install, its
// interfaces pending, with a warning: never an error, which would taint it and plan its
// replacement. The next plan is an update.
func TestVMCreateRekeyFailureIsPending(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	edge := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge})})
	f.connErr = func(c Connection) error {
		if c.KnownHostsLine == ownLine {
			return errors.New("writing the known_hosts file: disk full")
		}
		return nil
	}
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := diagText(a.Diagnostics); errorsOf(a.Diagnostics) != "" || !strings.Contains(got, "The VM's NICs are not attached") || !strings.Contains(got, "disk full") {
		t.Fatalf("create: %q", got)
	}
	if h.attrs(a.NewState)["vm"].IsNull() || len(f.callsOf("network")) != 0 {
		t.Fatalf("ops %q", f.ops())
	}
	f.connErr = nil
	p := h.plan(a.NewState, config)
	if got := diagText(p.Diagnostics); len(p.RequiresReplace) != 0 || !strings.Contains(got, "NICs not attached yet") {
		t.Fatalf("plan: replace %v, %q", p.RequiresReplace, got)
	}
}

// The update attaching NICs left pending reads the VM's own key when the create could not, and its
// reboot retires the one-time key the create could not: the plan holds host_key_line and
// seed_boot_id unknown, so that update is consistent with it (the harness checks).
func TestVMPendingAttachPlansTheKeysUnknown(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]string{"retire": "retire", "own key": "hostkey"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			edge := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
			config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge})})
			f.set("network", reply{exit: 1, stderr: "the NIC is not there"})
			f.then("retire", reply{exit: 1, stderr: "sudo: a password is required"})
			if fail == "hostkey" {
				f.then("hostkey", reply{exit: 1, stderr: "cat: permission denied"})
			}
			a := h.apply(nil, h.plan(nil, config).PlannedState, config)
			if d := errorsOf(a.Diagnostics); d != "" {
				t.Fatal(d)
			}
			if fail == "retire" && vmAttr(t, h, a.NewState, "seed_boot_id") != "boot-1" || fail == "hostkey" && vmAttr(t, h, a.NewState, "host_key_line") != "<null>" {
				t.Fatalf("the create's keys: %v", h.attrs(a.NewState)["vm"])
			}
			p := h.plan(a.NewState, config)
			for _, k := range []string{"host_key_line", "seed_boot_id"} {
				if v := h.attrs(p.PlannedState)["vm"]; fail == "hostkey" || k == "seed_boot_id" {
					var vm map[string]tftypes.Value
					if err := v.As(&vm); err != nil || vm[k].IsKnown() {
						t.Errorf("%s planned %v", k, vm[k])
					}
				}
			}
			f.reset()
			f.set("network", reply{})
			f.set("hostkey", reply{stdout: "ssh-ed25519 " + ownKey + "\n"})
			f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
			f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
			f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
			u := h.apply(a.NewState, p.PlannedState, config)
			if d := errorsOf(u.Diagnostics); d != "" {
				t.Fatal(d)
			}
			if vmAttr(t, h, u.NewState, "host_key_line") != ownLine || vmAttr(t, h, u.NewState, "seed_boot_id") != "<null>" {
				t.Fatalf("after the attach: %v", h.attrs(u.NewState)["vm"])
			}
		})
	}
}

// An imported VM's first apply reads the NICs the VM names: a role named by the MAC given is taken
// over whatever its addressing (one addressed otherwise is warned about, naming the `network set
// --role` that aligns it), the others attached; NICs that cannot be read are all attached. The
// plan says so.
func TestImportedVMAttachesOnlyNewNICs(t *testing.T) {
	t.Parallel()
	const roles = `{"live":{"addresses":[],"dns":[]},"source":"dhcp","override":null,"roles":[` +
		`{"role":"edge","mac":"02:00:00:00:00:02","addresses":["192.0.2.20/24"],"override":null},` +
		`{"role":"lan","mac":"02:00:00:00:00:09","addresses":["192.0.2.30/24"],"override":null},` +
		`{"role":"san","mac":"","addresses":[],"override":null},` +
		`{"role":"wan","mac":"02:00:00:00:00:05","addresses":["198.51.100.9/24"],"override":null}]}`
	// Every role named by its MAC, wan with the address given.
	named := strings.NewReplacer(`"02:00:00:00:00:09"`, `"02:00:00:00:00:03"`, `"mac":""`, `"mac":"02:00:00:00:00:04"`, `198.51.100.9/24`, `198.51.100.5/24`)
	const realignWan = "- wan: it has 198.51.100.9/24, interfaces gives static 198.51.100.5/24: `sudo notes-stow network set --role wan --mode static --address 198.51.100.5/24`, then `sudo notes-stow network confirm` once the VM answers."
	for name, c := range map[string]struct {
		read    reply
		attach  []string
		warn    string
		realign string // the addressing warning's lines; "" for none
	}{
		// wan is named by its MAC with another address: taken over, with the warning.
		"read":     {reply{stdout: roles}, []string{"lan", "san"}, "", realignWan},
		"not read": {reply{exit: 1, stderr: "the daemon is down"}, []string{"edge", "lan", "san", "wan"}, "The VM's NICs could not be read", ""},
		"none new": {reply{stdout: named.Replace(roles)}, nil, "", ""},
		// edge's override (`network set --role`) is dhcp, static given: taken over, with the warning.
		"override": {reply{stdout: strings.Replace(named.Replace(roles), `"addresses":["192.0.2.20/24"],"override":null`, `"addresses":["192.0.2.20/24"],"override":{"mode":"dhcp"}`, 1)}, nil, "",
			"- edge: its override (`network set --role`) is dhcp, interfaces gives static 192.0.2.20/24: `sudo notes-stow network set --role edge --mode static --address 192.0.2.20/24`, then `sudo notes-stow network confirm` once the VM answers."},
		// lan is static on the VM (its override), dhcp given: taken over, with the warning.
		"static, dhcp given": {reply{stdout: strings.Replace(named.Replace(roles), `"addresses":["192.0.2.30/24"],"override":null`, `"addresses":["192.0.2.30/24"],"override":{"mode":"static","address":"192.0.2.30/24"}`, 1)}, nil, "",
			"- lan: its override (`network set --role`) is static 192.0.2.30/24, interfaces gives dhcp: `sudo notes-stow network set --role lan --mode dhcp`, then `sudo notes-stow network confirm` once the VM answers."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			state := h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
			nics := nicsValue(h, map[string]tftypes.Value{
				"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24"),
				"lan":  nicValue(h, "02:00:00:00:00:03", "dhcp", ""),
				"san":  nicValue(h, "02:00:00:00:00:04", "", ""),
				// The VM names the MAC with another address: attached again.
				"wan": nicValue(h, "02:00:00:00:00:05", "static", "198.51.100.5/24"),
			})
			config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": nics, "known_hosts_line": str(hostLine)})})
			p := h.plan(state, config)
			if got := diagText(p.Diagnostics); !strings.Contains(got, "The first apply reads the VM's NICs") {
				t.Fatalf("plan: %q", got)
			}
			f.set("network-read", c.read)
			// plan_dry_run reads them at plan, and says what the apply attaches.
			dry := h.vmConfig("1.1.0", map[string]tftypes.Value{"plan_dry_run": boolean(true),
				"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": nics, "known_hosts_line": str(hostLine)})})
			got := diagText(h.plan(state, dry).Diagnostics)
			switch {
			case c.warn != "" && !strings.Contains(got, c.warn):
				t.Fatalf("dry run: %q", got)
			case len(c.attach) == 0 && !strings.Contains(got, "The VM names every interface given"):
				t.Fatalf("dry run: %q", got)
			case len(c.attach) > 0 && !strings.Contains(got, "for "+strings.Join(c.attach, ", ")+": the apply attaches them"):
				t.Fatalf("dry run: %q", got)
			}
			if len(f.callsOf("network")) != 0 {
				t.Fatalf("the dry run attached: %q", f.ops())
			}
			realigned := func(got string) bool {
				if c.realign == "" {
					return !strings.Contains(got, "The VM addresses NICs otherwise than interfaces")
				}
				return strings.Contains(got, "The VM addresses NICs otherwise than interfaces") && strings.Contains(got, "run on it:\n"+c.realign)
			}
			if !realigned(got) {
				t.Fatalf("dry run, the addressing warning: %q", got)
			}
			f.reset()
			f.set("network", reply{})
			f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
			f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
			f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
			a := h.apply(state, p.PlannedState, config)
			if d := errorsOf(a.Diagnostics); d != "" {
				t.Fatal(d)
			}
			var attached []string
			for _, call := range f.callsOf("network") {
				attached = append(attached, call.argv[12])
			}
			if !slices.Equal(attached, c.attach) || (len(c.attach) == 0) != (len(f.callsOf("reboot")) == 0) {
				t.Fatalf("attached %q, ops %q", attached, f.ops())
			}
			if c.warn != "" && !strings.Contains(diagText(a.Diagnostics), c.warn) {
				t.Fatalf("no warning: %q", diagText(a.Diagnostics))
			}
			if !realigned(diagText(a.Diagnostics)) {
				t.Fatalf("apply, the addressing warning: %q", diagText(a.Diagnostics))
			}
			// State records the configuration.
			if got := h.attrs(a.NewState)["vm"]; !strings.Contains(got.String(), "198.51.100.5/24") || !strings.Contains(got.String(), "02:00:00:00:00:03") {
				t.Fatalf("state's interfaces: %v", got)
			}
		})
	}
	// With a release change, refused at apply as at plan.
	h, f := newVMHarness(t)
	state := h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
	config := h.vmConfig("1.2.0", map[string]tftypes.Value{"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"known_hosts_line": str(hostLine),
		"interfaces": nicsValue(h, map[string]tftypes.Value{"lan": nicValue(h, "02:00:00:00:00:03", "dhcp", "")})})})
	a := h.apply(state, h.plan(state, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "interfaces change with version") || len(f.callsOf("network")) != 0 || len(f.callsOf("upgrade")) != 0 {
		t.Fatalf("got %q, ops %q", got, f.ops())
	}
	// The VM names lan by its MAC (dhcp, an address): nothing to attach, the release change is not
	// refused, at plan (its dry run) or apply.
	h, f = newVMHarness(t)
	state = h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
	f.set("network-read", reply{stdout: named.Replace(roles)})
	// The update's timeout cuts the upgrade's reboot short: the upgrade ran, which is all this asks.
	dry := h.vmConfig("1.2.0", map[string]tftypes.Value{"plan_dry_run": boolean(true), "timeouts": h.block("timeouts", map[string]tftypes.Value{"update": str("100ms")}),
		"vm": h.vmBlock("1.2.0", map[string]tftypes.Value{"known_hosts_line": str(hostLine),
			"interfaces": nicsValue(h, map[string]tftypes.Value{"lan": nicValue(h, "02:00:00:00:00:03", "dhcp", "")})})})
	p := h.plan(state, dry)
	if got := diagText(p.Diagnostics); strings.Contains(got, "interfaces change with version") || !strings.Contains(got, "The VM names every interface given") {
		t.Fatalf("plan %q", got)
	}
	a = h.apply(state, p.PlannedState, dry)
	if got := errorsOf(a.Diagnostics); strings.Contains(got, "interfaces change with version") || len(f.callsOf("upgrade")) == 0 || len(f.callsOf("network")) != 0 {
		t.Fatalf("got %q, ops %q", got, f.ops())
	}
	// One role to attach with the release change: refused at the dry run as at apply.
	h, f = newVMHarness(t)
	state = h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
	f.set("network-read", reply{stdout: roles})
	if got := errorsOf(h.plan(state, dry).Diagnostics); !strings.Contains(got, "interfaces change with version") {
		t.Fatalf("dry run %q", got)
	}
}

// namedRole: a role the VM names by the MAC given (in any case) is named, whatever its addressing;
// realign: "" with the addressing given — an override's mode and address, else the addresses the
// VM reports — else the line naming the `network set --role` that aligns it.
func TestNamedRoleAndRealign(t *testing.T) {
	t.Parallel()
	const mac = "02:00:00:00:00:02"
	static := installer.Interface{Role: "edge", MAC: mac, Mode: "static", Address: "192.0.2.20/24"}
	dhcp := installer.Interface{Role: "edge", MAC: mac, Mode: "dhcp"}
	none := installer.Interface{Role: "edge", MAC: "02:00:00:00:00:02"}
	role := func(mac string, addrs []string, o *installer.NICAddress) []installer.NICRole {
		return []installer.NICRole{{Role: "edge", MAC: mac, Addresses: addrs, Override: o}}
	}
	const (
		toStatic = "`sudo notes-stow network set --role edge --mode static --address 192.0.2.20/24`, then `sudo notes-stow network confirm` once the VM answers."
		toDHCP   = "`sudo notes-stow network set --role edge --mode dhcp`, then `sudo notes-stow network confirm` once the VM answers."
		toNone   = "`sudo notes-stow network set --role edge --mode none`, then `sudo notes-stow network confirm` once the VM answers."
	)
	for name, c := range map[string]struct {
		roles   []installer.NICRole
		i       installer.Interface
		named   bool
		realign string // the line's end; "" when aligned
	}{
		"static, its address":             {role("02:00:00:00:00:02", []string{"192.0.2.20/24"}, nil), static, true, ""},
		"static, the MAC in another case": {role("02:AB:00:00:00:02", []string{"192.0.2.20/24"}, nil), installer.Interface{Role: "edge", MAC: "02:ab:00:00:00:02", Mode: "static", Address: "192.0.2.20/24"}, true, ""},
		"static, another address":         {role(mac, []string{"192.0.2.21/24"}, nil), static, true, "- edge: it has 192.0.2.21/24, interfaces gives static 192.0.2.20/24: " + toStatic},
		"another MAC":                     {role("02:00:00:00:00:03", []string{"192.0.2.20/24"}, nil), static, false, ""},
		"another role":                    {[]installer.NICRole{{Role: "lan", MAC: mac}}, static, false, ""},
		"no MAC":                          {role("", nil, nil), none, false, ""},
		"none, no address":                {role(mac, nil, nil), none, true, ""},
		"none, an address":                {role(mac, []string{"192.0.2.20/24"}, nil), none, true, "- edge: it has 192.0.2.20/24, interfaces gives none: " + toNone},
		"dhcp, an address":                {role(mac, []string{"192.0.2.99/24"}, nil), dhcp, true, ""},
		"dhcp, no address yet":            {role(mac, nil, nil), dhcp, true, ""},
		"override static, its address":    {role(mac, []string{"192.0.2.20/24"}, &installer.NICAddress{Mode: "static", Address: "192.0.2.20/24"}), static, true, ""},
		"override static, another":        {role(mac, []string{"192.0.2.20/24"}, &installer.NICAddress{Mode: "static", Address: "192.0.2.21/24"}), static, true, "- edge: its override (`network set --role`) is static 192.0.2.21/24, interfaces gives static 192.0.2.20/24: " + toStatic},
		"override dhcp, static given":     {role(mac, []string{"192.0.2.20/24"}, &installer.NICAddress{Mode: "dhcp"}), static, true, "- edge: its override (`network set --role`) is dhcp, interfaces gives static 192.0.2.20/24: " + toStatic},
		"override static, dhcp given":     {role(mac, []string{"192.0.2.20/24"}, &installer.NICAddress{Mode: "static", Address: "192.0.2.20/24"}), dhcp, true, "- edge: its override (`network set --role`) is static 192.0.2.20/24, interfaces gives dhcp: " + toDHCP},
		"override dhcp, dhcp given":       {role(mac, nil, &installer.NICAddress{Mode: "dhcp"}), dhcp, true, ""},
		"override none, none given":       {role(mac, nil, &installer.NICAddress{Mode: "none"}), none, true, ""},
		"override none, dhcp given":       {role(mac, nil, &installer.NICAddress{Mode: "none"}), dhcp, true, "- edge: its override (`network set --role`) is none, interfaces gives dhcp: " + toDHCP},
	} {
		r, ok := namedRole(c.roles, c.i)
		if ok != c.named {
			t.Errorf("%s: named %v, want %v", name, ok, c.named)
			continue
		}
		if !ok {
			continue
		}
		if got := realign(r, c.i, "notes"); got != c.realign {
			t.Errorf("%s: realign %q, want %q", name, got, c.realign)
		}
	}
}

// A first boot whose reset failed in the recovery system fails at once, naming the platform's
// console.
func TestVMResetFailedAtOnce(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("status", reply{stdout: strings.Replace(vmStatusJSON("1.1.0", "boot-1", `,"reset_failed":"kairos-reset.service failed: disk too small"`), `"boot":"active"`, `"boot":"recovery"`, 1)})
	// A timeout far beyond the polls: the failure comes at once, never from the timeout.
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"platform": str("truenas")}),
		"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("3s")})})
	a := h.apply(nil, h.plan(nil, cfg).PlannedState, cfg)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "gave up in Kairos's recovery system") || !strings.Contains(got, "disk too small") || !strings.Contains(got, "its display on TrueNAS") || len(f.callsOf("status")) != 1 {
		t.Fatalf("got %q, ops %q", got, f.ops())
	}
}

// A destroy's plan of a VM's install asks the VM's installer whether a disk set is left for its
// platform, and warns when there is none, before the destroy is approved; it never stops the
// destroy, whose apply asks nothing.
func TestVMDestroyWarnsWithoutADiskSet(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	cfg := h.vmConfig("1.1.0", map[string]tftypes.Value{"deletion_protection": boolean(false), "vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"platform": str("truenas")})})
	state := h.create(cfg)
	f.reset()
	f.set("vm-seed", reply{exit: 3, stdout: `{"format":1,"refused":{"reason":"the registry holds no disk for truenas","fix":"ship --disks"}}`})
	p := h.planDestroy(state)
	if got := diagText(p.Diagnostics); errorsOf(p.Diagnostics) != "" || !strings.Contains(got, "No disk set to make this VM again") || !strings.Contains(got, "no disk for truenas") {
		t.Fatalf("got %q", got)
	}
	seed := f.callsOf("vm-seed")[0]
	if !slices.Contains(seed.argv, "--platform") || !strings.Contains(untarAll(t, seed.stdin)["ssh-key"], ownKey) {
		t.Fatalf("vm seed %q", seed.argv)
	}
	f.reset()
	if d := h.destroy(state); diagText(d.Diagnostics) != "" || len(f.ops()) != 0 {
		t.Fatalf("the destroy's apply: %q, ops %q", diagText(d.Diagnostics), f.ops())
	}
	f.set("vm-seed", reply{stdout: `{"format":1}`})
	if got := diagText(h.planDestroy(state).Diagnostics); got != "" {
		t.Fatalf("a disk set there: %q", got)
	}
	// The check's connection is bounded; a VM that does not answer is not checked, said so.
	f.mu.Lock()
	c := f.conns[len(f.conns)-1]
	f.mu.Unlock()
	if c.ConnectTimeout != diskConnect {
		t.Fatalf("connect timeout %d", c.ConnectTimeout)
	}
	f.set("vm-seed", reply{exit: installer.SSHFailure, stderr: "ssh: connect to host vm.example port 22: Connection timed out"})
	if got := diagText(h.planDestroy(state).Diagnostics); !strings.Contains(got, "was not checked") || !strings.Contains(got, "did not answer over SSH within 10 seconds") {
		t.Fatalf("an unanswering VM: %q", got)
	}
}

// writeTestFile writes s to p, failing t otherwise.
func writeTestFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hasSeq says whether argv holds seq, in a row.
func hasSeq(argv []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(seq)], seq) {
			return true
		}
	}
	return false
}

// A resource type makes its schemas and value types once, not at each request (each of which
// makes a resource).
func TestShapeModelIsMadeOnce(t *testing.T) {
	t.Parallel()
	a := newShapeResource("docker")().(*shapeResource).model(context.Background())
	b := newShapeResource("docker")().(*shapeResource).model(context.Background())
	if reflect.ValueOf(a.schema.Attributes).Pointer() != reflect.ValueOf(b.schema.Attributes).Pointer() {
		t.Fatal("the model made again")
	}
}

// The connection's bound reaches ssh.
func TestDefaultRunnerConnectTimeout(t *testing.T) {
	t.Parallel()
	r, err := DefaultRunner(Connection{Host: "vm.example", User: "ops", KnownHostsLine: hostLine, ConnectTimeout: diskConnect})
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := r.(target.SSH); !ok || s.ConnectTimeout != diskConnect {
		t.Fatalf("%#v", r)
	}
}

// An imported VM's first apply whose attach fails saves the roles it took over as planned and the
// ones it attaches as state had them (none): the next apply attaches only those.
func TestImportedVMAttachFailureKeepsTheTakenOver(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
	nics := nicsValue(h, map[string]tftypes.Value{
		"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24"), // the VM names it: taken over
		"lan":  nicValue(h, "02:00:00:00:00:03", "dhcp", ""),                // new: attached
	})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": nics, "known_hosts_line": str(hostLine)})})
	f.set("network-read", reply{stdout: `{"live":{"addresses":[],"dns":[]},"source":"dhcp","override":null,"roles":[` +
		`{"role":"edge","mac":"02:00:00:00:00:02","addresses":["192.0.2.20/24"],"override":null}]}`})
	f.set("network", reply{exit: 1, stderr: "the NIC is not there"})
	a := h.apply(state, h.plan(state, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Attaching the VM's NICs") {
		t.Fatalf("apply: %q", got)
	}
	if got := h.attrs(a.NewState)["vm"].String(); !strings.Contains(got, "02:00:00:00:00:02") || strings.Contains(got, "02:00:00:00:00:03") {
		t.Fatalf("state's interfaces: %s", got)
	}
	f.reset()
	f.set("network", reply{})
	f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
	f.then("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")})
	u := h.apply(a.NewState, h.plan(a.NewState, config).PlannedState, config)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	var attached []string
	for _, c := range f.callsOf("network") {
		attached = append(attached, c.argv[12])
	}
	if !slices.Equal(attached, []string{"lan"}) {
		t.Fatalf("attached %q, ops %q", attached, f.ops())
	}
}

// An imported VM's first apply with interfaces waits for its SSH, as the platform restarted it to
// give it the NIC: a read that fails over SSH is tried again.
func TestImportedVMWaitsForSSH(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	state := h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
	nics := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": nics, "known_hosts_line": str(hostLine)})})
	f.set("network-read", reply{stdout: `{"live":{"addresses":[],"dns":[]},"source":"dhcp","override":null,"roles":[` +
		`{"role":"edge","mac":"02:00:00:00:00:02","addresses":["192.0.2.20/24"],"override":null}]}`})
	f.then("status", reply{exit: 255, stderr: "ssh: connect to host 192.0.2.7 port 22: Connection refused"})
	a := h.apply(state, h.plan(state, config).PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if st := f.callsOf("status"); len(st) < 2 || !slices.Contains(st[1].argv, "--values") {
		t.Fatalf("ops %q", f.ops())
	}
}

// An imported VM's wait for its SSH stops at once on a refused login or another host key, at
// plan (plan_dry_run) and at apply, with the read's guidance: no later try answers otherwise.
func TestImportedVMWaitStopsOnARefusal(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ stderr, want string }{
		"login":    {"ops@192.0.2.7: Permission denied (publickey).", "ssh's login to the VM 192.0.2.7 was refused"},
		"host key": {"Host key verification failed.", "serves another host key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			state := h.importState("vm:192.0.2.7/notes-stow").ImportedResources[0].State
			nics := nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
			// The refusal, then a read that answers: a wait that tries again reads it.
			for _, dry := range []bool{true, false} {
				config := h.vmConfig("1.1.0", map[string]tftypes.Value{"plan_dry_run": boolean(dry),
					"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": nics, "known_hosts_line": str(hostLine)})})
				var got string
				if dry {
					f.reset()
					f.then("status", reply{exit: 255, stderr: c.stderr})
					got = errorsOf(h.plan(state, config).Diagnostics)
				} else {
					p := h.plan(state, config)
					f.reset()
					f.then("status", reply{exit: 255, stderr: c.stderr})
					got = errorsOf(h.apply(state, p.PlannedState, config).Diagnostics)
				}
				if !strings.Contains(got, c.want) || !strings.Contains(got, "The install stays in state") || strings.Contains(got, "ran out") || len(f.callsOf("status")) != 1 {
					t.Fatalf("dry run %v: %q, ops %q", dry, got, f.ops())
				}
				if name == "host key" && !strings.Contains(got, "A read accepts the key in state's known_hosts_line alone") {
					t.Fatalf("dry run %v, the key's way: %q", dry, got)
				}
			}
		})
	}
}

// The wait's timeout names its bound: plan_dry_run's at plan, never a restart; the resource's
// timeouts block's at apply.
func TestImportedVMWaitNamesItsBound(t *testing.T) {
	t.Parallel()
	r := &installResource{data: &Data{Poll: time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := installer.VM{Runner: newFake(), Short: "notes"}
	_, err := r.answers(ctx, v, true, true)
	if err == nil || !strings.Contains(err.Error(), "the VM did not answer over SSH when plan_dry_run's bound (10 minutes) ran out") || strings.Contains(err.Error(), "restart") {
		t.Fatalf("at plan: %v", err)
	}
	_, err = r.answers(ctx, v, true, false)
	if err == nil || !strings.Contains(err.Error(), "the VM did not answer over SSH after its restart when "+resourceTimeout+" ran out") {
		t.Fatalf("at apply: %v", err)
	}
}

// A status without boot_id or events refuses the NICs' attach before any is attached: the reboot
// naming them could not be followed.
func TestVMAttachRefusesFirstWithoutABootID(t *testing.T) {
	t.Parallel()
	edge := func(h *harness) tftypes.Value {
		return nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	}
	for name, status := range map[string]string{
		"no boot_id": vmStatusJSON("1.1.0", "", ""),
		"no events":  strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"events":0`, `"events":null`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, f := newVMHarness(t)
			f.set("network", reply{})
			state := h.create(h.vmConfig("1.1.0", nil))
			f.reset()
			f.set("status", reply{stdout: status})
			a := h.update(state, h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge(h)})}))
			if got := errorsOf(a.Diagnostics); !strings.Contains(got, "nothing was attached") || !strings.Contains(got, "`sudo notes-stow status`") ||
				len(f.callsOf("network")) != 0 || len(f.callsOf("reboot")) != 0 {
				t.Fatalf("%q, ops %q", got, f.ops())
			}
		})
	}
}

// NICs attached whose reboot the health gate found unhealthy are recorded attached, nothing
// pending: the create says the verdict in a warning, an update in an error, and the next plan
// attaches nothing.
func TestVMAttachUnhealthyIsAttached(t *testing.T) {
	t.Parallel()
	unhealthy := func(f *vmFake) {
		f.set("network", reply{})
		f.set("reboot", reply{exit: 255, stderr: "Connection closed by remote host"})
		f.unjudged = &reply{stdout: vmStatusJSON("1.1.0", "boot-1", "")}
		f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-2", "")})
		f.set("events", reply{stdout: judgedJSON(5, "boot-2", "1.1.0", "kept_unhealthy", "api not ready", false, "the boot is not on trial: there is nothing to go back to")})
	}
	settled := func(t *testing.T, h *harness, state *tfprotov6.DynamicValue, config tftypes.Value) {
		t.Helper()
		p := h.plan(state, config)
		if got := diagText(p.Diagnostics); strings.Contains(got, "not attached yet") || !h.attrs(p.PlannedState)["status"].IsKnown() {
			t.Fatalf("the next plan: %q", got)
		}
	}
	edge := func(h *harness) tftypes.Value {
		return nicsValue(h, map[string]tftypes.Value{"edge": nicValue(h, "02:00:00:00:00:02", "static", "192.0.2.20/24")})
	}
	t.Run("create", func(t *testing.T) {
		t.Parallel()
		h, f := newVMHarness(t)
		unhealthy(f)
		config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge(h)})})
		a := h.apply(nil, h.plan(nil, config).PlannedState, config)
		if got := warningsOf(a.Diagnostics); errorsOf(a.Diagnostics) != "" || !strings.Contains(got, "The VM came back unhealthy from attaching its NICs") || !strings.Contains(got, "api not ready") ||
			strings.Contains(got, "are not attached") || len(f.callsOf("network")) != 1 {
			t.Fatalf("create: %s %s", errorsOf(a.Diagnostics), got)
		}
		settled(t, h, a.NewState, config)
	})
	t.Run("update", func(t *testing.T) {
		t.Parallel()
		h, f := newVMHarness(t)
		state := h.create(h.vmConfig("1.1.0", nil))
		f.reset()
		unhealthy(f)
		f.judged = false
		config := h.vmConfig("1.1.0", map[string]tftypes.Value{"vm": h.vmBlock("1.1.0", map[string]tftypes.Value{"interfaces": edge(h)})})
		a := h.apply(state, h.plan(state, config).PlannedState, config)
		if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Attaching the VM's NICs") || !strings.Contains(got, "api not ready") || len(f.callsOf("network")) != 1 {
			t.Fatalf("update: %q %q", got, f.ops())
		}
		if got := h.attrs(a.NewState)["vm"].String(); !strings.Contains(got, "02:00:00:00:00:02") {
			t.Fatalf("state's interfaces: %s", got)
		}
		settled(t, h, a.NewState, config)
	})
}
