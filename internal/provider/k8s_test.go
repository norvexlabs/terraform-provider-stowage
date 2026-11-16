package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

	"github.com/norvexlabs/terraform-provider-stowage/internal/fetch"
	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
	"github.com/norvexlabs/terraform-provider-stowage/internal/testutil"
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
		"no namespace":       {block(nil), "namespace is required"},
		"a namespace's form": {block(map[string]tftypes.Value{"namespace": str("My_ns")}), "namespace is not a DNS label"},
		"a long release":     {block(map[string]tftypes.Value{"namespace": str("n"), "release": str(strings.Repeat("r", 54))}), "release is not a DNS label"},
		"values not YAML":    {block(map[string]tftypes.Value{"namespace": str("n"), "values": str("a: [")}), "values is not a chart values file"},
		"values a list":      {block(map[string]tftypes.Value{"namespace": str("n"), "values": str("- a\n")}), "its document is not a mapping"},
		"values empty":       {block(map[string]tftypes.Value{"namespace": str("n"), "values": str("")}), "its document is not a mapping"},
		"values two docs":    {block(map[string]tftypes.Value{"namespace": str("n"), "values": str("a: 1\n---\n- x\n")}), "more than one YAML document"},
	} {
		if got := diagText(h.validate(c.config)); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", name, got, c.want)
		}
	}
	// Mappings helm takes: a key that is no string, an empty document after the mapping.
	for _, v := range []string{"1: a\nb: {c: null}\n", "a: 1\n---\n"} {
		if d := errorsOf(h.validate(block(map[string]tftypes.Value{"namespace": str("n"), "values": str(v)}))); d != "" {
			t.Errorf("values %q: %s", v, d)
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
	// Every call reads the kubeconfig written for the run, in TempDir, never in the cache, removed
	// once it ended; the run's working directory beside it.
	kc := install.argv[5]
	if !strings.HasSuffix(kc, "/kubeconfig") || !strings.HasPrefix(kc, h.tmp+"/stowage-k8s-") || strings.HasPrefix(kc, h.cache+"/") ||
		!strings.HasPrefix(install.argv[4], filepath.Dir(kc)+"/") {
		t.Fatalf("kubeconfig %q, the run in %q", kc, install.argv[4])
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
	// Nothing to give but values: the release is applied again with them.
	h.fake.reset()
	config = h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes"), "values": str("replicas: 2\n")})})
	p = h.plan(nil, config)
	if d := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || untarAll(t, ups[0].stdin)["values"] != "replicas: 2\n" || untarAll(t, ups[0].stdin)["given"] != "" {
		t.Fatalf("take-over with values: %q", h.fake.ops())
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
			"namespace": str("other")})}), "namespace is first-install only"},
		"another release": {h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{
			"namespace": str("notes"), "release": str("notes2")})}), "the helm release (release, else the project's name) is first-install only"},
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
		if got := errorsOf(h.importState(id).Diagnostics); !strings.Contains(got, "want <namespace>/<release>") {
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

// An installer image without an entry for this machine's platform: refused before anything runs
// on the cluster, naming the platforms it has.
func TestK8sNoEntryForThisPlatform(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.set("fetch", reply{exit: 1, stderr: image + " has no image for plan9/amd64 (it has linux/amd64, darwin/arm64)"})
	config := h.k8sConfig(nil)
	p := h.plan(nil, config)
	if got := errorsOf(h.apply(nil, p.PlannedState, config).Diagnostics); !strings.Contains(got, "it has linux/amd64, darwin/arm64") || slices.ContainsFunc(h.fake.ops(), func(o string) bool { return strings.HasPrefix(o, "k8s-") }) {
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

// quietRunner answers every run with exit 0 and nothing: a Kubernetes run's tools all found.
type quietRunner struct{}

func (quietRunner) Run(context.Context, []string, []byte) ([]byte, []byte, int, error) {
	return nil, nil, 0, nil
}

// countingGet is a registry read counting its calls, each answered with an installer of short
// name notes.
type countingGet struct{ n int }

func (c *countingGet) get(_ context.Context, ref string, o fetch.Options) (*fetch.Installer, error) {
	c.n++
	if o.Platform != Platform {
		return nil, fmt.Errorf("asked %s", o.Platform)
	}
	f, err := os.CreateTemp(o.Dir, ".installer-*.part")
	if err != nil {
		return nil, err
	}
	_, err = f.Write(withPayload("exe of " + ref))
	if err = errors.Join(err, f.Chmod(0o700), f.Close()); err != nil {
		return nil, err
	}
	return &fetch.Installer{Short: "notes", Path: f.Name(), Image: ref}, nil
}

// withPayload is exe with a release payload of files appended, as an installer carries one.
func withPayload(exe string, files ...[2]string) []byte {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for _, f := range files {
		_ = tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o644, Size: int64(len(f[1])), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	out := append([]byte(exe), tb.Bytes()...)
	out = binary.BigEndian.AppendUint64(out, uint64(tb.Len()))
	return append(out, "STOWAGE-PAYLOAD1"...)
}

// An installer is read once per digest, then from the cache (0700 in 0700 directories, by the
// index's digest and this machine's platform); another digest is read; the run's script, in the
// run's directory in TempDir, runs its own link to the kept program, in the cache's runs/.
func TestK8sScriptCache(t *testing.T) {
	t.Parallel()
	g := &countingGet{}
	tmp, cache := t.TempDir(), t.TempDir()
	r := &installResource{data: &Data{NewRunner: func(Connection) (target.Runner, error) { return quietRunner{}, nil }, TempDir: tmp, CacheDir: cache,
		Installers: nativeSource{get: g.get}}}
	m := &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}
	kept := filepath.Join(cache, strings.TrimPrefix(digest, "sha256:"), strings.ReplaceAll(Platform, "/", "-"), "notes-stow")
	for i, ref := range []string{image, image, image2} {
		var diags diag.Diagnostics
		run := r.k8sStart(context.Background(), m, "notes", &diags)
		if run == nil {
			t.Fatal(diags)
		}
		if filepath.Dir(run.progs) != filepath.Join(cache, "runs") || filepath.Dir(run.dir) != tmp {
			t.Fatalf("run %d: the run's directories %s, %s", i, run.progs, run.dir)
		}
		if b, ok := run.k.Runner.(busyRunner); !ok || b.dir != run.progs {
			t.Fatalf("run %d: the runs are not made again on a busy program: %#v", i, run.k.Runner)
		}
		k, _ := run.in("run", &diags)
		if err := run.script(context.Background(), k, ref); err != nil {
			t.Fatal(err)
		}
		own := filepath.Join(run.progs, "run", "notes-stow")
		if b, err := os.ReadFile(filepath.Join(k.Dir, "notes.sh")); err != nil || string(b) != "#!/bin/sh\nexec '"+own+"' \"$@\"\n" {
			t.Fatalf("run %d: script %q %v", i, b, err)
		}
		ofRef := filepath.Join(cache, strings.TrimPrefix(map[string]string{image: digest, image2: digest2}[ref], "sha256:"), strings.ReplaceAll(Platform, "/", "-"), "notes-stow")
		if !sameFile(t, own, ofRef) {
			t.Fatalf("run %d: the run's program is not a link to the kept one", i)
		}
		run.close()
	}
	if g.n != 2 {
		t.Fatalf("%d reads, want 2 (one per digest)", g.n)
	}
	if fi, err := os.Stat(kept); err != nil || fi.Mode() != 0o700 {
		t.Fatalf("kept %v %v", fi, err)
	}
	if b, _ := os.ReadFile(kept); string(b) != string(withPayload("exe of "+image)) {
		t.Fatalf("kept %q", b)
	}
	if fi, err := os.Stat(filepath.Dir(kept)); err != nil || fi.Mode() != 0o700|os.ModeDir {
		t.Fatalf("kept dir %v %v", fi, err)
	}
	noneLeft(t, tmp)
	noneLeft(t, filepath.Join(cache, "runs"))
}

// sameFile says whether a and b are the same file (a hard link of one another).
func sameFile(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	fb, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(fa, fb)
}

// The program a run executes is its own, in its directory: a prune of the cache removing the
// entry it came from (another run's, meanwhile) leaves it whole.
func TestProgramOutlivesAPrune(t *testing.T) {
	t.Parallel()
	g := &countingGet{}
	src := nativeSource{get: g.get}
	d := &Data{CacheDir: t.TempDir()}
	for _, fresh := range []bool{true, false} {
		dir := t.TempDir()
		p, err := src.Program(context.Background(), d, Want{Ref: image}, dir)
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(p.Path) != dir || p.Kept == "" || !sameFile(t, p.Path, p.Kept) {
			t.Fatalf("fresh %v: %+v", fresh, p)
		}
		if fi, err := os.Stat(p.Path); err != nil || fi.Mode() != 0o700 {
			t.Fatalf("fresh %v: %v %v", fresh, fi, err)
		}
		if fresh {
			continue // the second takes the cache's
		}
		if err := os.RemoveAll(filepath.Join(d.CacheDir, strings.TrimPrefix(digest, "sha256:"))); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(p.Path); err != nil || string(b) != string(withPayload("exe of "+image)) {
			t.Fatalf("after a prune: %q %v", b, err)
		}
	}
	if g.n != 1 {
		t.Fatalf("%d reads", g.n)
	}
}

// A run's program is a hard link to the kept one, else a copy of it (0700, renamed into place:
// nothing else left in the run's directory), on a cache hit as on a fetch, and a cache hit is
// not fetched again.
func TestProgramLinkFails(t *testing.T) {
	t.Parallel()
	g := &countingGet{}
	fail := false
	src := nativeSource{get: g.get, link: func(o, n string) error {
		if fail {
			return errors.New("invalid cross-device link")
		}
		return os.Link(o, n)
	}}
	d := &Data{CacheDir: t.TempDir()}
	if _, err := src.Program(context.Background(), d, Want{Ref: image}, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	fail = true
	for _, ref := range []string{image, image2} { // a cache hit, then a fetch
		dir := t.TempDir()
		p, err := src.Program(context.Background(), d, Want{Ref: ref}, dir)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if p.Path != filepath.Join(dir, "notes-stow") || p.Kept == "" || sameFile(t, p.Path, p.Kept) {
			t.Fatalf("%s: not a copy: %+v", ref, p)
		}
		if fi, err := os.Stat(p.Path); err != nil || fi.Mode() != 0o700 {
			t.Fatalf("%s: %v %v", ref, fi, err)
		}
		if b, err := os.ReadFile(p.Path); err != nil || string(b) != string(withPayload("exe of "+ref)) {
			t.Fatalf("%s: the copy %q %v", ref, b, err)
		}
		if left, _ := os.ReadDir(dir); len(left) != 1 {
			t.Fatalf("%s: left %v", ref, left)
		}
	}
	if g.n != 2 {
		t.Fatalf("%d reads, want 2: the cache hit is not fetched again", g.n)
	}
}

// A run's directory is in the cache's runs/ when there is a cache (so its program is a link to
// the kept one), else in TempDir; making one removes the runs' directories a day old or more.
func TestRunDir(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	if dir, err := (&Data{TempDir: tmp}).runDir("stowage-k8s-"); err != nil || filepath.Dir(dir) != tmp {
		t.Fatalf("no cache: %q %v", dir, err)
	}
	d := &Data{TempDir: t.TempDir(), CacheDir: t.TempDir()}
	runs := filepath.Join(d.CacheDir, "runs")
	old, young := filepath.Join(runs, "stowage-k8s-old"), filepath.Join(runs, "stowage-k8s-young")
	for _, p := range []string{old, young} {
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	at := time.Now().Add(-runsStale)
	if err := os.Chtimes(old, at, at); err != nil {
		t.Fatal(err)
	}
	at = at.Add(time.Minute)
	if err := os.Chtimes(young, at, at); err != nil {
		t.Fatal(err)
	}
	dir, err := d.runDir("stowage-seed-")
	if err != nil || filepath.Dir(dir) != runs || !strings.HasPrefix(filepath.Base(dir), "stowage-seed-") {
		t.Fatalf("cache: %q %v", dir, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode() != 0o700|os.ModeDir {
		t.Fatalf("mode %v %v", fi, err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("a run's directory a day old is kept: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("a younger one is removed: %v", err)
	}
	noneLeft(t, d.TempDir)
	// A cache that cannot be written: TempDir.
	ro := &Data{TempDir: t.TempDir(), CacheDir: filepath.Join(t.TempDir(), "file")}
	if err := os.WriteFile(ro.CacheDir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, err := ro.runDir("stowage-k8s-"); err != nil || filepath.Dir(dir) != ro.TempDir {
		t.Fatalf("a cache that cannot be written: %q %v", dir, err)
	}
}

// Without a cache, the program is written in the run's directory, removed with it.
func TestK8sNoCache(t *testing.T) {
	t.Parallel()
	g := &countingGet{}
	tmp := t.TempDir()
	r := &installResource{data: &Data{NewRunner: func(Connection) (target.Runner, error) { return quietRunner{}, nil }, TempDir: tmp, Installers: nativeSource{get: g.get}}}
	var diags diag.Diagnostics
	run := r.k8sStart(context.Background(), &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}, "notes", &diags)
	k, _ := run.in("run", &diags)
	if err := run.script(context.Background(), k, image); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(run.progs, "run", "notes-stow")); err != nil || fi.Mode() != 0o700 {
		t.Fatalf("written %v %v", fi, err)
	}
	run.close()
	noneLeft(t, tmp)
}

// A status read the create timeout cuts says the release is still installing.
func TestK8sCreateTimeoutDuringARead(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.lenient = true // every call before the wait answers however late: the timeout is the wait's
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

// A kept program whose checksum does not match is read again; the cache keeps the last used
// cacheKeep entries.
func TestK8sScriptCacheChecksAndEvicts(t *testing.T) {
	t.Parallel()
	g := &countingGet{}
	cache := t.TempDir()
	r := &installResource{data: &Data{NewRunner: func(Connection) (target.Runner, error) { return quietRunner{}, nil }, TempDir: t.TempDir(), CacheDir: cache,
		Installers: nativeSource{get: g.get}}}
	m := &installModel{Kubernetes: &k8sModel{Namespace: types.StringValue("notes")}}
	fetchOne := func(ref string) {
		var diags diag.Diagnostics
		run := r.k8sStart(context.Background(), m, "notes", &diags)
		defer run.close()
		k, _ := run.in("run", &diags)
		if err := run.script(context.Background(), k, ref); err != nil {
			t.Fatal(err)
		}
	}
	fetchOne(image)
	kept := filepath.Join(cache, strings.TrimPrefix(digest, "sha256:"), strings.ReplaceAll(Platform, "/", "-"), "notes-stow")
	if err := os.WriteFile(kept, []byte("tampered"), 0o700); err != nil {
		t.Fatal(err)
	}
	fetchOne(image)
	if b, _ := os.ReadFile(kept); g.n != 2 || string(b) != string(withPayload("exe of "+image)) {
		t.Fatalf("%d reads, kept %q: a program not matching its checksum was used", g.n, b)
	}
	for i := 0; i < cacheKeep+2; i++ {
		fetchOne(fmt.Sprintf("registry.example.com/notes/notes-installer@sha256:%064x", i))
	}
	entries, _ := os.ReadDir(cache)
	entries = slices.DeleteFunc(entries, func(e os.DirEntry) bool { return e.Name() == "runs" })
	if len(entries) != cacheKeep {
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
	// Turned on with a rollback: refused before anything runs (helm's rollback writes no Secret).
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	upgraded := h.update(plain, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2)}))
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	h.fake.set("k8s-describe", reply{stdout: describeK8s2})
	back := h.k8sConfig(map[string]tftypes.Value{"kubernetes": login, "allow_rollback": boolean(true)})
	p := h.plan(upgraded.NewState, back)
	got := errorsOf(p.Diagnostics) + errorsOf(h.apply(upgraded.NewState, p.PlannedState, back).Diagnostics)
	if !strings.Contains(got, "a rollback goes alone") || len(h.fake.callsOf("k8s-rollback")) != 0 {
		t.Fatalf("a rollback with image_pull_login turned on: %q %q", got, h.fake.ops())
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

// Every Kubernetes run carries the mirror's login; image_pull_login writes the Secret from it.
func TestK8sMirrorLogin(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, map[string]tftypes.Value{"registry": str("mirror.lan:5000/site"), "registry_username": str("site"), "registry_password": str("site-" + secretVal)})
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	login := h.block("kubernetes", map[string]tftypes.Value{"namespace": str("notes"), "image_pull_login": boolean(true)})
	h.create(h.k8sConfig(map[string]tftypes.Value{"kubernetes": login}))
	install := h.fake.callsOf("k8s-install")[0]
	if !contains(install.argv, "--image-pull-login") || install.argv[7] != "mirror.lan:5000" || install.argv[8] != "site" || untarAll(t, install.stdin)["login-password"] != "site-"+secretVal {
		t.Fatalf("install %q", install.argv)
	}
	st := h.fake.callsOf("k8s-status")[0]
	if untarAll(t, st.stdin)["mirror-password"] != "site-"+secretVal {
		t.Fatalf("status run without the mirror's login: %q", st.argv)
	}
}

// An imported release's first apply compares the configuration with the inputs it records: matching,
// no helm upgrade runs; a differing input re-runs it.
func TestK8sImportedTakeOverComparesTheRecordedValues(t *testing.T) {
	t.Parallel()
	for email, upgrades := range map[string]int{"a@example.com": 0, "b@example.com": 1} {
		h := k8sHarness(t, nil)
		h.fake.set("k8s-status", reply{stdout: strings.TrimSuffix(strings.TrimSpace(statusK8s), "}") + `,"values":{"inputs":{"ADMIN_EMAIL":"a@example.com"}}}`})
		state := h.importState("kubernetes:prod/notes").ImportedResources[0].State
		a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("prod")}),
			"inputs": strMap(map[string]string{"ADMIN_EMAIL": email})}))
		if d := errorsOf(a.Diagnostics); d != "" {
			t.Fatal(d)
		}
		st := h.fake.callsOf("k8s-status")
		if len(st) == 0 || !contains(st[0].argv, "--values") || len(h.fake.callsOf("k8s-upgrade")) != upgrades {
			t.Errorf("%s: calls %q", email, h.fake.ops())
		}
	}
}

// values goes to helm as a values file (`--values`, on standard input) at the install,
// a change of it alone applies the release again with it, an upgrade carries it, a rollback beside
// a change of it is refused, and leaving it out runs nothing, with a warning.
func TestK8sChartValues(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	cfg := func(values string, extra map[string]tftypes.Value) tftypes.Value {
		b := map[string]tftypes.Value{"namespace": str("notes")}
		if values != "" {
			b["values"] = str(values)
		}
		c := map[string]tftypes.Value{"kubernetes": h.block("kubernetes", b)}
		maps.Copy(c, extra)
		return h.k8sConfig(c)
	}
	carries := func(c call, want string) bool {
		return strings.Contains(c.argv[6], "values") && untarAll(t, c.stdin)["values"] == want
	}
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(cfg("replicas: 2\n", nil))
	if install := h.fake.callsOf("k8s-install"); len(install) != 1 || !carries(install[0], "replicas: 2\n") {
		t.Fatalf("install %q", h.fake.ops())
	}
	// Changed alone: the release applied again, given the new values.
	h.fake.reset()
	a := h.update(state, cfg("replicas: 3\n", nil))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || !carries(ups[0], "replicas: 3\n") {
		t.Fatalf("changed: %q", h.fake.ops())
	}
	state = a.NewState
	// The same values: nothing runs.
	h.fake.reset()
	if d := errorsOf(h.update(state, cfg("replicas: 3\n", nil)).Diagnostics); d != "" || len(h.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("unchanged: %s %q", d, h.fake.ops())
	}
	// An upgrade carries them.
	h.fake.reset()
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	upgraded := h.update(state, cfg("replicas: 3\n", map[string]tftypes.Value{"installer": str(image2)}))
	if d := errorsOf(upgraded.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || !carries(ups[0], "replicas: 3\n") {
		t.Fatalf("upgrade: %q", h.fake.ops())
	}
	// A rollback beside a change of them: refused before anything runs.
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	h.fake.set("k8s-describe", reply{stdout: describeK8s2})
	back := cfg("replicas: 4\n", map[string]tftypes.Value{"allow_rollback": boolean(true)})
	p := h.plan(upgraded.NewState, back)
	got := errorsOf(p.Diagnostics) + errorsOf(h.apply(upgraded.NewState, p.PlannedState, back).Diagnostics)
	if !strings.Contains(got, "a rollback goes alone") || !strings.Contains(got, "values") || len(h.fake.callsOf("k8s-rollback")) != 0 {
		t.Fatalf("a rollback with values changed: %q %q", got, h.fake.ops())
	}
	// State's previous stale (the plan sees no rollback): the apply, reading the release's, refuses.
	attrs := h.attrs(upgraded.NewState)
	attrs["previous"] = tftypes.NewValue(attrs["previous"].Type(), nil)
	stale := h.dv(h.typ, tftypes.NewValue(h.typ, attrs))
	p = h.plan(&stale, back)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatalf("plan over a stale previous: %s", d)
	}
	if got := errorsOf(h.apply(&stale, p.PlannedState, back).Diagnostics); !strings.Contains(got, "a rollback goes alone") || len(h.fake.callsOf("k8s-rollback")) != 0 {
		t.Fatalf("apply over a stale previous: %q %q", got, h.fake.ops())
	}
	// A rollback with the values unchanged: helm's takes back the earlier release's values, so the
	// release is applied again with them after it.
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	h.fake.set("k8s-describe", reply{stdout: describeK8s2})
	if d := errorsOf(h.update(upgraded.NewState, cfg("replicas: 3\n", map[string]tftypes.Value{"allow_rollback": boolean(true)})).Diagnostics); d != "" {
		t.Fatal(d)
	}
	// The re-apply is the reached release's installer's (fetched into its own directory), never
	// the one rolled back from, whose upgrade would undo the rollback.
	if ops, ups := h.fake.ops(), h.fake.callsOf("k8s-upgrade"); len(h.fake.callsOf("k8s-rollback")) != 1 || len(ups) != 1 || untarAll(t, ups[0].stdin)["values"] != "replicas: 3\n" ||
		slices.Index(ops, "k8s-rollback") > slices.Index(ops, "k8s-upgrade") || filepath.Base(ups[0].argv[4]) != "back" ||
		filepath.Base(h.fake.callsOf("k8s-rollback")[0].argv[4]) != "run" {
		t.Fatalf("rollback with values: %q %q", ops, h.fake.callsOf("k8s-upgrade"))
	}
	if !slices.ContainsFunc(h.fake.callsOf("fetch"), func(c call) bool { return filepath.Base(c.argv[4]) == "back" && c.argv[5] == image }) {
		t.Fatalf("the reached release's installer was not fetched into back: %q", h.fake.callsOf("fetch"))
	}
	// Its re-apply failing: rolled back, state records no values, so the next plan gives them.
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	h.fake.set("k8s-describe", reply{stdout: describeK8s2})
	h.fake.then("k8s-upgrade", reply{exit: 1, stderr: "Error: helm failed\n"})
	rb := cfg("replicas: 3\n", map[string]tftypes.Value{"allow_rollback": boolean(true)})
	p = h.plan(upgraded.NewState, rb)
	a = h.apply(upgraded.NewState, p.PlannedState, rb)
	if d := errorsOf(a.Diagnostics); !strings.Contains(d, "after its rollback") {
		t.Fatalf("re-apply failing: %q", d)
	}
	var kb map[string]tftypes.Value
	if err := h.attrs(a.NewState)["kubernetes"].As(&kb); err != nil || !kb["values"].IsNull() || asString(t, h.attrs(a.NewState)["installer"]) != image {
		t.Fatalf("state after the failed re-apply: %v %v", kb, err)
	}
	// The next plan gives them again.
	h.fake.reset()
	h.fake.set("k8s-status", reply{stdout: statusK8s})
	if d := errorsOf(h.update(a.NewState, cfg("replicas: 3\n", nil)).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || untarAll(t, ups[0].stdin)["values"] != "replicas: 3\n" {
		t.Fatalf("the values not given again: %q", h.fake.ops())
	}
	// Left out: nothing runs, and the plan says the release keeps them.
	h.fake.reset()
	p = h.plan(state, cfg("", nil))
	if w := warningsOf(p.Diagnostics); !strings.Contains(w, "values left out: the release keeps its values") {
		t.Fatalf("left out: %q", w)
	}
	if d := errorsOf(h.apply(state, p.PlannedState, cfg("", nil)).Diagnostics); d != "" || len(h.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("left out: %s %q", d, h.fake.ops())
	}
}

// An imported release taken over with values: its recorded inputs matching, the release
// is still applied again with the values (they are not read), which the plan's report says.
func TestK8sImportedTakeOverGivesTheChartValues(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	h.fake.set("k8s-status", reply{stdout: strings.TrimSuffix(strings.TrimSpace(statusK8s), "}") + `,"values":{"inputs":{"ADMIN_EMAIL":"a@example.com"}}}`})
	state := h.importState("kubernetes:prod/notes").ImportedResources[0].State
	cfg := func(extra map[string]tftypes.Value) tftypes.Value {
		c := map[string]tftypes.Value{"kubernetes": h.block("kubernetes", map[string]tftypes.Value{"namespace": str("prod"), "values": str("replicas: 2\n")}),
			"inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@example.com"})}
		maps.Copy(c, extra)
		return h.k8sConfig(c)
	}
	p := h.plan(state, cfg(map[string]tftypes.Value{"plan_dry_run": boolean(true)}))
	if w := warningsOf(p.Diagnostics); !strings.Contains(w, "its values match, applied again for values") || !strings.Contains(w, "It gives values again") {
		t.Fatalf("plan: %s %s", errorsOf(p.Diagnostics), w)
	}
	h.fake.reset()
	if d := errorsOf(h.update(state, cfg(nil)).Diagnostics); d != "" {
		t.Fatal(d)
	}
	if ups := h.fake.callsOf("k8s-upgrade"); len(ups) != 1 || untarAll(t, ups[0].stdin)["values"] != "replicas: 2\n" {
		t.Fatalf("calls %q", h.fake.ops())
	}
}

// Part files a fetch cut off left (fetch.PartPattern) are removed once unwritten for partStale:
// at a fetch into their entry, and by a prune of the cache's entries kept; a newer one (a fetch
// under way) and any other file stay.
func TestStalePartsAreRemoved(t *testing.T) {
	t.Parallel()
	now := time.Now()
	parts := func(t *testing.T, dir string) (stale, fresh, other string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		stale, fresh, other = filepath.Join(dir, ".installer-1.part"), filepath.Join(dir, ".installer-2.part"), filepath.Join(dir, ".keys.json-3")
		for p, age := range map[string]time.Duration{stale: partStale, fresh: partStale - time.Minute, other: 2 * partStale} {
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
				t.Fatal(err)
			}
		}
		return stale, fresh, other
	}
	left := func(t *testing.T, gone string, kept ...string) {
		t.Helper()
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s left: %v", gone, err)
		}
		for _, k := range kept {
			if _, err := os.Stat(k); err != nil {
				t.Errorf("%s removed: %v", k, err)
			}
		}
	}
	plat := strings.ReplaceAll(Platform, "/", "-")
	// At the boundary, against a fixed now.
	dir := t.TempDir()
	stale, fresh, other := parts(t, dir)
	removeStaleParts(dir, now)
	left(t, stale, fresh, other)
	// A fetch into the entry, before it reads anything: one that fails then (no prune follows).
	d := &Data{CacheDir: t.TempDir()}
	stale, fresh, other = parts(t, filepath.Join(d.CacheDir, strings.TrimPrefix(digest, "sha256:"), plat))
	failing := func(context.Context, string, fetch.Options) (*fetch.Installer, error) {
		return nil, errors.New("the registry is away")
	}
	if _, err := (nativeSource{get: failing}).Program(context.Background(), d, Want{Ref: image}, t.TempDir()); err == nil {
		t.Fatal("no error")
	}
	left(t, stale, fresh, other)
	// A prune: the entry kept and another kept entry.
	cache := t.TempDir()
	keep := strings.Repeat("a", 64)
	s1, f1, o1 := parts(t, filepath.Join(cache, keep, plat))
	s2, f2, o2 := parts(t, filepath.Join(cache, strings.Repeat("b", 64), plat))
	prune(cache, keep)
	left(t, s1, f1, o1)
	left(t, s2, f2, o2)
}

