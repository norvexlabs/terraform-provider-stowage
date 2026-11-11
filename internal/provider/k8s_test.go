package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const (
	describeK8s  = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.2.0","revision":1},"channel":"stable","shapes":["docker","kubernetes"],"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"LEGACY"}],"first_install_only":["registry","registry_ca","channel"],"routes":[]}`
	describeK8s2 = `{"format":1,"name":"notes","short_name":"notes","release":{"version":"2.0.0","revision":1},"channel":"stable","shapes":["kubernetes"],"inputs":[{"key":"ADMIN_EMAIL"},{"key":"SMTP_PASSWORD","secret":true},{"key":"NEW_KEY"},{"key":"NEW_TOKEN","secret":true}],"routes":[]}`
	statusK8s    = `{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"channel":"stable","previous":null,"inputs":["ADMIN_EMAIL","SMTP_PASSWORD"],"helm":{"revision":1,"status":"deployed"}}`
	statusK8s2   = `{"format":1,"name":"notes","installed":{"version":"2.0.0","revision":1,"installer":"` + image2 + `"},"channel":"stable","previous":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"inputs":["ADMIN_EMAIL","NEW_KEY"],"helm":{"revision":2,"status":"deployed"}}`
	noRelease    = "error: no release notes in notes: `notes-stow k8s install --namespace notes --release notes` installs 1.2.0\n"
	// noReleaseJSON is the refusal's object k8s status --json prints with it.
	noReleaseJSON = `{"format":1,"refused":{"reason":"no release notes in notes","fix":"notes-stow k8s install --namespace notes --release notes installs 1.2.0","absent":true}}`
	k8sToken      = "tok-NEVER-ON-A-COMMAND-LINE"
)

// k8sProvider is a provider configuration whose kubernetes block reaches the cluster by host and
// token, with extra attributes.
func k8sProvider(t *testing.T, extra map[string]tftypes.Value) map[string]tftypes.Value {
	h := newHarness(t, nil)
	conn := h.ptyp.AttributeTypes["kubernetes"].(tftypes.Object)
	vals := map[string]tftypes.Value{"kubernetes": fill(conn, map[string]tftypes.Value{"host": str("https://k8s.example:6443"), "token": str(k8sToken)})}
	for k, v := range extra {
		vals[k] = v
	}
	return vals
}

// k8sHarness is a harness, its provider reaching a cluster (k8sProvider), whose fake answers the
// Kubernetes calls: the programs found, the installer describing the Kubernetes shape, a release
// installed.
func k8sHarness(t *testing.T, providerConfig map[string]tftypes.Value) *harness {
	h := newHarness(t, k8sProvider(t, providerConfig))
	done := reply{stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`}
	for op, r := range map[string]reply{
		"tools": {}, "k8s-describe": {stdout: describeK8s}, "k8s-status": {stdout: statusK8s}, "k8s-install": done,
		"k8s-upgrade":  {stdout: `{"format":1,"result":"done","release":{"version":"2.0.0","revision":1}}`},
		"k8s-rollback": done, "k8s-uninstall": {stdout: `{"format":1,"result":"done"}`},
	} {
		h.fake.set(op, r)
	}
	return h
}

// k8sConfig is a resource configuration on a cluster reached by host and token, with extra
// attributes.
func (h *harness) k8sConfig(extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{
		"installer":  str(image),
		"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes")}),
	}
	for k, v := range extra {
		vals[k] = v
	}
	return fill(h.typ, vals)
}

func TestK8sValidation(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	if d := errorsOf(h.validate(h.k8sConfig(nil))); d != "" {
		t.Fatalf("the clean configuration: %s", d)
	}
	block := func(vals map[string]tftypes.Value) tftypes.Value {
		return h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", vals)})
	}
	for name, c := range map[string]struct {
		config tftypes.Value
		want   string
	}{
		"no namespace":       {block(nil), "kubernetes.namespace is required"},
		"a namespace's form": {block(map[string]tftypes.Value{"namespace": str("My_ns")}), "kubernetes.namespace is not a DNS label"},
		"a long release":     {block(map[string]tftypes.Value{"namespace": str("n"), "release": str(strings.Repeat("r", 54))}), "kubernetes.release is not a DNS label"},
		"tls":                {h.k8sConfig(map[string]tftypes.Value{"tls": str("internal")}), "tls is not Kubernetes's"},
		"ports":              {h.k8sConfig(map[string]tftypes.Value{"ports": tftypes.NewValue(tftypes.Map{ElementType: tftypes.Number}, map[string]tftypes.Value{"web": num(80)})}), "ports is not Kubernetes's"},
		"a secrets export":   {h.k8sConfig(map[string]tftypes.Value{"secrets_wo": str("x")}), "secrets_wo is not Kubernetes's"},
	} {
		if got := diagText(h.validate(c.config)); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestK8sProviderBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	conn := h.ptyp.AttributeTypes["kubernetes"].(tftypes.Object)
	exec := func(vals map[string]tftypes.Value) tftypes.Value {
		return fill(conn.AttributeTypes["exec"].(tftypes.Object), vals)
	}
	configure := func(vals map[string]tftypes.Value) string {
		cfg := h.dv(h.ptyp, fill(h.ptyp, map[string]tftypes.Value{"kubernetes": fill(conn, vals)}))
		resp, err := h.srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &cfg})
		if err != nil {
			t.Fatal(err)
		}
		return errorsOf(resp.Diagnostics)
	}
	for name, vals := range map[string]map[string]tftypes.Value{
		"a kubeconfig file":  {"config_path": str("~/.kube/config"), "config_context": str("kind-a"), "insecure": boolean(false)},
		"kubectl's default":  {},
		"client certificate": {"host": str("https://k8s.example"), "client_certificate": str("PEM"), "client_key": str("KEY"), "cluster_ca_certificate": str("CA")},
		"exec":               {"host": str("https://k8s.example"), "exec": exec(map[string]tftypes.Value{"api_version": str("client.authentication.k8s.io/v1"), "command": str("aws")})},
	} {
		if d := configure(vals); d != "" {
			t.Errorf("%s: %s", name, d)
		}
	}
	for name, c := range map[string]struct {
		vals map[string]tftypes.Value
		want string
	}{
		"host and file":          {map[string]tftypes.Value{"host": str("https://k8s.example"), "token": str("t"), "config_path": str("k")}, "config_path with host"},
		"token without host":     {map[string]tftypes.Value{"token": str("t")}, "token without host"},
		"insecure without host":  {map[string]tftypes.Value{"insecure": boolean(true)}, "insecure without host"},
		"exec without host":      {map[string]tftypes.Value{"exec": exec(map[string]tftypes.Value{"command": str("aws")})}, "exec without host"},
		"no credential":          {map[string]tftypes.Value{"host": str("https://k8s.example")}, "no credential for host"},
		"certificate alone":      {map[string]tftypes.Value{"host": str("https://k8s.example"), "client_certificate": str("PEM")}, "client_certificate and client_key go together"},
		"insecure with a CA":     {map[string]tftypes.Value{"host": str("https://k8s.example"), "token": str("t"), "insecure": boolean(true), "cluster_ca_certificate": str("CA")}, "insecure with cluster_ca_certificate"},
		"exec without a command": {map[string]tftypes.Value{"host": str("https://k8s.example"), "exec": exec(map[string]tftypes.Value{"api_version": str("client.authentication.k8s.io/v1")})}, "exec.command is required"},
		"host not a URL":         {map[string]tftypes.Value{"host": str("k8s.example"), "token": str("t")}, "host is not a URL"},
	} {
		if got := configure(c.vals); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
}

