package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const seedJSON = `{"format":1,"platform":"truenas","user_data":"#cloud-config\nssh_keys: …\n","known_hosts_line":"vm-seed-host-key ssh-ed25519 ` + edKey + `",
"fingerprint":"SHA256:abc","host_key_private":"-----BEGIN OPENSSH PRIVATE KEY-----\nx\n","disk":{"truenas":{"format":"qcow2","image":"registry.example.com/notes/notes-disk@` + digest + `","os_gib":23,"disk_gib":43},
"aws":{"format":"raw","image":"registry.example.com/notes/notes-disk@` + digest + `","os_gib":23,"disk_gib":43,"cloud_image":"notes-0123456789ab"}},"waiting":["API_KEY"],
"meta_data":"instance-id: app\nlocal-hostname: app\n","app_gib":20,"disk_file":{"path":"/disks/notes.qcow2","format":"qcow2","sha256":"ab12","disk_gib":43},"await_secrets":true}`

// seedHarness drives stowage_vm_seed through the protocol; the fake's "install" op is the
// fetched installer's run that is neither describe nor upgrade: vm seed.
type seedHarness struct {
	*harness
	styp tftypes.Object
}

func newSeedHarness(t *testing.T, providerConfig map[string]tftypes.Value) *seedHarness {
	h := newHarness(t, providerConfig)
	s, err := h.srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sh := &seedHarness{harness: h, styp: s.ResourceSchemas["stowage_vm_seed"].ValueType().(tftypes.Object)}
	h.fake.set("describe", reply{stdout: strings.Replace(describe1, `"shapes":["docker"]`, `"shapes":["docker","kairos"]`, 1)})
	h.fake.set("install", reply{stdout: seedJSON})
	return sh
}

func (s *seedHarness) config(vals map[string]tftypes.Value) tftypes.Value {
	base := map[string]tftypes.Value{"installer": str(image), "platform": str("truenas")}
	for k, v := range vals {
		base[k] = v
	}
	return fill(s.styp, base)
}

func (s *seedHarness) validate(config tftypes.Value) []*tfprotov6.Diagnostic {
	cfg := s.dv(s.styp, config)
	resp, err := s.srv.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "stowage_vm_seed", Config: &cfg})
	if err != nil {
		s.t.Fatal(err)
	}
	return resp.Diagnostics
}

func (s *seedHarness) plan(prior *tfprotov6.DynamicValue, config tftypes.Value) *tfprotov6.PlanResourceChangeResponse {
	if prior == nil {
		p := s.dv(s.styp, tftypes.NewValue(s.styp, nil))
		prior = &p
	}
	cfg := s.dv(s.styp, config)
	resp, err := s.srv.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: "stowage_vm_seed", PriorState: prior, ProposedNewState: &cfg, Config: &cfg})
	if err != nil {
		s.t.Fatal(err)
	}
	return resp
}

func (s *seedHarness) create(config tftypes.Value) *tfprotov6.ApplyResourceChangeResponse {
	p := s.plan(nil, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		s.t.Fatalf("plan: %s", d)
	}
	prior := s.dv(s.styp, tftypes.NewValue(s.styp, nil))
	cfg := s.dv(s.styp, config)
	resp, err := s.srv.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName: "stowage_vm_seed", PriorState: &prior, PlannedState: p.PlannedState, Config: &cfg})
	if err != nil {
		s.t.Fatal(err)
	}
	return resp
}

func (s *seedHarness) attrs(state *tfprotov6.DynamicValue) map[string]tftypes.Value {
	v, err := state.Unmarshal(s.styp)
	if err != nil {
		s.t.Fatal(err)
	}
	out := map[string]tftypes.Value{}
	if err := v.As(&out); err != nil {
		s.t.Fatal(err)
	}
	return out
}