// scriptedRunner answers each run with the next of its replies (the last one again after them),
// and counts the runs; after, when set, runs after each.
type scriptedRunner struct {
	replies []reply
	n       int
	after   func(n int)
}

func (s *scriptedRunner) Run(context.Context, []string, []byte) ([]byte, []byte, int, error) {
	r := s.replies[min(s.n, len(s.replies)-1)]
	s.n++
	if s.after != nil {
		s.after(s.n)
	}
	return []byte(r.stdout), []byte(r.stderr), r.exit, nil
}

// A run the shell could not exec as its program in the run's directory was busy (ETXTBSY) is made
// again, up to busyTries times; any other failure is the run's result at once.
func TestBusyRunnerRetries(t *testing.T) {
	t.Parallel()
	const dir = "/cache/runs/stowage-k8s-1/run"
	busyDash := reply{exit: 126, stderr: "./notes.sh: 2: exec: " + dir + "/notes-stow: Text file busy\n"}
	busyBash := reply{exit: 126, stderr: "./notes.sh: line 2: " + dir + "/notes-stow: Text file busy\n"}
	for name, c := range map[string]struct {
		replies []reply
		runs    int
		exit    int
	}{
		"busy, then runs":     {[]reply{busyDash, busyBash, {stdout: "ok"}}, 3, 0},
		"busy every time":     {[]reply{busyDash}, busyTries, 126},
		"another program":     {[]reply{{exit: 126, stderr: "./notes.sh: 2: exec: /elsewhere/notes-stow: Text file busy\n"}}, 1, 126},
		"not exit 126":        {[]reply{{exit: 1, stderr: dir + "/notes-stow: Text file busy\n"}}, 1, 1},
		"another 126":         {[]reply{{exit: 126, stderr: "./notes.sh: 2: exec: " + dir + "/notes-stow: Permission denied\n"}}, 1, 126},
		"the program's error": {[]reply{{exit: 3, stdout: `{"format":1}`}}, 1, 3},
	} {
		s := &scriptedRunner{replies: c.replies}
		_, _, exit, err := busyRunner{Runner: s, dir: dir, delay: time.Microsecond}.Run(context.Background(), []string{"sh"}, nil)
		if err != nil || exit != c.exit || s.n != c.runs {
			t.Errorf("%s: exit %d, %d runs, %v", name, exit, s.n, err)
		}
	}
	// A cancelled context stops the retries.
	ctx, cancel := context.WithCancel(context.Background())
	s := &scriptedRunner{replies: []reply{busyDash}, after: func(int) { cancel() }}
	if _, _, exit, _ := (busyRunner{Runner: s, dir: dir, delay: time.Hour}).Run(ctx, []string{"sh"}, nil); exit != 126 || s.n != 1 {
		t.Fatalf("cancelled: exit %d, %d runs", exit, s.n)
	}
}