func TestK8sCreate(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes"), "registry_ca": str("-----BEGIN CERTIFICATE-----\nX\n")})
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{
		"inputs":           strMap(map[string]string{"ADMIN_EMAIL": "ops@example.com"}),
		"secret_inputs_wo": strMap(map[string]string{"SMTP_PASSWORD": secretVal}), "secret_inputs_wo_version": num(1),
	}))
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-describe", "k8s-status", "k8s-install", "k8s-status"}) {
		t.Fatalf("calls %q", got)
	}
	if c := h.fake.conns[0]; !reflect.DeepEqual(c, Connection{}) {
		t.Fatalf("a Kubernetes run is on this machine, not %+v", c)
	}
	install := h.fake.callsOf("k8s-install")[0]
	f := flags(install.argv)
	for flag, want := range map[string]string{"--namespace": "notes", "--set": "ADMIN_EMAIL=ops@example.com", "--image-registry": "mirror.lan/notes"} {
		if f[flag] != want {
			t.Errorf("install %s = %q, want %q (argv %q)", flag, f[flag], want, install.argv)
		}
	}
	if !contains(install.argv, "--no-input") || !contains(install.argv, "--json") || f["--release"] != "" {
		t.Errorf("install argv %q", install.argv)
	}
	files := untarAll(t, install.stdin)
	if install.argv[6] != "answers registry-ca" || files["answers"] != `SMTP_PASSWORD: "`+secretVal+`"`+"\n" || !strings.HasPrefix(files["registry-ca"], "-----BEGIN") {
		t.Fatalf("install files %q %q", install.argv[6], files)
	}
	// Every call reads the kubeconfig written for the run, removed once it ended.
	kc := install.argv[5]
	if !strings.HasSuffix(kc, "/kubeconfig") || !strings.HasPrefix(kc, h.tmp+"/stowage-k8s-") {
		t.Fatalf("kubeconfig %q", kc)
	}
	if _, err := os.Stat(kc); !os.IsNotExist(err) {
		t.Fatalf("the run's kubeconfig is left: %v", err)
	}
	for _, c := range h.fake.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) || strings.Contains(strings.Join(c.argv, " "), k8sToken) {
			t.Fatalf("a secret on the %s command line", c.op())
		}
	}
	// The input keys the installer declares are unset in its run: the environment never gives one.
	if unset := install.unset; !slices.Contains(unset, "SMTP_PASSWORD") || !slices.Contains(unset, "ADMIN_EMAIL") || !slices.Contains(unset, "HELM_KUBEAPISERVER") {
		t.Fatalf("unset %q", install.unset)
	}
	at := h.attrs(state)
	for name, want := range map[string]string{"id": "notes/notes", "version": "1.2.0", "status": "installed", "registry": "mirror.lan/notes", "short_name": "notes"} {
		if got := asString(t, at[name]); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if bytes.Contains(state.MsgPack, []byte(secretVal)) || bytes.Contains(state.MsgPack, []byte(k8sToken)) {
		t.Fatal("a secret input or the cluster's token in state")
	}

	// Read: status --json by the installer state names, after a fetch.
	h.fake.reset()
	r := h.read(state)
	if got := h.fake.ops(); errorsOf(r.Diagnostics) != "" || !slices.Equal(got, []string{"tools", "fetch", "k8s-status"}) {
		t.Fatalf("read: %s %q", errorsOf(r.Diagnostics), got)
	}
	// A namespace without the release: the resource is gone.
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	if r := h.read(state); errorsOf(r.Diagnostics) != "" || !h.attrsNull(r.NewState) {
		t.Fatalf("read of a gone release: %s", errorsOf(r.Diagnostics))
	}
	// Another refusal of status is an error, never a release gone.
	h.fake.then("k8s-status", reply{exit: 3, stderr: "error: Secret notes-stowage was not written by stowage\n"})
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "k8s status failed (exit 3)") {
		t.Fatalf("another refusal: %q", got)
	}
}

func (h *harness) attrsNull(state interface {
	Unmarshal(tftypes.Type) (tftypes.Value, error)
}) bool {
	v, err := state.Unmarshal(h.typ)
	return err == nil && v.IsNull()
}

func TestK8sCreateRefusals(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		setup func(*fake)
		want  string
	}{
		"missing programs": {func(f *fake) { f.set("tools", reply{stdout: "helm\nkubectl\n"}) }, "Missing on this machine: helm, kubectl"},
		"no kubernetes shape": {func(f *fake) {
			f.set("k8s-describe", reply{stdout: strings.Replace(describeK8s, `"docker","kubernetes"`, `"docker"`, 1)})
		}, "offers docker, not kubernetes"},
		"another project's release": {func(f *fake) {
			f.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"name":"notes"`, `"name":"atlas"`, 1)})
		}, "Another project's release"},
		"another installer runs": {func(f *fake) { f.set("k8s-status", reply{stdout: statusK8s2}) }, "The release runs another installer"},
		"the install refused": {func(f *fake) {
			f.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
			f.set("k8s-install", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"a Secret notes-stowage another tool wrote","fix":"delete it"}}`})
		}, "a Secret notes-stowage another tool wrote"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := k8sHarness(t, nil)
			c.setup(h.fake)
			config := h.k8sConfig(nil)
			p := h.plan(nil, config)
			if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// A create over the release it would install takes it over: the inputs given again.
func TestK8sCreateOverTheRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	config := h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "ops@example.com"})})
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" || !strings.Contains(diagText(a.Diagnostics), "A release is already there") {
		t.Fatalf("apply: %s", diagText(a.Diagnostics))
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-describe", "k8s-status", "k8s-upgrade", "k8s-status"}) {
		t.Fatalf("calls %q", got)
	}
	up := h.fake.callsOf("k8s-upgrade")[0]
	if g := untarAll(t, up.stdin)["given"]; g != "project: \"notes\"\ninputs:\n  ADMIN_EMAIL: \"ops@example.com\"\n" || flags(up.argv)["--set"] != "" {
		t.Fatalf("take-over %q %q", up.argv, g)
	}
}

