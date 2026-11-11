package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

func TestSchemaValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if d := errorsOf(h.validate(h.dockerConfig(nil))); d != "" {
		t.Fatalf("the clean configuration: %s", d)
	}
	local := fill(h.typ, map[string]tftypes.Value{"installer": str(image), "docker": h.block("docker", nil)})
	if d := errorsOf(h.validate(local)); d != "" {
		t.Fatalf("a local install: %s", d)
	}
	ssh := func(vals map[string]tftypes.Value) tftypes.Value {
		base := map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine)}
		for k, v := range vals {
			base[k] = v
		}
		return h.block("docker", base)
	}
	vmBlock := h.block("vm", map[string]tftypes.Value{"host": str("vm.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "version": str("1.2.0")})
	for name, c := range map[string]struct {
		config tftypes.Value
		want   string
	}{
		"two targets":       {h.dockerConfig(map[string]tftypes.Value{"vm": vmBlock}), "Invalid Attribute Combination"},
		"no target":         {fill(h.typ, map[string]tftypes.Value{"installer": str(image)}), "Exactly one of these attributes must be configured"},
		"installer by tag":  {h.dockerConfig(map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer:1.2.0")}), "installer is not pinned by digest"},
		"short digest":      {h.dockerConfig(map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@sha256:abc")}), "installer is not pinned by digest"},
		"no installer":      {h.dockerConfig(map[string]tftypes.Value{"installer": tftypes.NewValue(tftypes.String, nil)}), "installer is required"},
		"vm with installer": {fill(h.typ, map[string]tftypes.Value{"installer": str(image), "vm": vmBlock}), "installer is not the VM's release"},
		"vm without version": {fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
			"host": str("vm.example"), "user": str("ops"), "known_hosts_line": str(hostLine)})}), "vm.version is required"},
		"k8s without namespace": {fill(h.typ, map[string]tftypes.Value{"installer": str(image), "kubernetes": h.block("kubernetes", nil)}), "kubernetes.namespace is required"},
		"host, no known_hosts":  {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"known_hosts_line": tftypes.NewValue(tftypes.String, nil)})}), "known_hosts_line is required with host"},
		"bad known_hosts":       {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"known_hosts_line": str("@cert-authority * ssh-ed25519 " + edKey)})}), "known_hosts_line does not parse"},
		"host, no user":         {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"user": tftypes.NewValue(tftypes.String, nil)})}), "user is required with host"},
		"host as an option":     {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"host": str("-oProxyCommand=x")})}), "ssh would misread"},
		"local with user":       {fill(h.typ, map[string]tftypes.Value{"installer": str(image), "docker": h.block("docker", map[string]tftypes.Value{"user": str("ops")})}), "user without host"},
		"port out of range":     {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"port": num(0)})}), "between 1 and 65535"},
		"empty work_dir":        {h.dockerConfig(map[string]tftypes.Value{"docker": ssh(map[string]tftypes.Value{"work_dir": str(" ")})}), "work_dir is empty"},
		"bad input key":         {h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"A=B": "x"})}), "not an input key"},
		"input given twice": {h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"K": "x"}),
			"secret_inputs_wo": strMap(map[string]string{"K": "y"}), "secret_inputs_wo_version": num(1)}), "input given twice"},
		"tls mode":      {h.dockerConfig(map[string]tftypes.Value{"tls": str("self")}), "value must be one of"},
		"bad route":     {h.dockerConfig(map[string]tftypes.Value{"domains": strMap(map[string]string{"a,b": "x.lan"})}), "not a route name"},
		"port of route": {h.dockerConfig(map[string]tftypes.Value{"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(70000)})}), "port out of range"},
	} {
		if got := diagText(h.validate(c.config)); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	// A write-only map without its version is applied once only: a warning says so.
	got := h.validate(h.dockerConfig(map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"K": "y"})}))
	if len(got) != 1 || got[0].Severity != tfprotov6.DiagnosticSeverityWarning || !strings.Contains(got[0].Summary, "secret_inputs_wo_version") {
		t.Errorf("secret_inputs_wo without a version: %s", diagText(got))
	}
}

func TestProviderConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for name, c := range map[string]struct {
		vals map[string]tftypes.Value
		want string
	}{
		"username alone": {map[string]tftypes.Value{"username": str("pull")}, "username and password go together"},
		"password alone": {map[string]tftypes.Value{"password": str("pw")}, "username and password go together"},
		"ca not pem":     {map[string]tftypes.Value{"registry_ca": str("hello")}, "registry_ca is not PEM"},
	} {
		cfg := h.dv(h.ptyp, fill(h.ptyp, c.vals))
		resp, err := h.srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &cfg})
		if err != nil || !strings.Contains(diagText(resp.Diagnostics), c.want) {
			t.Errorf("%s: got %v %q, want %q", name, err, diagText(resp.Diagnostics), c.want)
		}
	}
}

func TestCreateOnDocker(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes")})
	state := h.create(h.dockerConfig(map[string]tftypes.Value{
		"inputs":  strMap(map[string]string{"ADMIN_EMAIL": "ops@example.com"}),
		"tls":     str("internal"),
		"domains": strMap(map[string]string{"web": "notes.lan"}),
	}))
	if got := h.fake.ops(); !slices.Equal(got, []string{"fetch", "describe", "new-status", "install", "status"}) {
		t.Fatalf("calls %q", got)
	}
	c := h.fake.conns[0]
	if c.Host != "app.example" || c.User != "ops" || c.KnownHostsLine != hostLine || c.Port != 0 {
		t.Fatalf("connection %+v", c)
	}
	fetch := h.fake.calls[0].argv
	if !slices.Equal(fetch[4:], []string{"/srv", image, "notes", "", ""}) || h.fake.calls[0].stdin != nil {
		t.Fatalf("fetch %q, no login expected", fetch[4:])
	}
	install := h.fake.callsOf("install")[0].argv
	for _, want := range [][]string{{"--set", "ADMIN_EMAIL=ops@example.com"}, {"--tls", "internal"}, {"--domains", "web=notes.lan"}, {"--registry", "mirror.lan/notes"}, {"--no-input", "--json"}} {
		if !slices.Contains(install, want[0]) || install[slices.Index(install, want[0])+1] != want[1] {
			t.Errorf("install argv %q lacks %q", install, want)
		}
	}
	a := h.attrs(state)
	for name, want := range map[string]string{"id": "/srv/notes", "version": "1.2.0", "channel": "stable", "status": "installed", "registry": "mirror.lan/notes", "installer": image} {
		if got := asString(t, a[name]); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !a["previous"].IsNull() {
		t.Errorf("previous %v, want null", a["previous"])
	}
	var protected bool
	if err := a["deletion_protection"].As(&protected); err != nil || !protected {
		t.Errorf("deletion_protection defaults to %v, want true", a["deletion_protection"])
	}
}

func untarAll(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[hdr.Name] = string(data)
	}
}

func TestSecretsNeverInArgvOrState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, map[string]tftypes.Value{"username": str("pull"), "password": str("pw-" + secretVal)})
	config := h.dockerConfig(map[string]tftypes.Value{
		"inputs":                   strMap(map[string]string{"ADMIN_EMAIL": "ops@example.com"}),
		"secret_inputs_wo":         strMap(map[string]string{"SMTP_PASSWORD": "smtp-" + secretVal}),
		"secret_inputs_wo_version": num(1),
		"given_wo":                 str("given-" + secretVal),
		"secrets_wo":               str("export-" + secretVal),
	})
	p := h.plan(nil, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if bytes.Contains(p.PlannedState.MsgPack, []byte(secretVal)) {
		t.Fatal("a secret is in the plan")
	}
	a := h.apply(nil, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if bytes.Contains(a.NewState.MsgPack, []byte(secretVal)) || bytes.Contains(a.NewState.JSON, []byte(secretVal)) {
		t.Fatal("a secret is in the state")
	}
	for _, c := range h.fake.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) {
			t.Fatalf("a secret is on the %s command line: %q", c.op(), c.argv)
		}
	}
	// They reached the target, on standard input.
	login := untarAll(t, h.fake.calls[0].stdin)
	if login["password"] != "pw-"+secretVal || h.fake.calls[0].argv[7] != "registry.example.com" || h.fake.calls[0].argv[8] != "pull" {
		t.Fatalf("login: %q %q", login, h.fake.calls[0].argv[4:])
	}
	files := untarAll(t, h.fake.callsOf("install")[0].stdin)
	if files["answers"] != `SMTP_PASSWORD: "smtp-`+secretVal+`"`+"\n" || files["given"] != "given-"+secretVal || files["secrets"] != "export-"+secretVal {
		t.Fatalf("install files %q", files)
	}
}

func TestSecretInputGivenInInputsIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	config := h.dockerConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"SMTP_PASSWORD": "x"})})
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "SMTP_PASSWORD is a secret input") {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "install") {
		t.Fatal("installed anyway")
	}
}

func TestRefusalIsADiagnostic(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.replies["install"] = reply{
		stdout: "{\n  \"format\": 1,\n  \"result\": \"nothing\",\n  \"refused\": {\n    \"reason\": \"ADMIN_EMAIL is required\",\n    \"fix\": \"give --set ADMIN_EMAIL=…\"\n  }\n}\n",
		stderr: "error: ADMIN_EMAIL is required\n", exit: 3}
	config := h.dockerConfig(nil)
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	// The summary is the reason, the detail the fix: shown, never run.
	if len(a.Diagnostics) != 1 || a.Diagnostics[0].Summary != "ADMIN_EMAIL is required" ||
		!strings.HasPrefix(a.Diagnostics[0].Detail, "give --set ADMIN_EMAIL=…\n\nInstalling notes was refused by the installer (install, exit 3)") {
		t.Fatalf("got %q", errorsOf(a.Diagnostics))
	}
	if ops := h.fake.ops(); ops[len(ops)-1] != "install" {
		t.Fatalf("ran something after the refusal: %q", ops)
	}
	if v, _ := a.NewState.Unmarshal(h.typ); !v.IsNull() {
		t.Fatal("a refused install left state")
	}
	h.fake.replies["install"] = reply{stderr: "compose up failed", exit: 1}
	a = h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "failed (exit 1)") || !strings.Contains(got, "compose up failed") {
		t.Fatalf("failure: %q", got)
	}
	// Exit 3 carries the object: without one it is a failure naming the breach, not a refusal.
	h.fake.replies["install"] = reply{stderr: "error: ADMIN_EMAIL is required", exit: 3}
	a = h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "install failed (exit 3): install --json: no result object: the installer breaks the machine interface") || !strings.Contains(got, "ADMIN_EMAIL is required") {
		t.Fatalf("exit 3 without an object: %q", got)
	}
	if v, _ := a.NewState.Unmarshal(h.typ); !v.IsNull() {
		t.Fatal("a failed install left state")
	}
}

func TestUnknownFormatIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.replies["describe"] = reply{stdout: `{"format":2,"name":"notes","release":{"version":"9.0"}}`}
	config := h.dockerConfig(nil)
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "format 2 is newer than this provider reads") || !strings.Contains(got, "upgrade terraform-provider-stowage") {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "install") {
		t.Fatal("installed with an unknown format")
	}
}

func TestInstallerOfAnotherProject(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.replies["describe"] = reply{stdout: `{"format":1,"name":"other","short_name":"other","release":{"version":"1"}}`}
	config := h.dockerConfig(nil)
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, `describes the project "other"`) {
		t.Fatalf("got %q", got)
	}
}

func TestDeletionProtection(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	if got := errorsOf(h.destroy(state).Diagnostics); !strings.Contains(got, "deletion_protection is on") {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "uninstall") {
		t.Fatal("uninstalled a protected install")
	}

	h2 := newHarness(t, nil)
	state = h2.create(h2.dockerConfig(map[string]tftypes.Value{"deletion_protection": boolean(false), "delete_data": boolean(true)}))
	if got := errorsOf(h2.destroy(state).Diagnostics); got != "" {
		t.Fatal(got)
	}
	last := h2.fake.calls[len(h2.fake.calls)-1]
	if last.op() != "uninstall" || !slices.Equal(last.argv[9:], []string{"uninstall", "--yes", "--no-input", "--json", "--data"}) || last.argv[6] != "notes" {
		t.Fatalf("uninstall %q", last.argv)
	}

	h3 := newHarness(t, nil)
	state = h3.create(h3.dockerConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)}))
	h3.fake.replies["uninstall"] = reply{stdout: `{"format":1,"result":"nothing","release":{"version":"1.2.0","revision":1},"refused":{"reason":"another lifecycle run holds the install's lock","fix":"wait for it to end"}}`, exit: 3}
	if got := errorsOf(h3.destroy(state).Diagnostics); !strings.HasPrefix(got, "another lifecycle run holds the install's lock: wait for it to end\n\nUninstalling was refused") {
		t.Fatalf("refused uninstall: %q", got)
	}
	if last := h3.fake.calls[len(h3.fake.calls)-1]; slices.Contains(last.argv, "--data") {
		t.Fatal("--data without delete_data")
	}
}