// The real shell's ETXTBSY: a program held open for writing does not run, and once closed, the
// run made again runs it.
func TestBusyRunnerRealShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prog := filepath.Join(dir, "notes-stow")
	testutil.WriteExecutable(t, prog, "#!/bin/sh\necho ran \"$@\"\n")
	if err := wrapper(dir, "notes", prog); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(prog, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	s := &scriptedRunner{}
	inner := runFunc(func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
		stdout, stderr, exit, err := target.Local{}.Run(ctx, argv, stdin)
		if s.n++; s.n == 1 {
			if exit != 126 || !strings.Contains(strings.ToLower(string(stderr)), "text file busy") {
				t.Errorf("the first run: exit %d %q", exit, stderr)
			}
			_ = held.Close()
		}
		return stdout, stderr, exit, err
	})
	stdout, stderr, exit, err := busyRunner{Runner: inner, dir: dir, delay: time.Millisecond}.Run(context.Background(), []string{"sh", filepath.Join(dir, "notes.sh"), "x"}, nil)
	if err != nil || exit != 0 || string(stdout) != "ran x\n" || s.n < 2 {
		t.Fatalf("exit %d, %d runs, %q %q %v", exit, s.n, stdout, stderr, err)
	}
}

// runFunc is a function as a target.Runner.
type runFunc func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error)

func (f runFunc) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	return f(ctx, argv, stdin)
}