func TestSeedCreate(t *testing.T) {
	t.Parallel()
	s := newSeedHarness(t, map[string]tftypes.Value{"username": str("pull"), "password": str(secretVal), "registry": str("mirror.example/notes")})
	a := s.create(s.config(map[string]tftypes.Value{
		"hostname": str("app"), "ssh_public_key": str("ssh-ed25519 AAAA me"), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"}),
		"await_secrets": boolean(true), "disk_dir": str("/disks"),
	}))
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := s.fake.ops(); !slices.Equal(got, []string{"fetch", "describe", "install"}) {
		t.Errorf("ops %q", got)
	}
	for _, c := range s.fake.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) {
			t.Errorf("the pull password on a command line: %q", c.argv)
		}
	}
	seed := s.fake.callsOf("install")[0]
	for _, want := range []string{"vm", "seed", "--json", "--platform=truenas", "--hostname=app", "--registry=mirror.example/notes", "ADMIN_EMAIL=a@b", "--await-secrets", "--disk-dir=/disks"} {
		if !slices.Contains(seed.argv, want) {
			t.Errorf("vm seed's argv lacks %q: %q", want, seed.argv)
		}
	}
	if seed.argv[5] != "ssh-key" || !strings.Contains(string(seed.stdin), "ssh-ed25519 AAAA me") {
		t.Errorf("the SSH key is not a file: %q", seed.argv)
	}
	st := s.attrs(a.NewState)
	if asString(t, st["id"]) != "SHA256:abc" || asString(t, st["short_name"]) != "notes" || asString(t, st["version"]) != "1.2.0" ||
		asString(t, st["known_hosts_line"]) != "vm-seed-host-key ssh-ed25519 "+edKey || !strings.HasPrefix(asString(t, st["host_key_private"]), "-----BEGIN") {
		t.Errorf("state %v", st)
	}
	var disk map[string]tftypes.Value
	if err := st["disk"].As(&disk); err != nil || disk["truenas"].IsNull() {
		t.Errorf("disk %v %v", st["disk"], err)
	}
	var aws, file map[string]tftypes.Value
	_ = disk["aws"].As(&aws)
	_ = st["disk_file"].As(&file)
	if asString(t, aws["cloud_image"]) != "notes-0123456789ab" || asString(t, st["meta_data"]) != "instance-id: app\nlocal-hostname: app\n" ||
		asString(t, file["path"]) != "/disks/notes.qcow2" || asString(t, file["sha256"]) != "ab12" {
		t.Errorf("aws %v, meta_data %v, disk_file %v", aws, st["meta_data"], file)
	}
	var tr map[string]tftypes.Value
	_ = disk["truenas"].As(&tr)
	if !tr["cloud_image"].IsNull() {
		t.Errorf("a disk with no cloud image: %v", tr["cloud_image"])
	}
	// Every argument replaces the seed: its key is generated once.
	p := s.plan(a.NewState, s.config(map[string]tftypes.Value{"hostname": str("app2"), "ssh_public_key": str("ssh-ed25519 AAAA me"), "inputs": strMap(map[string]string{"ADMIN_EMAIL": "a@b"})}))
	if len(p.RequiresReplace) == 0 {
		t.Error("a new hostname does not replace the seed")
	}
	// The schema marks the key's carriers sensitive.
	sch, _ := s.srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	for _, at := range sch.ResourceSchemas["stowage_vm_seed"].Block.Attributes {
		if (at.Name == "user_data" || at.Name == "host_key_private") != at.Sensitive {
			t.Errorf("%s: sensitive %v", at.Name, at.Sensitive)
		}
	}
}