func TestReadFollowsTheTarget(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	other := "registry.example.com/notes/notes-installer@" + digest2
	h.fake.replies["status"] = reply{stdout: `{"format":1,"name":"notes","installed":{"version":"1.1.0","revision":3,"installer":"` + other + `"},
		"channel":"stable","install_failed":"provision hook exited 1","previous":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"inputs":[]}`}
	r := h.read(state)
	if d := errorsOf(r.Diagnostics); d != "" {
		t.Fatal(d)
	}
	a := h.attrs(r.NewState)
	if asString(t, a["installer"]) != other || asString(t, a["version"]) != "1.1.0" || asString(t, a["status"]) != "failed: provision hook exited 1" {
		t.Fatalf("read %v", a)
	}
	prev := map[string]tftypes.Value{}
	if err := a["previous"].As(&prev); err != nil || asString(t, prev["installer"]) != image {
		t.Fatalf("previous %v %v", a["previous"], err)
	}

	h.fake.replies["status"] = reply{exit: 100, stderr: "the installer's own exit 100"}
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "status failed (exit 100)") {
		t.Fatalf("an installer's exit 100 taken for a missing install: %q", got)
	}
	h.fake.replies["status"] = reply{exit: 100, stderr: installer.NotInstalledMark + "\n"}
	if r := h.read(state); errorsOf(r.Diagnostics) != "" || h.attrs(r.NewState) != nil {
		t.Fatalf("a missing install stays in state: %v", r.Diagnostics)
	}
	h.fake.replies["status"] = reply{stdout: `{"format":7,"name":"notes"}`}
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "format 7") {
		t.Fatalf("status format: %q", got)
	}
}

func TestFirstInstallOnlyIsAPlanError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2")}))
	moved := h.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "work_dir": str("/opt")})
	for name, c := range map[string]struct {
		config tftypes.Value
		want   string
	}{
		"work_dir": {h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"), "docker": moved}), "docker.work_dir is first-install only"},
		"address":  {h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.3")}), "address is first-install only"},
		"work_dir taken out": {h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"),
			"docker": h.block("docker", map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine)})}),
			`docker.work_dir was "/srv" at the install and is now "."`},
		"project": {h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2"), "installer": str("registry.example.com/x/atlas-installer@" + digest)}), "the installer's project is first-install only"},
		"target": {fill(h.typ, map[string]tftypes.Value{"address": str("127.0.0.2"), "installer": str(image),
			"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes")})}), "the target is first-install only"},
	} {
		if got := errorsOf(h.plan(state, c.config).Diagnostics); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	if got := errorsOf(h.plan(state, h.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2")})).Diagnostics); got != "" {
		t.Fatalf("an unchanged plan: %q", got)
	}
	// The same state planned by a provider given another registry.
	mirror := newHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes")})
	if got := errorsOf(mirror.plan(state, mirror.dockerConfig(map[string]tftypes.Value{"address": str("127.0.0.2")})).Diagnostics); !strings.Contains(got, "the provider's registry is first-install only") {
		t.Fatalf("registry: %q", got)
	}
}

func TestUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	calls := len(h.fake.calls)

	// The provider's own settings change in state alone.
	config := h.dockerConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)})
	p := h.plan(state, config)
	a := h.apply(state, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if len(h.fake.calls) != calls {
		t.Fatalf("the target was called: %q", h.fake.ops()[calls:])
	}
	if asString(t, h.attrs(a.NewState)["version"]) != "1.2.0" {
		t.Fatal("the computed values were lost")
	}

	// A new release plans its version unknown.
	config = h.dockerConfig(map[string]tftypes.Value{"installer": str("registry.example.com/notes/notes-installer@" + digest2)})
	p = h.plan(state, config)
	planned, _ := p.PlannedState.Unmarshal(h.typ)
	vals := map[string]tftypes.Value{}
	_ = planned.As(&vals)
	if vals["version"].IsKnown() {
		t.Fatalf("version planned as %v on a new installer", vals["version"])
	}
}