func TestK8sUpdate(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com", "LEGACY": "x"})}))

	// A changed input and a removed one: the running release applied again with them given.
	h.fake.reset()
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status", "k8s-describe", "k8s-upgrade", "k8s-status"}) {
		t.Fatalf("calls %q", got)
	}
	up := h.fake.callsOf("k8s-upgrade")[0]
	if g := untarAll(t, up.stdin)["given"]; g != "project: \"notes\"\ninputs:\n  ADMIN_EMAIL: \"b@example.com\"\n  LEGACY: \"\"\n" || h.fake.callsOf("fetch")[0].argv[5] != image {
		t.Fatalf("reconfigure %q", g)
	}

	// A new installer: the inputs the running release declares by --given, then the new
	// release's upgrade given those it adds.
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	config := h.k8sConfig(map[string]tftypes.Value{
		"installer":        str(image2),
		"inputs":           strMap(map[string]string{"ADMIN_EMAIL": "c@example.com", "NEW_KEY": "added"}),
		"secret_inputs_wo": strMap(map[string]string{"NEW_TOKEN": "tok-" + secretVal}), "secret_inputs_wo_version": num(1),
	})
	a2 := h.update(a.NewState, config)
	if d := errorsOf(a2.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status", "k8s-describe", "fetch", "k8s-describe", "k8s-upgrade", "k8s-status", "k8s-upgrade", "k8s-status"}) {
		t.Fatalf("calls %q", got)
	}
	ups := h.fake.callsOf("k8s-upgrade")
	if g := untarAll(t, ups[0].stdin)["given"]; g != "project: \"notes\"\ninputs:\n  ADMIN_EMAIL: \"c@example.com\"\n" || ups[0].argv[4] == ups[1].argv[4] {
		t.Fatalf("the re-run %q in %s", g, ups[0].argv[4])
	}
	f := flags(ups[1].argv)
	if f["--set"] != "NEW_KEY=added" || untarAll(t, ups[1].stdin)["answers"] != `NEW_TOKEN: "tok-`+secretVal+`"`+"\n" || !strings.HasSuffix(ups[1].argv[4], "/next") {
		t.Fatalf("the upgrade %q %q", ups[1].argv, untarAll(t, ups[1].stdin))
	}
	at := h.attrs(a2.NewState)
	if asString(t, at["installer"]) != image2 || asString(t, at["version"]) != "2.0.0" || at["previous"].IsNull() {
		t.Fatalf("state %v", at)
	}
	if bytes.Contains(a2.NewState.MsgPack, []byte(secretVal)) {
		t.Fatal("a secret in state")
	}

	// Back to the previous installer, with allow_rollback: k8s rollback alone.
	h.fake.reset()
	h.fake.then("k8s-status", reply{stdout: statusK8s2})
	h.fake.set("k8s-status", reply{stdout: statusK8s})
	back := h.k8sConfig(map[string]tftypes.Value{"allow_rollback": boolean(true),
		"inputs":           strMap(map[string]string{"ADMIN_EMAIL": "c@example.com", "NEW_KEY": "added"}),
		"secret_inputs_wo": strMap(map[string]string{"NEW_TOKEN": "tok-" + secretVal}), "secret_inputs_wo_version": num(1)})
	a3 := h.update(a2.NewState, back)
	if d := errorsOf(a3.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status", "k8s-describe", "k8s-rollback", "k8s-status"}) {
		t.Fatalf("rollback calls %q", got)
	}
	if asString(t, h.attrs(a3.NewState)["installer"]) != image {
		t.Fatalf("after the rollback %v", h.attrs(a3.NewState))
	}
}

// A failed upgrade leaves the reconfiguration in state: the next plan shows the upgrade alone.
func TestK8sUpgradeFailureSavesTheReconfiguration(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-upgrade", reply{stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`}, reply{exit: 1, stderr: "helm: timed out"})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Upgrading notes to 2.0.0") {
		t.Fatalf("got %q", got)
	}
	at := h.attrs(a.NewState)
	if asString(t, at["installer"]) != image || inputsOf(t, h, a.NewState)["ADMIN_EMAIL"] != "b@example.com" {
		t.Fatalf("saved %v", at)
	}
}

func TestK8sPlanRefusals(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	for name, c := range map[string]struct {
		config tftypes.Value
		want   string
	}{
		"another namespace": {h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{
			"namespace": str("other")})}), "kubernetes.namespace is first-install only"},
		"another release": {h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{
			"namespace": str("notes"), "release": str("notes2")})}), "the helm release (kubernetes.release, else the project's name) is first-install only"},
		"rollback without allow_rollback": {h.k8sConfig(nil), ""},
	} {
		got := errorsOf(h.plan(state, c.config).Diagnostics)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	// The release named as the installer's default is the same release.
	same := h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{
		"namespace": str("notes"), "release": str("notes")})})
	if got := errorsOf(h.plan(state, same).Diagnostics); got != "" {
		t.Fatalf("the default release named: %q", got)
	}
}

func TestK8sDelete(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false), "delete_data": boolean(true)}))
	h.fake.reset()
	if d := errorsOf(h.destroy(state).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-describe", "k8s-uninstall"}) {
		t.Fatalf("calls %q", got)
	}
	if un := h.fake.callsOf("k8s-uninstall")[0].argv; !contains(un, "--yes") || !contains(un, "--data") || flags(un)["--namespace"] != "notes" {
		t.Fatalf("uninstall %q", un)
	}
	// Gone already, nothing left of it: the destroy is done.
	h.fake.set("k8s-uninstall", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"no release notes in notes, and nothing notes-stow made for it there","fix":"nothing to uninstall"}}`})
	if d := errorsOf(h.destroy(state).Diagnostics); d != "" {
		t.Fatalf("a release gone: %s", d)
	}
	// Its data's Secrets kept: a refusal, shown.
	h.fake.set("k8s-uninstall", reply{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"no release notes in notes, and nothing notes-stow made for it there but the Secrets notes-stowage, which answer for its data","fix":"kubectl delete secret notes-stowage --namespace notes"}}`})
	if got := errorsOf(h.destroy(state).Diagnostics); !strings.Contains(got, "but the Secrets") {
		t.Fatalf("data kept: %q", got)
	}
}

func TestK8sImport(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	resp := h.importState("kubernetes:prod/notes")
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	state := resp.ImportedResources[0].State
	if at := h.attrs(state); asString(t, at["id"]) != "prod/notes" || !at["registry"].IsNull() {
		t.Fatalf("imported %v", at)
	}
	if r := h.read(state); errorsOf(r.Diagnostics) != "" || len(h.fake.calls) != 0 {
		t.Fatalf("read of an imported state: %s %q", errorsOf(r.Diagnostics), h.fake.ops())
	}
	config := h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{
		"namespace": str("prod")}),
		"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})})
	a := h.update(state, config)
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status", "k8s-describe", "k8s-upgrade", "k8s-status"}) {
		t.Fatalf("calls %q", got)
	}
	if at := h.attrs(a.NewState); asString(t, at["registry"]) != "" || asString(t, at["installer"]) != image || asString(t, at["id"]) != "prod/notes" {
		t.Fatalf("state %v", at)
	}
	for _, id := range []string{"kubernetes:prod", "kubernetes:/notes", "kubernetes:prod/notes/x", "kubernetes:Prod/notes"} {
		if got := errorsOf(h.importState(id).Diagnostics); !strings.Contains(got, "kubernetes:<namespace>/<release>") {
			t.Errorf("%s: %q", id, got)
		}
	}
}