func TestSeedRefusals(t *testing.T) {
	t.Parallel()
	s := newSeedHarness(t, nil)
	if got := errorsOf(s.validate(s.config(map[string]tftypes.Value{"tls": str("byo")}))); got != "" {
		t.Errorf("byo: %s", got)
	}
	for _, d := range []string{".", "disks", "/disks/", "/a/../b"} {
		if got := errorsOf(s.validate(s.config(map[string]tftypes.Value{"disk_dir": str(d)}))); !strings.Contains(got, "disk_dir is not an absolute, clean path") {
			t.Errorf("disk_dir %q: %s", d, got)
		}
	}
	if got := errorsOf(s.validate(s.config(map[string]tftypes.Value{"tls": str("byo"), "await_secrets": boolean(true)}))); !strings.Contains(got, "await_secrets with tls byo") {
		t.Errorf("byo with await_secrets: %s", got)
	}
	s.fake.set("install", reply{exit: 3, stdout: `{"format":1,"refused":{"reason":"API_KEY: a seed carries no secret input and no TLS material; they go after the boot, over SSH"}}`,
		stderr: "error: API_KEY: a seed carries no secret input and no TLS material; they go after the boot, over SSH\n"})
	a := s.create(s.config(map[string]tftypes.Value{"inputs": strMap(map[string]string{"API_KEY": "x"})}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "a seed carries no secret input") {
		t.Errorf("a refused seed: %s", got)
	}
	s2 := newSeedHarness(t, nil)
	s2.fake.set("describe", reply{stdout: describe1}) // docker alone
	a = s2.create(s2.config(nil))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "No Kairos shape") || slices.Contains(s2.fake.ops(), "install") {
		t.Errorf("no kairos shape: %s %q", got, s2.fake.ops())
	}
}

// Validation reads inputs by key: a value not known yet (a variable at validate) is no error.
func TestSeedValidateUnknownInput(t *testing.T) {
	t.Parallel()
	s := newSeedHarness(t, nil)
	cfg := s.config(map[string]tftypes.Value{"inputs": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
		map[string]tftypes.Value{"ADMIN_EMAIL": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})})
	if got := errorsOf(s.validate(cfg)); got != "" {
		t.Fatalf("%q", got)
	}
	bad := s.config(map[string]tftypes.Value{"inputs": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
		map[string]tftypes.Value{"A=B": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})})
	if got := errorsOf(s.validate(bad)); !strings.Contains(got, "not an input key") {
		t.Fatalf("a bad key: %q", got)
	}
}

// The seed's run logs the pull account in to the installer's registry alone: a provider registry
// on another host (a mirror) is read with the machine's own login, the account never sent there.
func TestSeedLoginIsTheInstallerRegistrys(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		registry, wantHost, wantUser string
	}{
		"no registry":            {"", "registry.example.com", "pull"},
		"the installer's host":   {"registry.example.com/mirror", "registry.example.com", "pull"},
		"a mirror on other host": {"mirror.lan:5000/site", "", ""},
	} {
		cfg := map[string]tftypes.Value{"username": str("pull"), "password": str(secretVal)}
		if c.registry != "" {
			cfg["registry"] = str(c.registry)
		}
		s := newSeedHarness(t, cfg)
		if d := errorsOf(s.create(s.config(nil)).Diagnostics); d != "" {
			t.Fatalf("%s: %s", name, d)
		}
		seed := s.fake.callsOf("install")[0]
		if seed.argv[6] != c.wantHost || seed.argv[7] != c.wantUser || (c.wantUser == "") == strings.Contains(string(seed.stdin), secretVal) {
			t.Errorf("%s: login %q %q", name, seed.argv[6], seed.argv[7])
		}
	}
}

// The seed reads a mirror with its own login (in the tar), the pull account never sent there.
func TestSeedMirrorLogin(t *testing.T) {
	t.Parallel()
	s := newSeedHarness(t, map[string]tftypes.Value{"username": str("pull"), "password": str("pull-" + secretVal),
		"registry": str("mirror.lan:5000/site"), "registry_username": str("site"), "registry_password": str("site-" + secretVal)})
	if d := errorsOf(s.create(s.config(nil)).Diagnostics); d != "" {
		t.Fatal(d)
	}
	seed := s.fake.callsOf("install")[0]
	files := untarAll(t, seed.stdin)
	if seed.argv[6] != "" || files["mirror-host"] != "mirror.lan:5000" || files["mirror-password"] != "site-"+secretVal || strings.Contains(string(seed.stdin), "pull-"+secretVal) {
		t.Fatalf("seed %q %q", seed.argv[6:8], files)
	}
}
