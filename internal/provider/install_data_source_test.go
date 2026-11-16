package provider

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func (h *harness) readData(kind string, vals map[string]tftypes.Value) (*tfprotov6.ReadDataSourceResponse, tftypes.Object) {
	h.t.Helper()
	s, err := h.srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	name := shapes[kind].typeName("stowage")
	typ := s.DataSourceSchemas[name].ValueType().(tftypes.Object)
	cfg := h.dv(typ, fill(typ, vals))
	resp, err := h.srv.ReadDataSource(context.Background(), &tfprotov6.ReadDataSourceRequest{TypeName: name, Config: &cfg})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, typ
}

// dataAttrs is a data source's state.
func dataAttrs(t *testing.T, resp *tfprotov6.ReadDataSourceResponse, typ tftypes.Object) map[string]tftypes.Value {
	t.Helper()
	v, err := resp.State.Unmarshal(typ)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]tftypes.Value{}
	_ = v.As(&at)
	return at
}

// The stowage_docker_install data source reads a Docker install's status by the installer it
// keeps, changing nothing, and refuses no install and another project's.
func TestInstallDataSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"format":1,`, `"format":1,"values":{"pull_login":true},`, 1)})
	conn := map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes"), "host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "work_dir": str("/srv")}
	resp, typ := h.readData("docker", conn)
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	at := dataAttrs(t, resp, typ)
	var inst map[string]tftypes.Value
	_ = at["installed"].As(&inst)
	if asString(t, at["id"]) != "/srv/notes" || asString(t, inst["version"]) != "1.2.0" || asString(t, at["registry"]) != "mirror.lan/notes" ||
		asString(t, at["registry_ca_digest"]) != "sha256:aa" || asString(t, at["status"]) != "installed" || !at["pull_login"].Equal(boolean(true)) {
		t.Fatalf("state %v", at)
	}
	if ops := h.fake.ops(); len(ops) != 1 || ops[0] != "status" || !contains(h.fake.calls[0].argv, "--values") {
		t.Fatalf("calls %q", ops)
	}
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"name":"notes"`, `"name":"atlas"`, 1)})
	resp, _ = h.readData("docker", conn)
	if got := errorsOf(resp.Diagnostics); !strings.Contains(got, "Another project's install") {
		t.Fatalf("another project: %q", got)
	}
	rel := maps.Clone(conn)
	rel["work_dir"] = str("srv")
	if r, _ := h.readData("docker", rel); !strings.Contains(errorsOf(r.Diagnostics), "work_dir is not an absolute, clean path") {
		t.Fatalf("a relative work_dir: %q", errorsOf(r.Diagnostics))
	}
	noLine := maps.Clone(conn)
	delete(noLine, "known_hosts_line")
	if r, _ := h.readData("docker", noLine); !strings.Contains(errorsOf(r.Diagnostics), "known_hosts_line is required with host") {
		t.Fatalf("no known_hosts_line: %q", errorsOf(r.Diagnostics))
	}
}

// On a VM, the data source reads its status by its OS bundle's installer.
func TestInstallDataSourceVM(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "b1", `,"registry":"mirror.lan/notes"`)})
	resp, typ := h.readData("vm", map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes"),
		"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine)})
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	at := dataAttrs(t, resp, typ)
	if asString(t, at["id"]) != "192.0.2.7/notes-stow" || asString(t, at["registry"]) != "mirror.lan/notes" || !at["registry_ca_digest"].IsNull() || !at["pull_login"].IsNull() {
		t.Fatalf("state %v", at)
	}
}

// On Kubernetes, the data source runs the installer given on this machine: its k8s status.
func TestInstallDataSourceK8s(t *testing.T) {
	t.Parallel()
	h := k8sHarness(t, nil)
	resp, typ := h.readData("kubernetes", map[string]tftypes.Value{"installer": str(image), "namespace": str("prod")})
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	at := dataAttrs(t, resp, typ)
	if asString(t, at["id"]) != "prod/notes" || asString(t, at["status"]) != "installed" {
		t.Fatalf("state %v", at)
	}
	if got := h.fake.ops(); !slices.Equal(got, []string{"tools", "fetch", "k8s-status"}) || !contains(h.fake.callsOf("k8s-status")[0].argv, "--values") {
		t.Fatalf("calls %q", got)
	}
	h.fake.set("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	if r, _ := h.readData("kubernetes", map[string]tftypes.Value{"installer": str(image), "namespace": str("prod")}); !strings.Contains(errorsOf(r.Diagnostics), "holds no release prod/notes") {
		t.Fatalf("no release: %q", errorsOf(r.Diagnostics))
	}
}