func TestKubeconfig(t *testing.T) {
	t.Parallel()
	var diags diag.Diagnostics
	args, _ := types.ListValueFrom(context.Background(), types.StringType, []string{"eks", "get-token"})
	env, _ := types.MapValueFrom(context.Background(), types.StringType, map[string]string{"B": "2", "A": "1"})
	b := &k8sConnModel{Host: types.StringValue("https://k8s.example"), Token: types.StringValue("t"), ClusterCACertificate: types.StringValue("CA PEM"),
		ClientCertificate: types.StringNull(), ClientKey: types.StringNull(), Insecure: types.BoolNull(), TLSServerName: types.StringValue("api.k8s"),
		ProxyURL: types.StringNull(), Exec: &k8sExecModel{APIVersion: types.StringValue("client.authentication.k8s.io/v1"), Command: types.StringValue("aws"), Args: args, Env: env}}
	var got struct {
		Clusters []struct {
			Cluster map[string]any `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User map[string]any `json:"user"`
		} `json:"users"`
		Current string `json:"current-context"`
	}
	if err := json.Unmarshal(kubeconfig(context.Background(), b, &diags), &got); err != nil || diags.HasError() {
		t.Fatal(err, diags)
	}
	c, u := got.Clusters[0].Cluster, got.Users[0].User
	if c["server"] != "https://k8s.example" || c["certificate-authority-data"] != base64.StdEncoding.EncodeToString([]byte("CA PEM")) || c["tls-server-name"] != "api.k8s" || c["insecure-skip-tls-verify"] != nil {
		t.Fatalf("cluster %v", c)
	}
	e := u["exec"].(map[string]any)
	if u["token"] != "t" || e["command"] != "aws" || e["interactiveMode"] != "Never" || len(e["args"].([]any)) != 2 ||
		e["env"].([]any)[0].(map[string]any)["name"] != "A" || got.Current != kubeconfigName {
		t.Fatalf("user %v", u)
	}
}

// The kubeconfig is private to the run: 0600 in a 0700 directory, removed at its close.
func TestK8sRunIsPrivate(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("tools", reply{})
	r := &installResource{data: &Data{NewRunner: f.factory, TempDir: t.TempDir(), Kubernetes: &k8sConnModel{Host: types.StringValue("https://k8s.example"), Token: types.StringValue("t")}}}
	var diags diag.Diagnostics
	m := &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}
	run := r.k8sStart(context.Background(), m, "notes", &diags)
	if run == nil {
		t.Fatal(diags)
	}
	for p, want := range map[string]os.FileMode{run.dir: 0o700 | os.ModeDir, run.k.Kubeconfig: 0o600} {
		if fi, err := os.Stat(p); err != nil || fi.Mode() != want {
			t.Errorf("%s: %v %v, want %v", p, fi.Mode(), err, want)
		}
	}
	run.close()
	if _, err := os.Stat(run.dir); !os.IsNotExist(err) {
		t.Fatalf("the run's directory is left: %v", err)
	}
}

// Not linux/amd64: refused before anything runs. Sets a package variable: not parallel.
func TestK8sPlatform(t *testing.T) {
	was := k8sPlatform
	k8sPlatform = "darwin/arm64"
	defer func() { k8sPlatform = was }()
	h := k8sHarness(t, nil)
	config := h.k8sConfig(nil)
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "needs linux/amd64") || len(h.fake.calls) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// A new secret_inputs_wo_version gives the secret inputs again, in the --given file on standard
// input: never on a command line or in state.
func TestK8sSecretVersionGivesSecrets(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	with := func(v int64) tftypes.Value {
		return h.k8sConfig(map[string]tftypes.Value{"secret_inputs_wo": strMap(map[string]string{"SMTP_PASSWORD": "new-" + secretVal}), "secret_inputs_wo_version": num(v)})
	}
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(with(1))
	h.fake.reset()
	a := h.update(state, with(2))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	up := h.fake.callsOf("k8s-upgrade")
	if len(up) != 1 || untarAll(t, up[0].stdin)["given"] != "project: \"notes\"\ninputs:\n  SMTP_PASSWORD: \"new-"+secretVal+"\"\n" {
		t.Fatalf("upgrade %v", up)
	}
	if strings.Contains(strings.Join(up[0].argv, " "), secretVal) || bytes.Contains(a.NewState.MsgPack, []byte(secretVal)) {
		t.Fatal("a secret on the command line or in state")
	}
}

// An imported release whose upgrade fails: state keeps the installer of the release that runs,
// as status --json names it, and the provider's registry.
func TestK8sImportedUpgradeFailureSavesTheRunningInstaller(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	state := h.importState("kubernetes:notes/notes").ImportedResources[0].State
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-upgrade", reply{stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`}, reply{exit: 1, stderr: "helm: timed out"})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Upgrading notes to 2.0.0") {
		t.Fatalf("got %q", got)
	}
	if at := h.attrs(a.NewState); asString(t, at["installer"]) != image || asString(t, at["registry"]) != "" {
		t.Fatalf("saved %v", at)
	}
}

// A create over the release: one helm works on is refused; one helm left failed (a first install
// that did not finish) is applied again; one whose values record no installer is taken over only
// by an installer of the release it runs.
func TestK8sCreateOverABusyFailedOrUnknownRelease(t *testing.T) {
	t.Parallel()
	helm := func(status string) string {
		return strings.Replace(statusK8s, `"status":"deployed"`, `"status":"`+status+`"`, 1)
	}
	noInstaller := strings.Replace(statusK8s, `,"installer":"`+image+`"`, ``, 1)
	for name, c := range map[string]struct {
		first  string
		inputs bool
		want   string
		ops    []string
	}{
		"helm upgrading":                 {helm("pending-upgrade"), true, "helm is working on the release: helm's latest revision (1) of the release is pending-upgrade", nil},
		"installing":                     {strings.Replace(statusK8s, `"previous":null`, `"previous":null,"installing":true`, 1), true, "helm is working on the release", nil},
		"helm failed, inputs":            {helm("failed"), true, "", []string{"tools", "fetch", "k8s-describe", "k8s-status", "k8s-upgrade", "k8s-status"}},
		"helm failed, no inputs":         {helm("failed"), false, "", []string{"tools", "fetch", "k8s-describe", "k8s-status", "k8s-upgrade", "k8s-status"}},
		"deployed, no inputs":            {statusK8s, false, "", []string{"tools", "fetch", "k8s-describe", "k8s-status", "k8s-status"}},
		"no installer, another release":  {strings.Replace(noInstaller, `"version":"1.2.0"`, `"version":"1.1.0"`, 1), true, "The release runs another release", nil},
		"no installer, the same release": {noInstaller, true, "", nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := k8sHarness(t, nil)
			h.fake.then("k8s-status", reply{stdout: c.first})
			extra := map[string]tftypes.Value{}
			if c.inputs {
				extra["inputs"] = strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})
			}
			config := h.k8sConfig(extra)
			p := h.plan(nil, config)
			got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
			if c.ops != nil && !slices.Equal(h.fake.ops(), c.ops) {
				t.Fatalf("calls %q", h.fake.ops())
			}
			if up := h.fake.callsOf("k8s-upgrade"); name == "helm failed, no inputs" && (len(up) != 1 || up[0].argv[6] != "" || flags(up[0].argv)["--set"] != "") {
				t.Fatalf("the failed release applied again with nothing given: %v", up)
			}
		})
	}
	// A create whose release is left failed once settled is an error, and saves no state.
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	h.fake.set("k8s-status", reply{stdout: helm("failed")})
	config := h.k8sConfig(nil)
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "left the release not deployed") || !h.attrsNull(a.NewState) {
		t.Fatalf("got %q", got)
	}
}