func TestOtherTargetsAreNotYet(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for name, config := range map[string]tftypes.Value{
		"vm": fill(h.typ, map[string]tftypes.Value{"vm": h.block("vm", map[string]tftypes.Value{
			"host": str("vm.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "version": str("1.2.0")})}),
	} {
		if d := errorsOf(h.validate(config)); d != "" {
			t.Fatalf("%s: %s", name, d)
		}
		p := h.plan(nil, config)
		if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "is not implemented yet") {
			t.Errorf("%s: got %q", name, got)
		}
	}
	if len(h.fake.calls) != 0 {
		t.Fatalf("the target was called: %q", h.fake.ops())
	}
}

// The kept installer's describe runs before uninstall: one without the machine interface stops
// there, naming the Stowage release that has it, and no install directory is an install gone.
func TestDeleteDescribesTheKeptInstallerFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)}))
	h.fake.reset()
	h.fake.set("kept-describe", reply{stderr: `Error: unknown command "describe" for "notes-stow"`, exit: 1})
	got := errorsOf(h.destroy(state).Diagnostics)
	if !strings.Contains(got, "made by Stowage before "+installer.MinStowage) || !strings.Contains(got, `unknown command "describe"`) {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "uninstall") {
		t.Fatal("uninstall ran after a failed describe")
	}

	h.fake.reset()
	h.fake.set("kept-describe", reply{exit: installer.NotInstalled, stderr: installer.NotInstalledMark + "\n"})
	if got := errorsOf(h.destroy(state).Diagnostics); got != "" {
		t.Fatal(got)
	}
	if slices.Contains(h.fake.ops(), "uninstall") {
		t.Fatal("uninstall ran without an install directory")
	}
}

// An installer older than the machine interface has no describe: the create stops there, before
// install is given --json, naming the Stowage release it needs.
func TestOldInstallerStopsAtDescribe(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.replies["describe"] = reply{stderr: `Error: unknown command "describe" for "notes-stow"`, exit: 1}
	config := h.dockerConfig(nil)
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "use one made by Stowage "+installer.MinStowage+" or later") {
		t.Fatalf("got %q", got)
	}
	if slices.Contains(h.fake.ops(), "install") {
		t.Fatal("installed after a failed describe")
	}
}

// A value not known while the configuration is validated — a write-only input from an ephemeral
// variable, an input or port computed from another resource — is not an error: its key is still
// checked.
func TestValidateUnknownMapValues(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	unknown := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	strs := func(m map[string]tftypes.Value) tftypes.Value {
		return tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, m)
	}
	config := h.dockerConfig(map[string]tftypes.Value{
		"inputs":                   strs(map[string]tftypes.Value{"ADMIN_EMAIL": unknown}),
		"secret_inputs_wo":         strs(map[string]tftypes.Value{"SMTP_PASSWORD": unknown}),
		"secret_inputs_wo_version": num(1),
		"tls":                      str("internal"),
		"domains":                  strs(map[string]tftypes.Value{"web": unknown}),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number},
			map[string]tftypes.Value{"web": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)}),
	})
	if got := diagText(h.validate(config)); got != "" {
		t.Fatalf("got %q", got)
	}
	config = h.dockerConfig(map[string]tftypes.Value{
		"inputs":           strs(map[string]tftypes.Value{"bad-key": unknown}),
		"secret_inputs_wo": strs(map[string]tftypes.Value{"ADMIN_EMAIL": unknown, "bad key": unknown}),
		"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number},
			map[string]tftypes.Value{"no route": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)}),
	})
	got := errorsOf(h.validate(config))
	for _, want := range []string{`"bad-key": an input key`, `"bad key": an input key`, `"no route" is not a route name`} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in %q", want, got)
		}
	}
}
