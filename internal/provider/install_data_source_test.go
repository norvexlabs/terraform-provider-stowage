package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func (h *harness) readData(vals map[string]tftypes.Value) (*tfprotov6.ReadDataSourceResponse, tftypes.Object) {
	h.t.Helper()
	s, err := h.srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	typ := s.DataSourceSchemas["stowage_install"].ValueType().(tftypes.Object)
	cfg := h.dv(typ, fill(typ, vals))
	resp, err := h.srv.ReadDataSource(context.Background(), &tfprotov6.ReadDataSourceRequest{TypeName: "stowage_install", Config: &cfg})
	if err != nil {
		h.t.Fatal(err)
	}
	return resp, typ
}

// The stowage_install data source reads a Docker install's status by the installer it keeps,
// changing nothing, and refuses no install and another project's.
func TestInstallDataSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.fake.set("status", reply{stdout: statusHeld})
	block := func(typ tftypes.Object) tftypes.Value {
		bt := typ.AttributeTypes["docker"].(tftypes.Object)
		return fill(bt, map[string]tftypes.Value{"host": str("app.example"), "user": str("ops"), "known_hosts_line": str(hostLine), "work_dir": str("/srv")})
	}
	_, typ := h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes")})
	resp, _ := h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes"), "docker": block(typ)})
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	v, err := resp.State.Unmarshal(typ)
	if err != nil {
		t.Fatal(err)
	}
	at := map[string]tftypes.Value{}
	_ = v.As(&at)
	var inst map[string]tftypes.Value
	_ = at["installed"].As(&inst)
	if asString(t, at["id"]) != "/srv/notes" || asString(t, inst["version"]) != "1.2.0" || asString(t, at["registry"]) != "mirror.lan/notes" ||
		asString(t, at["registry_ca_digest"]) != "sha256:aa" || asString(t, at["status"]) != "installed" {
		t.Fatalf("state %v", at)
	}
	if ops := h.fake.ops(); len(ops) != 1 || ops[0] != "status" {
		t.Fatalf("calls %q", ops)
	}
	h.fake.set("status", reply{stdout: strings.Replace(statusHeld, `"name":"notes"`, `"name":"atlas"`, 1)})
	resp, _ = h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes"), "docker": block(typ)})
	if got := errorsOf(resp.Diagnostics); !strings.Contains(got, "Another project's install") {
		t.Fatalf("another project: %q", got)
	}
	none, _ := h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes")})
	if got := errorsOf(none.Diagnostics); !strings.Contains(got, "exactly one of the docker and vm blocks") {
		t.Fatalf("no block: %q", got)
	}
}

// On a VM, the data source reads its status by its OS bundle's installer.
func TestInstallDataSourceVM(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "b1", `,"registry":"mirror.lan/notes"`)})
	_, typ := h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes")})
	bt := typ.AttributeTypes["vm"].(tftypes.Object)
	resp, _ := h.readData(map[string]tftypes.Value{"name": str("notes"), "short_name": str("notes"),
		"vm": fill(bt, map[string]tftypes.Value{"host": str("192.0.2.7"), "user": str("ops"), "known_hosts_line": str(seedLine)})})
	if d := errorsOf(resp.Diagnostics); d != "" {
		t.Fatal(d)
	}
	v, _ := resp.State.Unmarshal(typ)
	at := map[string]tftypes.Value{}
	_ = v.As(&at)
	if asString(t, at["id"]) != "vm:192.0.2.7/notes-stow" || asString(t, at["registry"]) != "mirror.lan/notes" || !at["registry_ca_digest"].IsNull() {
		t.Fatalf("state %v", at)
	}
}