// An imported release whose values record no installer is taken over by the desired installer
// when it carries the release that runs; else the error names the release to give.
func TestK8sImportedReleaseWithoutInstaller(t *testing.T) {
	t.Parallel()
	noInstaller := strings.Replace(statusK8s, `,"installer":"`+image+`"`, ``, 1)
	for name, c := range map[string]struct {
		status, want string
	}{
		"the same release": {noInstaller, ""},
		"another release":  {strings.Replace(noInstaller, `"version":"1.2.0"`, `"version":"1.1.0"`, 1), "set installer to notes 1.1.0's"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := k8sHarness(t, nil)
			state := h.importState("kubernetes:notes/notes").ImportedResources[0].State
			h.fake.set("k8s-status", reply{stdout: c.status})
			a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
			got := errorsOf(a.Diagnostics)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// Read shows helm's status of a release that is not deployed: failed, or pending an operation.
func TestK8sReadShowsHelmStatus(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	for helm, want := range map[string]string{
		"failed":          "failed: helm's latest revision (2) of the release is failed",
		"pending-upgrade": "pending: helm's latest revision (2) of the release is pending-upgrade",
		"deployed":        "installed",
	} {
		h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"helm":{"revision":1,"status":"deployed"}`, `"helm":{"revision":2,"status":"`+helm+`"}`, 1)})
		r := h.read(state)
		if got := asString(t, h.attrs(r.NewState)["status"]); errorsOf(r.Diagnostics) != "" || got != want {
			t.Errorf("%s: status %q %s", helm, got, errorsOf(r.Diagnostics))
		}
	}
}

// A provider block not known at plan time (its cluster made again in this plan): Read keeps
// state, running nothing.
func TestK8sReadWithAnUnknownCluster(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	conn := h.ptyp.AttributeTypes["kubernetes"].(tftypes.Object)
	cfg := h.dv(h.ptyp, fill(h.ptyp, map[string]tftypes.Value{"kubernetes": fill(conn, map[string]tftypes.Value{
		"host": tftypes.NewValue(tftypes.String, tftypes.UnknownValue), "token": str(k8sToken)})}))
	if resp, err := h.srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &cfg}); err != nil || errorsOf(resp.Diagnostics) != "" {
		t.Fatalf("configure: %v %s", err, errorsOf(resp.Diagnostics))
	}
	h.fake.reset()
	r := h.read(state)
	if errorsOf(r.Diagnostics) != "" || len(h.fake.calls) != 0 || !bytes.Equal(r.NewState.MsgPack, state.MsgPack) {
		t.Fatalf("read: %s %q", errorsOf(r.Diagnostics), h.fake.ops())
	}
}

// The environment's KUBECONFIG is made absolute, each of its files: the installer runs in the
// run's directory. Sets the environment: not parallel.
func TestK8sRelativeKubeconfig(t *testing.T) {
	t.Setenv("KUBECONFIG", "kc.yaml"+string(os.PathListSeparator)+"/abs/other")
	f := newFake()
	f.set("tools", reply{})
	tmp := t.TempDir()
	r := &installResource{data: &Data{NewRunner: f.factory, TempDir: tmp}}
	defer noneLeft(t, tmp)
	var diags diag.Diagnostics
	run := r.k8sStart(context.Background(), &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}, "notes", &diags)
	if run == nil {
		t.Fatal(diags)
	}
	defer run.close()
	wd, _ := os.Getwd()
	if want := wd + "/kc.yaml" + string(os.PathListSeparator) + "/abs/other"; run.k.Kubeconfig != want {
		t.Fatalf("KUBECONFIG %q, want %q", run.k.Kubeconfig, want)
	}
}

// The state an update saves between the reconfiguration and a failed upgrade shows helm's status.
func TestK8sPartialStateShowsHelmStatus(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	// Deployed before the reconfiguration, failed after it: the state saved is the latter.
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"status":"deployed"`, `"status":"failed"`, 1)})
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-upgrade", reply{stdout: `{"format":1,"result":"done","release":{"version":"1.2.0","revision":1}}`}, reply{exit: 1, stderr: "helm: timed out"})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := asString(t, h.attrs(a.NewState)["status"]); !strings.HasPrefix(got, "failed: helm's latest revision") {
		t.Fatalf("saved status %q (%s)", got, errorsOf(a.Diagnostics))
	}
}

// An input named as a variable the run keeps from OpenTofu's environment, which sets it, is
// refused: the installer would take the environment's value. Sets the environment: not parallel.
func TestK8sRefusesAnInputTheEnvironmentGives(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.lan:3128")
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	config := h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"HTTPS_PROXY": "http://app.lan"})})
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "HTTPS_PROXY comes from the environment") || len(h.fake.callsOf("k8s-install")) != 0 {
		t.Fatalf("got %q", got)
	}
}

// A release uninstalled with its history kept reads as not installed, not failed.
func TestK8sReadUninstalledRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.set("k8s-status", reply{stdout: `{"format":1,"name":"notes","installed":null,"previous":{"version":"1.2.0","revision":1,"installer":"` + image + `"},"helm":{"revision":3,"status":"uninstalled"}}`})
	r := h.read(state)
	if got := asString(t, h.attrs(r.NewState)["status"]); errorsOf(r.Diagnostics) != "" || got != "not installed" {
		t.Fatalf("status %q %s", got, errorsOf(r.Diagnostics))
	}
}

// A create taking a release over gives given_wo first, as it is, then the configuration's inputs,
// which win as they would at an install.
func TestK8sCreateOverTheReleaseGivesGivenWO(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	given := "project: notes\ninputs:\n  ADMIN_EMAIL: from-docker@example.com\n  SITE: x\n"
	config := h.k8sConfig(map[string]tftypes.Value{"given_wo": str(given), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "ops@example.com"})})
	p := h.plan(nil, config)
	if d := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); d != "" {
		t.Fatal(d)
	}
	up := h.fake.callsOf("k8s-upgrade")
	if len(up) != 2 || untarAll(t, up[0].stdin)["given"] != given || !strings.Contains(untarAll(t, up[1].stdin)["given"], `ADMIN_EMAIL: "ops@example.com"`) {
		t.Fatalf("upgrades %v", up)
	}
}

// An update of a release helm works on is refused before anything runs.
func TestK8sUpdateRefusesABusyRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"status":"deployed"`, `"status":"pending-rollback"`, 1)})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "helm is working on the release") || len(h.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// writingRunner answers a fetch by writing a bootstrap script, as `docker run` does, and counts it.
type writingRunner struct{ fetches int }

func (w *writingRunner) Run(_ context.Context, argv []string, _ []byte) ([]byte, []byte, int, error) {
	if len(argv) > 6 && argv[2] == installer.FetchScript {
		w.fetches++
		return nil, nil, 0, os.WriteFile(filepath.Join(argv[4], argv[6]+".sh"), []byte("echo bootstrap\n"), 0o644)
	}
	return nil, nil, 0, nil
}

// An installer's bootstrap script is fetched once per digest, then read from the cache (0600 in a
// 0700 directory); another digest is fetched.
func TestK8sScriptCache(t *testing.T) {
	t.Parallel()
	w := &writingRunner{}
	tmp, cache := t.TempDir(), t.TempDir()
	r := &installResource{data: &Data{NewRunner: func(Connection) (target.Runner, error) { return w, nil }, TempDir: tmp, CacheDir: cache}}
	m := &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}
	for i, ref := range []string{image, image, image2} {
		var diags diag.Diagnostics
		run := r.k8sStart(context.Background(), m, "notes", &diags)
		if run == nil {
			t.Fatal(diags)
		}
		k, _ := run.in("run", &diags)
		if err := run.script(context.Background(), k, ref); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(filepath.Join(k.Dir, "notes.sh")); err != nil || string(b) != "echo bootstrap\n" {
			t.Fatalf("run %d: script %q %v", i, b, err)
		}
		run.close()
	}
	if w.fetches != 2 {
		t.Fatalf("%d fetches, want 2 (one per digest)", w.fetches)
	}
	kept := filepath.Join(cache, strings.TrimPrefix(digest, "sha256:"), "bootstrap.sh")
	if fi, err := os.Stat(kept); err != nil || fi.Mode() != 0o600 {
		t.Fatalf("kept %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(kept)); err != nil || fi.Mode() != 0o700|os.ModeDir {
		t.Fatalf("kept dir %v %v", fi, err)
	}
	noneLeft(t, tmp)
}

// A status read the create timeout cuts says the release is still installing.
func TestK8sCreateTimeoutDuringARead(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	h.fake.set("k8s-status", reply{block: true})
	config := h.k8sConfig(map[string]tftypes.Value{"timeouts": h.block("timeouts", map[string]tftypes.Value{"create": str("50ms")})})
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "still installing when the timeout ran out") {
		t.Fatalf("got %q", got)
	}
}

// An installer the release records that is not this project's by digest, from the configured
// installer's registry, is never fetched or run, and nothing is run over it: the update is refused
// before anything changes.
func TestK8sRecordedInstallerIsChecked(t *testing.T) {
	t.Parallel()
	for name, ref := range map[string]string{
		"a path out of the cache": "x/notes-installer@sha256:" + "../../../../../../../../../../../../../../../tmp/evil0000000000000",
		"another project":         "registry.example.com/atlas/atlas-installer@" + digest,
		"by tag":                  "registry.example.com/notes/notes-installer:1.2.0",
		"another registry":        "evil.example/notes/notes-installer@" + digest,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := k8sHarness(t, nil)
			h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
			state := h.create(h.k8sConfig(nil))
			h.fake.reset()
			h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":`+strconv.Quote(ref), 1)})
			a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
			if got := errorsOf(a.Diagnostics); !strings.Contains(got, "The release records another installer") {
				t.Fatalf("got %q", got)
			}
			if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status"}) || h.fake.callsOf("fetch")[0].argv[5] != image {
				t.Fatalf("calls %q", got)
			}
		})
	}
}

// KUBECONFIG, which a Kubernetes run sets itself, is refused as an input whatever the
// environment; an input given_wo names is checked as one inputs names. Sets the environment.
func TestK8sRunEnvInputs(t *testing.T) {
	t.Setenv("LANG", "C.UTF-8")
	os.Unsetenv("KUBECONFIG")
	for name, c := range map[string]struct {
		extra map[string]tftypes.Value
		want  string
	}{
		"KUBECONFIG":     {map[string]tftypes.Value{"inputs": strMap(map[string]string{"KUBECONFIG": "/x"})}, "KUBECONFIG is set by the run"},
		"given_wo's key": {map[string]tftypes.Value{"given_wo": str("project: notes\ninputs:\n  LANG: fr_FR.UTF-8\n")}, "LANG comes from the environment"},
	} {
		h := k8sHarness(t, nil)
		h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
		config := h.k8sConfig(c.extra)
		p := h.plan(nil, config)
		if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, c.want) || len(h.fake.callsOf("k8s-install")) != 0 {
			t.Errorf("%s: got %q", name, got)
		}
	}
}

// The final read failing after a partial save keeps the partial's values, the last saved.
func TestK8sFinalReadFailureKeepsThePartial(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	// Deployed before the reconfiguration (as state says), failed after it (the partial): the
	// final read fails, and the partial's status stays.
	failed := strings.Replace(statusK8s, `"status":"deployed"`, `"status":"failed"`, 1)
	h.fake.then("k8s-status", reply{stdout: statusK8s}, reply{stdout: failed})
	h.fake.set("k8s-status", reply{exit: 1, stderr: "the API server is gone"})
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Reading the release") {
		t.Fatalf("got %q", got)
	}
	if got := asString(t, h.attrs(a.NewState)["status"]); !strings.HasPrefix(got, "failed: helm's latest revision") {
		t.Fatalf("status %q, want the partial's failed", got)
	}
}

// A read cut by another end of the context than the timeout (an interrupt) is not reported as
// still installing.
func TestK8sSettleCanceled(t *testing.T) {
	t.Parallel()
	f := newFake()
	f.set("k8s-status", reply{block: true})
	r := &installResource{data: &Data{Poll: time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.k8sSettle(ctx, installer.K8s{Runner: f, Name: "notes", Namespace: "notes"})
	if err == nil || strings.Contains(err.Error(), "still installing") {
		t.Fatalf("err %v", err)
	}
}

// A kept script whose checksum does not match is fetched again; the cache keeps the last used
// cacheKeep entries.
func TestK8sScriptCacheChecksAndEvicts(t *testing.T) {
	t.Parallel()
	w := &writingRunner{}
	cache := t.TempDir()
	r := &installResource{data: &Data{NewRunner: func(Connection) (target.Runner, error) { return w, nil }, TempDir: t.TempDir(), CacheDir: cache}}
	m := &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}
	fetch := func(ref string) {
		var diags diag.Diagnostics
		run := r.k8sStart(context.Background(), m, "notes", &diags)
		defer run.close()
		k, _ := run.in("run", &diags)
		if err := run.script(context.Background(), k, ref); err != nil {
			t.Fatal(err)
		}
	}
	fetch(image)
	entry := filepath.Join(cache, strings.TrimPrefix(digest, "sha256:"))
	if err := os.WriteFile(filepath.Join(entry, "bootstrap.sh"), []byte("echo tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fetch(image)
	if w.fetches != 2 {
		t.Fatalf("%d fetches: a script not matching its checksum was used", w.fetches)
	}
	for i := 0; i < cacheKeep+2; i++ {
		fetch(fmt.Sprintf("registry.example.com/notes/notes-installer@sha256:%064x", i))
	}
	if entries, _ := os.ReadDir(cache); len(entries) != cacheKeep {
		t.Fatalf("%d entries kept, want %d", len(entries), cacheKeep)
	}
}

// Read never puts in state an installer the release records from another registry (the next run
// would fetch it with the provider's pull account and run it): state keeps its own, status says
// the drift, and a plan is an error until it is fixed by hand.
func TestK8sReadKeepsAnUntrustedInstallerOut(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)}))
	evil := "evil.example/notes/notes-installer@" + digest2
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"`+evil+`"`, 1)})
	r := h.read(state)
	at := h.attrs(r.NewState)
	if got := asString(t, at["installer"]); got != image || !strings.Contains(diagText(r.Diagnostics), "The release records another installer") ||
		asString(t, at["status"]) != driftPrefix+strconv.Quote(evil) {
		t.Fatalf("installer %q, status %q, diags %s", got, asString(t, at["status"]), diagText(r.Diagnostics))
	}
	// A plan changing the release is an error naming the fix; one of the provider's own settings
	// alone is not; one to an installer of the drift's registry is not either.
	change := h.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if got := errorsOf(h.plan(r.NewState, change).Diagnostics); !strings.Contains(got, "The release records another installer") || !strings.Contains(got, "k8s upgrade") {
		t.Fatalf("plan: %q", got)
	}
	if got := errorsOf(h.plan(r.NewState, h.k8sConfig(nil)).Diagnostics); got != "" {
		t.Fatalf("a plan of the provider's settings alone: %q", got)
	}
	if got := errorsOf(h.plan(r.NewState, h.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false), "installer": str("evil.example/notes/notes-installer@" + digest)})).Diagnostics); got != "" {
		t.Fatalf("a plan to the drift's registry: %q", got)
	}
	// Nor is anything run over it at a destroy.
	h.fake.reset()
	if d := errorsOf(h.destroy(r.NewState).Diagnostics); !strings.Contains(d, "Not uninstalled") || len(h.fake.calls) != 0 {
		t.Fatalf("destroy: %q, calls %q", d, h.fake.ops())
	}
	// One from the configured installer's registry replaces it, as the release that runs.
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"`+image2+`"`, 1)})
	if got := asString(t, h.attrs(h.read(state).NewState)["installer"]); got != image2 {
		t.Fatalf("installer %q", got)
	}
}

// A previous release recorded from another registry is never offered as the rollback's target.
func TestK8sUntrustedPreviousIsLeftOut(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	evil := "evil.example/notes/notes-installer@" + digest2
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s2, `"previous":{"version":"1.2.0","revision":1,"installer":"`+image+`"}`, `"previous":{"version":"1.2.0","revision":1,"installer":"`+evil+`"}`, 1)})
	r := h.read(state)
	if prev := h.attrs(r.NewState)["previous"]; strings.Contains(prev.String(), "evil.example") {
		t.Fatalf("previous %v", prev)
	}
}

func TestNormPrefix(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"norvex": "docker.io/norvex", "docker.io/norvex": "docker.io/norvex", "index.docker.io/norvex": "docker.io/norvex",
		"Mirror.Local:5000/notes": "mirror.local:5000/notes", "localhost/x": "localhost/x", "127.0.0.1:5000/notes": "127.0.0.1:5000/notes",
		"Registry/acme": "registry/acme",
	} {
		if got := normPrefix(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
}

// A create over a release running an installer from another registry is refused, naming no
// installer to configure.
func TestK8sCreateOverAnUntrustedRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"evil.example/notes/notes-installer@`+digest2+`"`, 1)})
	config := h.k8sConfig(nil)
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "The release records another installer") || strings.Contains(got, "the release is taken over") {
		t.Fatalf("got %q", got)
	}
}

// Read of another project's release under the name is an error, state untouched.
func TestK8sReadAnotherProject(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.set("k8s-status", reply{stdout: strings.Replace(strings.Replace(statusK8s, `"name":"notes"`, `"name":"wiki"`, 1), `,"installer":"`+image+`"`, ``, 1)})
	if got := errorsOf(h.read(state).Diagnostics); !strings.Contains(got, "Another project's release") {
		t.Fatalf("got %q", got)
	}
}

// A create after which the release records an installer from another registry saves its state
// (nothing left untracked) with a warning, not an error (which would taint it: its replacement
// uninstalls it), the drift in status: the next plan changing the release is an error naming the fix.
func TestK8sCreateLeavesAnUntrustedRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"evil.example/notes/notes-installer@`+digest2+`"`, 1)})
	config := h.k8sConfig(nil)
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" || !strings.Contains(diagText(a.Diagnostics), "The release records another installer") ||
		!strings.HasPrefix(asString(t, h.attrs(a.NewState)["status"]), driftPrefix) {
		t.Fatalf("got %s", diagText(a.Diagnostics))
	}
	change := h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if got := errorsOf(h.plan(a.NewState, change).Diagnostics); !strings.Contains(got, "The release records another installer") {
		t.Fatalf("plan: %q", got)
	}
}

// Drift is a Kubernetes target's alone: a Docker install whose failure reads like it is not.
func TestDriftIsKubernetesOnly(t *testing.T) {
	t.Parallel()
	docker := &installModel{Docker: &dockerModel{}, Status: types.StringValue(driftPrefix + "x")}
	k8s := &installModel{Kubernetes: &k8sModel{}, Status: types.StringValue(driftPrefix + "x")}
	if drifted(docker) || !drifted(k8s) {
		t.Fatalf("docker %v, kubernetes %v", drifted(docker), drifted(k8s))
	}
}

// The partial save after a reconfiguration that leaves the release recording an installer not
// trusted marks the drift, and the upgrade is not run over it.
func TestK8sPartialStopsOverAnUntrustedRelease(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	evil := "evil.example/notes/notes-installer@" + digest2
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"`+evil+`"`, 1)})
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "The release records another installer") || len(h.fake.callsOf("k8s-upgrade")) != 1 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	if got := asString(t, h.attrs(a.NewState)["status"]); got != driftPrefix+strconv.Quote(evil) {
		t.Fatalf("status %q", got)
	}
}

// An install_failed text never reads as the drift.
func TestK8sInstallFailedIsNotDrift(t *testing.T) {
	t.Parallel()
	m := &installModel{Kubernetes: &k8sModel{}}
	applyStatus(m, &installer.Status{InstallFailed: strings.TrimPrefix(driftPrefix, "failed: ") + "x"}, false)
	k8sStatus(m, &installer.Status{InstallFailed: strings.TrimPrefix(driftPrefix, "failed: ") + "x"})
	if drifted(m) {
		t.Fatalf("status %q reads as the drift", m.Status.ValueString())
	}
}

// A drifted state's plan is not refused while the provider's cluster is not known (it is being
// made again: the release will not be there).
func TestK8sDriftWithAnUnknownCluster(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"evil.example/notes/notes-installer@`+digest2+`"`, 1)})
	drifted := h.read(state).NewState
	conn := h.ptyp.AttributeTypes["kubernetes"].(tftypes.Object)
	cfg := h.dv(h.ptyp, fill(h.ptyp, map[string]tftypes.Value{"kubernetes": fill(conn, map[string]tftypes.Value{
		"host": tftypes.NewValue(tftypes.String, tftypes.UnknownValue), "token": str(k8sToken)})}))
	if resp, err := h.srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &cfg}); err != nil || errorsOf(resp.Diagnostics) != "" {
		t.Fatalf("configure: %v", err)
	}
	change := h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})})
	if got := errorsOf(h.plan(drifted, change).Diagnostics); got != "" {
		t.Fatalf("plan: %q", got)
	}
}

// The drift keeps helm's status beside it, and its installer reads back whole.
func TestK8sDriftKeepsTheStatus(t *testing.T) {
	t.Parallel()
	m := &installModel{Kubernetes: &k8sModel{}, Status: types.StringValue("pending: helm's latest revision (2) of the release is pending-upgrade")}
	markDrift(m, &untrustedRun{ref: "evil.example/notes/notes-installer@" + digest})
	if ref, ok := driftRef(m); !ok || ref != "evil.example/notes/notes-installer@"+digest || !strings.Contains(m.Status.ValueString(), "pending-upgrade") {
		t.Fatalf("status %q, ref %q", m.Status.ValueString(), ref)
	}
}

// A destroy of a drifted state whose installer the provider's registry now trusts uninstalls; a
// plan whose installer is not known yet is not refused on the drift.
func TestK8sDriftCheckedAgain(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": str("evil.example/notes")})
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)}))
	_ = state
	// The state as a refresh before the registry was set would have left it.
	h2 := k8sHarness(t, nil)
	h2.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	st2 := h2.create(h2.k8sConfig(map[string]tftypes.Value{"deletion_protection": boolean(false)}))
	h2.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"evil.example/notes/notes-installer@`+digest2+`"`, 1)})
	drifted := h2.read(st2).NewState
	if d := errorsOf(h.destroy(drifted).Diagnostics); d != "" || len(h.fake.callsOf("k8s-uninstall")) != 1 {
		t.Fatalf("destroy: %q, calls %q", d, h.fake.ops())
	}
	unknownInstaller := h2.k8sConfig(map[string]tftypes.Value{"installer": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	if got := errorsOf(h2.plan(drifted, unknownInstaller).Diagnostics); strings.Contains(got, "The release records another installer") {
		t.Fatalf("plan: %q", got)
	}
}

// A failed read after the reconfiguration stops the run before the upgrade.
func TestK8sPartialReadFailureStops(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{exit: 1, stderr: "the API server is gone"})
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the upgrade is not run") || len(h.fake.callsOf("k8s-upgrade")) != 1 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	if inputsOf(t, h, a.NewState)["ADMIN_EMAIL"] != "b@example.com" || asString(t, h.attrs(a.NewState)["installer"]) != image {
		t.Fatalf("state %v", h.attrs(a.NewState))
	}
}

// After a create too, an install_failed text never reads as the drift.
func TestK8sCreateInstallFailedIsNotDrift(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"previous":null`, `"previous":null,"install_failed":`+strconv.Quote(strings.TrimPrefix(driftPrefix, "failed: ")+"x"), 1)})
	state := h.create(h.k8sConfig(nil))
	if got := asString(t, h.attrs(state)["status"]); strings.HasPrefix(got, driftPrefix) {
		t.Fatalf("status %q reads as the drift", got)
	}
}

// A drifted status reads its installer back whole, whatever it holds.
func TestK8sDriftRefQuoted(t *testing.T) {
	t.Parallel()
	for _, ref := range []string{"reg.trusted/p-installer@sha256:" + strings.Repeat("a", 64) + " (and: x", `a"b`, ""} {
		m := &installModel{Kubernetes: &k8sModel{}, Status: types.StringValue("failed: helm's latest revision (1) of the release is failed")}
		markDrift(m, &untrustedRun{ref: ref})
		if got, ok := driftRef(m); !ok || got != ref {
			t.Errorf("%q: read back %q %v (status %q)", ref, got, ok, m.Status.ValueString())
		}
	}
}

// The upgrade is not run while helm works on the release after its reconfiguration.
func TestK8sPartialBusyStops(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}))
	h.fake.reset()
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"status":"deployed"`, `"status":"pending-upgrade"`, 1)})
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "b@example.com"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "the upgrade is not run") || len(h.fake.callsOf("k8s-upgrade")) != 1 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// A failed final read leaves no stale drift: the next plan is not refused for it.
func TestK8sFinalReadFailureClearsTheDrift(t *testing.T) {
	t.Parallel()
	m := &installModel{Kubernetes: &k8sModel{}}
	base := &installModel{Kubernetes: &k8sModel{}, Status: types.StringValue(driftPrefix + strconv.Quote("x")), Previous: types.ObjectNull(previousType)}
	f := newFake()
	f.set("k8s-status", reply{exit: 1, stderr: "gone"})
	r := &installResource{data: &Data{}}
	var diags diag.Diagnostics
	r.k8sFinal(context.Background(), installer.K8s{Runner: f, Name: "notes", Namespace: "notes"}, m, base, "notes", &diags)
	if drifted(m) || !diags.HasError() {
		t.Fatalf("status %q, diags %v", m.Status.ValueString(), diags)
	}
}

// With the planned installer unknown, a drift to another project's installer is refused at plan.
func TestK8sDriftOfAnotherProjectWithAnUnknownInstaller(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.set("k8s-status", reply{stdout: strings.Replace(statusK8s, `"installer":"`+image+`"`, `"installer":"registry.example.com/atlas/atlas-installer@`+digest2+`"`, 1)})
	drifted := h.read(state).NewState
	config := h.k8sConfig(map[string]tftypes.Value{"installer": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	if got := errorsOf(h.plan(drifted, config).Diagnostics); !strings.Contains(got, "not this project's installer by digest") {
		t.Fatalf("plan: %q", got)
	}
}

// image_pull_login runs the install and the upgrade with --image-pull-login, the pull account's
// login on standard input for the mirror's host (never an argument), and is refused at plan
// without the account or a mirror.
func TestK8sImagePullLogin(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan/notes"), "username": str("puller"), "password": str("pw-" + secretVal)})
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	login := h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes"), "image_pull_login": boolean(true)})
	state := h.create(h.k8sConfig(map[string]tftypes.Value{"kubernetes": login}))
	install := h.fake.callsOf("k8s-install")[0]
	if !contains(install.argv, "--image-pull-login") || install.argv[7] != "mirror.lan" || install.argv[8] != "puller" ||
		untarAll(t, install.stdin)["login-password"] != "pw-"+secretVal || strings.Contains(strings.Join(install.argv, " "), secretVal) {
		t.Fatalf("install %q", install.argv)
	}
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	if d := errorsOf(h.update(state, h.k8sConfig(map[string]tftypes.Value{"kubernetes": login, "installer": str(image2)})).Diagnostics); d != "" {
		t.Fatal(d)
	}
	ups := h.fake.callsOf("k8s-upgrade")
	if len(ups) == 0 {
		t.Fatalf("no upgrade: %q", h.fake.ops())
	}
	for _, up := range ups {
		if !contains(up.argv, "--image-pull-login") || up.argv[7] != "mirror.lan" {
			t.Fatalf("upgrade %q", up.argv)
		}
	}
	// Without it, no login and no flag.
	h.fake.reset()
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	plain := h.create(h.k8sConfig(nil))
	if install := h.fake.callsOf("k8s-install")[0]; contains(install.argv, "--image-pull-login") || install.argv[8] != "" {
		t.Fatalf("install without image_pull_login %q", install.argv)
	}
	// Turned on alone, the release is applied again with it, which writes the Secret.
	h.fake.reset()
	if d := errorsOf(h.update(plain, h.k8sConfig(map[string]tftypes.Value{"kubernetes": login})).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || !contains(ups[0].argv, "--image-pull-login") {
		t.Fatalf("turned on: %q", h.fake.ops())
	}
	for name, cfg := range map[string]map[string]tftypes.Value{
		"no account": {"registry": str("mirror.lan/notes")},
		"no mirror":  {"username": str("puller"), "password": str("pw")},
	} {
		o := k8sHarness(t, cfg)
		if got := errorsOf(o.plan(nil, o.k8sConfig(map[string]tftypes.Value{"kubernetes": login})).Diagnostics); !strings.Contains(got, "image_pull_login needs") {
			t.Errorf("%s: got %q", name, got)
		}
	}
}
