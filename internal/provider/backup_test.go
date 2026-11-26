package provider

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

const backupSettingsJSON = `{"configured":true,"destination":"dav:backups","rclone_config_set":true,"rclone_config_from":"operator",` +
	`"recipients":["age1ops"],"retention_days":30,"schedule":"daily","profile":"full","locked":false,"profiles":["full"]}`

func (h *harness) backupValue(vals map[string]tftypes.Value) tftypes.Value {
	return fill(h.typ.AttributeTypes["backup"].(tftypes.Object), vals)
}

func (h *harness) restoreValue(object string) tftypes.Value {
	return fill(h.typ.AttributeTypes["restore"].(tftypes.Object), map[string]tftypes.Value{"object": str(object)})
}

// exportFile is a decrypted secrets export, as the installer's `secrets export` writes it.
var exportFile = "project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: export-" + secretVal + "\n"

// madeAgain is a Docker configuration of a machine made again: secrets, backup settings to a
// destination, a restore.
func (h *harness) madeAgain(extra map[string]tftypes.Value) tftypes.Value {
	vals := map[string]tftypes.Value{
		"secrets_wo":                      str(exportFile),
		"backup":                          h.backupValue(map[string]tftypes.Value{"destination": str("dav:backups"), "recipients": strList("age1ops")}),
		"backup_rclone_config_wo":         str("[dav]\npass = rclone-" + secretVal + "\n"),
		"backup_rclone_config_wo_version": num(1),
		"restore":                         h.restoreValue("notes-full-x.pgdump.age"),
		"restore_identity_wo":             str("AGE-SECRET-KEY-" + secretVal),
	}
	for k, v := range extra {
		vals[k] = v
	}
	return h.dockerConfig(vals)
}

func (h *harness) backupAttr(t *testing.T, state *tfprotov6.DynamicValue, name string) tftypes.Value {
	t.Helper()
	b := map[string]tftypes.Value{}
	if err := h.attrs(state)["backup"].As(&b); err != nil {
		t.Fatal(err)
	}
	return b[name]
}

func TestMachineMadeAgainOnDocker(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{})
	config := h.madeAgain(nil)
	state := h.create(config)
	ops := h.fake.ops()
	if got := strings.Join(ops[len(ops)-4:], " "); got != "status backup status restore" {
		t.Fatalf("calls %q: the settings, then the restore, after the install", ops)
	}
	// Each secret on standard input alone, never in the command line or state.
	for _, c := range h.fake.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) {
			t.Fatalf("a secret is on the %s command line: %q", c.op(), c.argv)
		}
	}
	if bytes.Contains(state.MsgPack, []byte(secretVal)) {
		t.Fatal("a secret is in the state")
	}
	cfg := h.fake.callsOf("backup")[0]
	if !strings.Contains(strings.Join(cfg.argv, " "), "backup config --destination dav:backups --recipient age1ops --json") ||
		untarAll(t, cfg.stdin)["rclone-config"] != "[dav]\npass = rclone-"+secretVal+"\n" {
		t.Fatalf("backup config %q", cfg.argv)
	}
	rs := h.fake.callsOf("restore")[0]
	if !strings.Contains(strings.Join(rs.argv, " "), "identity dir restore notes-full-x.pgdump.age --yes") || strings.Contains(strings.Join(rs.argv, " "), "--unrecorded") ||
		untarAll(t, rs.stdin)["identity"] != "AGE-SECRET-KEY-"+secretVal {
		t.Fatalf("restore %q", rs.argv)
	}
	// The computed settings come from the install's answer; nothing is left pending.
	if asString(t, h.backupAttr(t, state, "schedule")) != "daily" || asString(t, h.backupAttr(t, state, "destination")) != "dav:backups" {
		t.Fatalf("backup in state: %v", h.attrs(state)["backup"])
	}
	if bytes.Contains(h.privateOf(state), []byte(backupPendingKey)) {
		t.Fatal("a step is pending after a create that ran them all")
	}
	// The next plan changes nothing, and the restore is never run again.
	h.fake.reset()
	p := h.plan(state, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if !h.value(p.PlannedState).Equal(h.value(state)) {
		t.Fatalf("a plan after the create changes it: %v", h.attrs(p.PlannedState))
	}
	// A restore changed afterwards: the warning names the restore by hand of the backup now given.
	if w := diagText(h.plan(state, h.madeAgain(map[string]tftypes.Value{"restore": h.restoreValue("notes-full-y.pgdump.age")})).Diagnostics); !strings.Contains(w, "`/srv/notes/notes-stow restore notes-full-y.pgdump.age --yes --identity - --dir /srv/notes` on the host") {
		t.Fatalf("the warning's way by hand: %s", w)
	}
	h.update(state, h.madeAgain(map[string]tftypes.Value{"restore": h.restoreValue("notes-full-y.pgdump.age")}))
	if len(h.fake.callsOf("restore")) != 0 {
		t.Fatal("a restore ran after the create")
	}
}

func TestRestoreThatFailsIsRetried(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{exit: 3, stderr: "error: notes-full-x.pgdump.age: this install recorded no digest of it\n"})
	config := h.madeAgain(nil)
	p := h.plan(nil, config)
	a := h.apply(nil, p.PlannedState, config)
	// A warning, not an error: the install is recorded, untainted.
	if d := errorsOf(a.Diagnostics); d != "" {
		t.Fatalf("the create failed: %s", d)
	}
	if !strings.Contains(diagText(a.Diagnostics), "this install recorded no digest of it") {
		t.Fatalf("no warning names the refusal: %s", diagText(a.Diagnostics))
	}
	state := a.NewState
	if !bytes.Contains(h.privateOf(state), []byte(backupPendingKey)) {
		t.Fatal("the restore is not recorded pending")
	}
	// The next plan is an update; the update restores, with the configuration's fix.
	if p := h.plan(state, config); h.attrs(p.PlannedState)["status"].IsKnown() {
		t.Fatal("the plan after a failed restore shows no update")
	}
	h.fake.reset()
	h.fake.set("restore", reply{})
	h.fake.set("status", reply{stdout: awaitingStatus(h)})
	fixed := h.madeAgain(map[string]tftypes.Value{"restore": fill(h.typ.AttributeTypes["restore"].(tftypes.Object),
		map[string]tftypes.Value{"object": str("notes-full-x.pgdump.age"), "unrecorded": boolean(true)})})
	u := h.update(state, fixed)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if got := h.fake.ops(); strings.Join(got, " ") != "status restore" || !strings.Contains(strings.Join(h.fake.callsOf("restore")[0].argv, " "), "--unrecorded") {
		t.Fatalf("the retry ran %q", got)
	}
	if bytes.Contains(h.privateOf(u.NewState), []byte(backupPendingKey)) {
		t.Fatal("still pending after the restore")
	}
	// An update whose backup config fails is an error, the step still pending.
	h2 := madeAgainHarness(t)
	h2.fake.set("backup", reply{exit: 1, stderr: "rclone: the destination does not answer\n"})
	a = h2.apply(nil, h2.plan(nil, h2.madeAgain(nil)).PlannedState, h2.madeAgain(nil))
	if errorsOf(a.Diagnostics) != "" || len(h2.fake.callsOf("restore")) != 0 {
		t.Fatalf("settings that fail: %s, restores %d", errorsOf(a.Diagnostics), len(h2.fake.callsOf("restore")))
	}
	u = h2.update(a.NewState, h2.madeAgain(nil))
	if !strings.Contains(errorsOf(u.Diagnostics), "Setting the backup settings failed") || !bytes.Contains(h2.privateOf(u.NewState), []byte(backupPendingKey)) {
		t.Fatalf("the update's settings that fail: %s", diagText(u.Diagnostics))
	}
}

func TestRestoreNeedsAFreshInstall(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	// Without secrets_wo or the identity: refused at plan.
	for name, cfg := range map[string]tftypes.Value{
		"secrets_wo":          h.madeAgain(map[string]tftypes.Value{"secrets_wo": tftypes.NewValue(tftypes.String, nil)}),
		"restore_identity_wo": h.madeAgain(map[string]tftypes.Value{"restore_identity_wo": tftypes.NewValue(tftypes.String, nil)}),
	} {
		if d := errorsOf(h.plan(nil, cfg).Diagnostics); !strings.Contains(d, name) {
			t.Errorf("a restore without %s: %q", name, d)
		}
	}
	// An install already in the directory is run again, its data kept: nothing restored.
	h.fake.set("new-status", h.fake.replies["status"])
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	a := h.apply(nil, h.plan(nil, h.madeAgain(nil)).PlannedState, h.madeAgain(nil))
	if errorsOf(a.Diagnostics) != "" || len(h.fake.callsOf("restore")) != 0 || !strings.Contains(diagText(a.Diagnostics), "Not restored: the install does not await its data") {
		t.Fatalf("restored over an install that was there: %s", diagText(a.Diagnostics))
	}
	// One an earlier create made from a secrets export, still awaiting its data, is restored.
	h2 := newHarness(t, nil)
	awaiting := strings.Replace(h2.fake.replies["status"].stdout, `"channel"`, `"restore_awaited":true,"channel"`, 1)
	h2.fake.set("new-status", reply{stdout: awaiting})
	h2.fake.set("status", reply{stdout: awaiting})
	h2.fake.set("backup", reply{stdout: backupSettingsJSON})
	h2.fake.set("restore", reply{})
	a = h2.apply(nil, h2.plan(nil, h2.madeAgain(nil)).PlannedState, h2.madeAgain(nil))
	if errorsOf(a.Diagnostics) != "" || len(h2.fake.callsOf("restore")) != 1 {
		t.Fatalf("an install awaiting its data was not restored: %s", diagText(a.Diagnostics))
	}
}

func TestBackupValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	null := tftypes.NewValue(tftypes.String, nil)
	for want, extra := range map[string]map[string]tftypes.Value{
		"backup needs a destination or keep_local": {"backup": h.backupValue(map[string]tftypes.Value{"recipients": strList("age1ops")}),
			"backup_rclone_config_wo": null, "backup_rclone_config_wo_version": tftypes.NewValue(tftypes.Number, nil)},
		"backup_rclone_config_wo with keep_local":                         {"backup": h.backupValue(map[string]tftypes.Value{"keep_local": num(2), "recipients": strList("age1ops")})},
		"backup_rclone_config_wo without backup":                          {"backup": tftypes.NewValue(h.typ.AttributeTypes["backup"], nil)},
		"backup_rclone_config_wo_version without backup_rclone_config_wo": {"backup_rclone_config_wo": null},
		"restore_identity_wo without restore":                             {"restore": tftypes.NewValue(h.typ.AttributeTypes["restore"], nil)},
	} {
		if d := errorsOf(h.validate(h.madeAgain(extra))); !strings.Contains(d, want) {
			t.Errorf("%s: %q", want, d)
		}
	}
	if d := errorsOf(h.validate(h.madeAgain(nil))); d != "" {
		t.Errorf("a valid configuration: %s", d)
	}
	// keep_local's least, 1, is taken.
	one := h.madeAgain(map[string]tftypes.Value{"backup": h.backupValue(map[string]tftypes.Value{"keep_local": num(1), "recipients": strList("age1ops")}),
		"backup_rclone_config_wo": null, "backup_rclone_config_wo_version": tftypes.NewValue(tftypes.Number, nil)})
	if d := errorsOf(h.validate(one)); d != "" {
		t.Errorf("keep_local 1: %s", d)
	}
	for want, b := range map[string]tftypes.Value{
		"destination and keep_local": h.backupValue(map[string]tftypes.Value{"destination": str("dav:b"), "keep_local": num(2), "recipients": strList("age1ops")}),
		"keep_local":                 h.backupValue(map[string]tftypes.Value{"keep_local": num(0), "recipients": strList("age1ops")}),
	} {
		if d := errorsOf(h.validate(h.madeAgain(map[string]tftypes.Value{"backup": b, "backup_rclone_config_wo": null,
			"backup_rclone_config_wo_version": tftypes.NewValue(tftypes.Number, nil)}))); !strings.Contains(d, want) {
			t.Errorf("%s: %q", want, d)
		}
	}
	// Settings not known yet (a variable's, a module's): not checked, the rclone config with them.
	unknown := tftypes.NewValue(h.typ.AttributeTypes["backup"], tftypes.UnknownValue)
	if d := errorsOf(h.validate(h.madeAgain(map[string]tftypes.Value{"backup": unknown}))); d != "" {
		t.Errorf("unknown settings: %s", d)
	}
}

func TestBackupReadShowsChangesOnTheTarget(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{})
	config := h.madeAgain(nil)
	state := h.create(config)
	// An rclone config not the operator's (saved through the app socket): the version goes, so
	// the next plan sends it again.
	h.fake.set("backup", reply{stdout: strings.Replace(backupSettingsJSON, `"rclone_config_from":"operator"`, `"rclone_config_from":"unknown"`, 1)})
	r := h.read(state)
	if d := errorsOf(r.Diagnostics); d != "" || !h.attrs(r.NewState)["backup_rclone_config_wo_version"].IsNull() {
		t.Fatalf("the refresh kept the rclone config's version: %s %v", d, h.attrs(r.NewState)["backup_rclone_config_wo_version"])
	}
	if h.value(h.plan(r.NewState, config).PlannedState).Equal(h.value(r.NewState)) {
		t.Fatal("no update planned to send the rclone config again")
	}
	// Removed on the target: the refresh shows it.
	h.fake.set("backup", reply{stdout: `{"configured":false,"rclone_config_set":false,"recipients":[],"retention_days":30,"schedule":"daily","profile":"","locked":false,"profiles":["full"]}`})
	r = h.read(state)
	if d := errorsOf(r.Diagnostics); d != "" || !h.backupAttr(t, r.NewState, "destination").IsNull() {
		t.Fatalf("the refresh kept the settings: %s %v", d, h.attrs(r.NewState)["backup"])
	}
	h.fake.reset()
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	u := h.update(r.NewState, config)
	if d := errorsOf(u.Diagnostics); d != "" {
		t.Fatal(d)
	}
	if calls := h.fake.callsOf("backup"); len(calls) != 1 || untarAll(t, calls[0].stdin)["rclone-config"] == "" {
		t.Fatalf("the update did not set the settings and the rclone config again: %q", h.fake.ops())
	}
	// Left out, the settings are not read.
	h.fake.reset()
	plain := h.create(h.dockerConfig(nil))
	h.fake.reset()
	h.read(plain)
	if len(h.fake.callsOf("backup")) != 0 {
		t.Fatal("settings the configuration leaves out were read")
	}
}

// On a VM made from a seed awaiting its secrets: the settings kept on the host, read back over
// SSH, and the restore, each file on standard input, after the install.
func TestMachineMadeAgainOnVM(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	// Its first install awaits the secrets export: no install yet; once given it, it awaits its data.
	f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: awaitingSecrets()})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"restore_awaited":true`)})
	f.set("backup", reply{stdout: strings.Replace(backupSettingsJSON, `"destination":"dav:backups"`, `"keep_local":2`, 1)})
	f.set("restore", reply{})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{
		"secrets_wo":          str(exportFile),
		"backup":              h.backupValue(map[string]tftypes.Value{"keep_local": num(2), "recipients": strList("age1ops")}),
		"restore":             h.restoreValue("notes-full-x.pgdump.age"),
		"restore_identity_wo": str("AGE-SECRET-KEY-" + secretVal),
	})
	state := h.create(config)
	ops := strings.Join(f.ops(), " ")
	if !strings.Contains(ops, "install") || !strings.Contains(ops, "backup status restore") || strings.Index(ops, "install") > strings.Index(ops, "backup") {
		t.Fatalf("calls %q: the install, then the settings, then the restore", ops)
	}
	cfg := strings.Join(f.callsOf("backup")[0].argv, " ")
	if !strings.Contains(cfg, "sudo -n notes-stow backup config --keep-local 2 --recipient age1ops --json") || f.callsOf("backup")[0].stdin != nil {
		t.Fatalf("backup config %q", cfg)
	}
	rs := f.callsOf("restore")[0]
	if !strings.Contains(strings.Join(rs.argv, " "), "sudo -n notes-stow restore notes-full-x.pgdump.age --yes") || untarAll(t, rs.stdin)["identity"] != "AGE-SECRET-KEY-"+secretVal {
		t.Fatalf("restore %q", rs.argv)
	}
	for _, c := range f.calls {
		if strings.Contains(strings.Join(c.argv, " "), secretVal) {
			t.Fatalf("a secret on the command line: %q", c.argv)
		}
	}
	if bytes.Contains(state.MsgPack, []byte(secretVal)) || asString(t, h.backupAttr(t, state, "schedule")) != "daily" {
		t.Fatalf("state: %v", h.attrs(state)["backup"])
	}
	// A refresh reads the settings over SSH (no file).
	f.reset()
	if r := h.read(state); errorsOf(r.Diagnostics) != "" || len(f.callsOf("backup")) != 1 {
		t.Fatalf("read: %s %q", errorsOf(r.Diagnostics), f.ops())
	}
}

// An apply saves what its plan said, whatever the install answers: the next refresh reads it.
func TestBackupSavesThePlannedSettings(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON}) // retention_days 30
	h.fake.set("restore", reply{})
	config := h.madeAgain(map[string]tftypes.Value{"backup": h.backupValue(map[string]tftypes.Value{
		"destination": str("dav:backups"), "recipients": strList("age1ops"), "retention_days": num(7)})})
	state := h.create(config)
	if !h.backupAttr(t, state, "retention_days").Equal(num(7)) {
		t.Fatalf("retention_days %v", h.backupAttr(t, state, "retention_days"))
	}
	if r := h.read(state); h.value(h.plan(r.NewState, config).PlannedState).Equal(h.value(r.NewState)) {
		t.Fatal("the refresh did not show the install's retention")
	}
}

// awaitingStatus is the fake's status of an install awaiting its data.
func awaitingStatus(h *harness) string {
	return strings.Replace(h.fake.replies["status"].stdout, `"channel"`, `"restore_awaited":true,"channel"`, 1)
}

// A pending restore runs again only while the install awaits its data; else it is dropped with a
// warning, never run over data since. Given no identity, the plan refuses it.
func TestPendingRestoreChecksTheInstallFirst(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{exit: 1, stderr: "the restore's job was cut\n"})
	config := h.madeAgain(nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if errorsOf(a.Diagnostics) != "" || !bytes.Contains(h.privateOf(a.NewState), []byte(backupPendingKey)) {
		t.Fatalf("create: %s", diagText(a.Diagnostics))
	}
	state := a.NewState
	// No identity: refused at plan.
	if d := errorsOf(h.plan(state, h.madeAgain(map[string]tftypes.Value{"restore_identity_wo": tftypes.NewValue(tftypes.String, nil)})).Diagnostics); !strings.Contains(d, "restore_identity_wo is not given") {
		t.Fatalf("a pending restore without its identity: %q", d)
	}
	// The job finished after all: the install no longer awaits its data.
	h.fake.reset()
	h.fake.set("restore", reply{})
	h.fake.set("status", reply{stdout: newFake().replies["status"].stdout})
	u := h.update(state, config)
	if errorsOf(u.Diagnostics) != "" || len(h.fake.callsOf("restore")) != 0 || !strings.Contains(diagText(u.Diagnostics), "Not restored: the install does not await its data") {
		t.Fatalf("restored over an install that no longer awaits: %s %q", diagText(u.Diagnostics), h.fake.ops())
	}
	if bytes.Contains(h.privateOf(u.NewState), []byte(backupPendingKey)) {
		t.Fatal("the restore is still pending")
	}
	// A status that cannot be read: an error, the restore still pending.
	h.fake.set("status", reply{exit: 1, stderr: "boom\n"})
	u = h.update(state, config)
	if !strings.Contains(errorsOf(u.Diagnostics), "Reading the install before the restore") || len(h.fake.callsOf("restore")) != 0 {
		t.Fatalf("unread status: %s", diagText(u.Diagnostics))
	}
}

// A restore changed once the install was restored is recorded with a warning, nothing restored.
func TestRestoreChangedAfterwardsIsRecorded(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{})
	state := h.create(h.madeAgain(nil))
	p := h.plan(state, h.madeAgain(map[string]tftypes.Value{"restore": h.restoreValue("notes-full-y.pgdump.age")}))
	if !strings.Contains(diagText(p.Diagnostics), "restore is taken at create alone") {
		t.Fatalf("no warning: %s", diagText(p.Diagnostics))
	}
}

// A restore given no identity, and steps that cannot reach their target (here, the provider
// unconfigured), are told by fail and stay pending.
func TestBackupStepsThatCannotRun(t *testing.T) {
	t.Parallel()
	restore := types.ObjectValueMust(map[string]attr.Type{"object": types.StringType, "unrecorded": types.BoolType},
		map[string]attr.Value{"object": types.StringValue("o"), "unrecorded": types.BoolValue(false)})
	for _, c := range []struct {
		r    *installResource
		want string
	}{
		{&installResource{}, "The restore did not run"},
		{&installResource{wo: writeOnly{identity: "AGE-SECRET-KEY-1"}}, "The backup settings and the restore did not run"},
	} {
		m := installModel{Restore: restore, Backup: types.ObjectNull(backupAttrTypes), Docker: &dockerModel{}, ShortName: types.StringValue("notes")}
		var failed []string
		var diags diag.Diagnostics
		p := &memPrivate{}
		c.r.backupSteps(context.Background(), &m, backupPending{Restore: true}, p, func(s, _ string) { failed = append(failed, s) }, func() (bool, error) { return true, nil }, &diags)
		if len(failed) != 1 || failed[0] != c.want || !bytes.Contains(p.data[backupPendingKey], []byte("restore")) {
			t.Errorf("%s: failed %q, pending %q", c.want, failed, p.data[backupPendingKey])
		}
	}
}

// memPrivate is private state in memory.
type memPrivate struct{ data map[string][]byte }

func (p *memPrivate) GetKey(_ context.Context, k string) ([]byte, diag.Diagnostics) {
	return p.data[k], nil
}
func (p *memPrivate) SetKey(_ context.Context, k string, v []byte) diag.Diagnostics {
	if p.data == nil {
		p.data = map[string][]byte{}
	}
	p.data[k] = v
	return nil
}

// awaitingSecrets is a VM's status while its first install awaits a secrets export.
func awaitingSecrets() string {
	return strings.Replace(vmStatusJSON("1.1.0", "boot-1", ""), `"installed":{"version":"1.1.0","revision":1}`, `"installed":null`, 1)
}

// A VM create applied again after its first install ran: while that install awaits its data, the
// secrets export is not given again (the installer would refuse it) and the restore runs; an
// install that does not await it is refused before anything runs.
func TestVMCreateAgainAfterItsInstall(t *testing.T) {
	t.Parallel()
	for _, awaiting := range []bool{true, false} {
		h, f := newVMHarness(t)
		if awaiting {
			f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"restore_awaited":true,"made_again":true`)})
		}
		f.set("backup", reply{stdout: backupSettingsJSON})
		f.set("restore", reply{})
		config := h.vmConfig("1.1.0", map[string]tftypes.Value{
			"secrets_wo":          str(exportFile),
			"backup":              h.backupValue(map[string]tftypes.Value{"destination": str("dav:backups"), "recipients": strList("age1ops")}),
			"restore":             h.restoreValue("notes-full-x.pgdump.age"),
			"restore_identity_wo": str("AGE-SECRET-KEY-" + secretVal),
		})
		a := h.apply(nil, h.plan(nil, config).PlannedState, config)
		given := false
		for _, in := range f.callsOf("install") {
			given = given || in.stdin != nil && untarAll(t, in.stdin)["secrets"] != ""
		}
		text := diagText(a.Diagnostics)
		switch {
		case awaiting && (errorsOf(a.Diagnostics) != "" || given || len(f.callsOf("restore")) != 1 || !strings.Contains(text, "The VM's install took the secrets export")):
			t.Fatalf("awaiting: given %v, restores %d, %s", given, len(f.callsOf("restore")), text)
		case !awaiting && (given || len(f.callsOf("install")) != 0 || len(f.callsOf("restore")) != 0 || !strings.Contains(errorsOf(a.Diagnostics), "The VM holds an install of its own")):
			t.Fatalf("not awaiting: given %v, restores %d, %s", given, len(f.callsOf("restore")), text)
		}
	}
}

// A VM create applied again on an installed VM given a new install's secrets file (no export:
// true): a plain install says nothing of the file, so the create is refused before anything runs,
// naming the way on for both cases.
func TestVMCreateOnAnInstalledVMGivenASeededFile(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{"secrets_wo": str("project: notes\nsecrets:\n  SESSION_SECRET: seeded-" + secretVal + "\n")})
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	d := errorsOf(a.Diagnostics)
	if len(f.callsOf("install")) != 0 || !strings.Contains(d, "secrets_wo is a new install's secrets file") || !strings.Contains(d, "remove secrets_wo to take that install as it is") || strings.Contains(d, secretVal) {
		t.Fatalf("installs %d: %s", len(f.callsOf("install")), d)
	}
}

// An apply whose install holds no rclone config warns, and saves the version its plan said: an
// apply never changes what it planned.
func TestRcloneAtApplyKeepsTheVersion(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: strings.Replace(backupSettingsJSON, `"rclone_config_set":true`, `"rclone_config_set":false`, 1)})
	h.fake.set("restore", reply{})
	a := h.apply(nil, h.plan(nil, h.madeAgain(nil)).PlannedState, h.madeAgain(nil))
	if errorsOf(a.Diagnostics) != "" || !strings.Contains(diagText(a.Diagnostics), "The install holds no rclone config") ||
		h.attrs(a.NewState)["backup_rclone_config_wo_version"].IsNull() {
		t.Fatalf("%s %v", diagText(a.Diagnostics), h.attrs(a.NewState)["backup_rclone_config_wo_version"])
	}
}

// Refusals at plan and validate: an empty export with a restore, the rclone config without its
// version, an object that could read as a flag.
func TestBackupRefusalsAtPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str("")})).Diagnostics); !strings.Contains(d, "secrets_wo is empty") {
		t.Errorf("an empty export: %q", d)
	}
	// Blank, refused whatever a restore; unknown at plan, refused with a restore.
	if d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str(" \n"), "restore": tftypes.NewValue(h.typ.AttributeTypes["restore"], nil), "restore_identity_wo": tftypes.NewValue(tftypes.String, nil)})).Diagnostics); !strings.Contains(d, "secrets_wo is empty") {
		t.Errorf("a blank file without a restore: %q", d)
	}
	if d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})).Diagnostics); !strings.Contains(d, "secrets_wo is not known at plan") {
		t.Errorf("unknown with a restore: %q", d)
	}
	// Without a restore, the file is the installer's to judge: nothing refused at plan.
	for _, file := range []string{"project: notes\nsecrets: {}\nprovision:\n  ADMIN_KEY: x\n", "project: notes\nsecrets: {}\n"} {
		d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str(file), "restore": tftypes.NewValue(h.typ.AttributeTypes["restore"], nil), "restore_identity_wo": tftypes.NewValue(tftypes.String, nil)})).Diagnostics)
		if d != "" {
			t.Errorf("no restore, %q: %q", file, d)
		}
	}
	if d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str(" \n")})).Diagnostics); !strings.Contains(d, "secrets_wo is empty") || strings.Contains(d, "no secrets export") {
		t.Errorf("a blank export: %q", d)
	}
	for _, file := range []string{"just-text", "~", "- a\n"} {
		if d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str(file)})).Diagnostics); d != "" {
			t.Errorf("not a mapping %q: %q", file, d)
		}
	}
	// A seeded secrets file (no export: true) with a restore; the export itself plans clean.
	for file, want := range map[string]bool{
		"project: notes\nsecrets:\n  SESSION_SECRET: x\n":               true,
		"project: notes\nexport: false\nsecrets: {}\n":                  true,
		"project: notes\nexport: true\nsecrets:\n  SESSION_SECRET: x\n": false,
	} {
		d := errorsOf(h.plan(nil, h.madeAgain(map[string]tftypes.Value{"secrets_wo": str(file)})).Diagnostics)
		if strings.Contains(d, "secrets_wo is no secrets export") != want || strings.Contains(d, "SESSION_SECRET") {
			t.Errorf("%q: %q", file, d)
		}
	}
	if d := errorsOf(h.validate(h.madeAgain(map[string]tftypes.Value{"backup_rclone_config_wo_version": tftypes.NewValue(tftypes.Number, nil)}))); !strings.Contains(d, "backup_rclone_config_wo_version is required") {
		t.Errorf("the rclone config without its version: %q", d)
	}
	for _, o := range []string{"-x", "--input=/x", "a b", ""} {
		if d := errorsOf(h.validate(h.madeAgain(map[string]tftypes.Value{"restore": h.restoreValue(o)}))); !strings.Contains(d, "backup's name") {
			t.Errorf("object %q: %q", o, d)
		}
	}
}

// An update whose backup config fails keeps the settings state had, so the next plan shows the
// change again; a pending restore taken out of the configuration reads nothing.
func TestBackupUpdateThatFails(t *testing.T) {
	t.Parallel()
	h := madeAgainHarness(t)
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	h.fake.set("restore", reply{})
	state := h.create(h.madeAgain(nil))
	h.fake.set("backup", reply{exit: 3, stderr: "error: the schedule is no calendar event\n"})
	changed := h.madeAgain(map[string]tftypes.Value{"backup": h.backupValue(map[string]tftypes.Value{
		"destination": str("dav:backups"), "recipients": strList("age1ops"), "schedule": str("never")})})
	u := h.update(state, changed)
	if errorsOf(u.Diagnostics) == "" || asString(t, h.backupAttr(t, u.NewState, "schedule")) != "daily" {
		t.Fatalf("a refused change saved: %s %v", errorsOf(u.Diagnostics), h.attrs(u.NewState)["backup"])
	}
	// A restore left pending, then taken out: nothing read, nothing restored, nothing pending.
	h2 := madeAgainHarness(t)
	h2.fake.set("backup", reply{stdout: backupSettingsJSON})
	h2.fake.set("restore", reply{exit: 1, stderr: "cut\n"})
	a := h2.apply(nil, h2.plan(nil, h2.madeAgain(nil)).PlannedState, h2.madeAgain(nil))
	h2.fake.reset()
	h2.fake.set("status", reply{exit: 1, stderr: "unreachable\n"})
	null := tftypes.NewValue(tftypes.String, nil)
	u = h2.update(a.NewState, h2.madeAgain(map[string]tftypes.Value{"restore": tftypes.NewValue(h2.typ.AttributeTypes["restore"], nil), "restore_identity_wo": null}))
	if errorsOf(u.Diagnostics) != "" || len(h2.fake.callsOf("status")) != 0 || bytes.Contains(h2.privateOf(u.NewState), []byte(backupPendingKey)) {
		t.Fatalf("a removed restore: %s %q", errorsOf(u.Diagnostics), h2.fake.ops())
	}
}

// The restore's gate: a VM install of its own is refused before anything runs; one that awaits
// its data is not given the export again, and restored (a boot that failed before it wrote an
// install: TestVMBootThatFailedTakesTheExport); an update's settings run before the restore's
// check.
func TestRestoreGate(t *testing.T) {
	t.Parallel()
	vmConfig := func(h *harness) tftypes.Value {
		return h.vmConfig("1.1.0", map[string]tftypes.Value{
			"secrets_wo":          str(exportFile),
			"backup":              h.backupValue(map[string]tftypes.Value{"keep_local": num(2), "recipients": strList("age1ops")}),
			"restore":             h.restoreValue("notes-full-x.pgdump.age"),
			"restore_identity_wo": str("AGE-SECRET-KEY-" + secretVal),
		})
	}
	keptJSON := strings.Replace(backupSettingsJSON, `"destination":"dav:backups"`, `"keep_local":2`, 1)
	// Installed, not awaiting: refused, nothing run.
	h, f := newVMHarness(t)
	f.set("backup", reply{stdout: keptJSON})
	f.set("restore", reply{})
	a := h.apply(nil, h.plan(nil, vmConfig(h)).PlannedState, vmConfig(h))
	if len(f.callsOf("restore")) != 0 || len(f.callsOf("install")) != 0 || !strings.Contains(errorsOf(a.Diagnostics), "The VM holds an install of its own") ||
		!strings.Contains(errorsOf(a.Diagnostics), "`sudo notes-stow restore notes-full-x.pgdump.age --unrecorded --yes --identity -` over SSH (the identity on standard input) loads it") {
		t.Fatalf("a VM's own install: %s", diagText(a.Diagnostics))
	}
	// Installed by an earlier apply, awaiting its data: the export not given again, the restore run.
	h, f = newVMHarness(t)
	awaiting := vmStatusJSON("1.1.0", "boot-1", `,"restore_awaited":true,"made_again":true`)
	f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: awaiting})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"restore_awaited":true`)})
	f.set("backup", reply{stdout: keptJSON})
	f.set("restore", reply{})
	a = h.apply(nil, h.plan(nil, vmConfig(h)).PlannedState, vmConfig(h))
	for _, in := range f.callsOf("install") {
		if in.stdin != nil && untarAll(t, in.stdin)["secrets"] != "" {
			t.Fatal("the export was given to an install that took it")
		}
	}
	if errorsOf(a.Diagnostics) != "" || len(f.callsOf("restore")) != 1 {
		t.Fatalf("an unfinished install awaiting its data: restores %d, %s", len(f.callsOf("restore")), diagText(a.Diagnostics))
	}
	// An update: the settings changed are set even when the status before the restore fails.
	h2 := madeAgainHarness(t)
	h2.fake.set("backup", reply{stdout: backupSettingsJSON})
	h2.fake.set("restore", reply{exit: 1, stderr: "cut\n"})
	created := h2.apply(nil, h2.plan(nil, h2.madeAgain(nil)).PlannedState, h2.madeAgain(nil)).NewState
	h2.fake.reset()
	h2.fake.set("status", reply{exit: 1, stderr: "unreachable\n"})
	changed := h2.madeAgain(map[string]tftypes.Value{"backup": h2.backupValue(map[string]tftypes.Value{
		"destination": str("dav:backups"), "recipients": strList("age1ops"), "schedule": str("weekly")})})
	u := h2.update(created, changed)
	if len(h2.fake.callsOf("backup")) != 1 || asString(t, h2.backupAttr(t, u.NewState, "schedule")) != "weekly" ||
		!strings.Contains(errorsOf(u.Diagnostics), "Reading the install before the restore") || !bytes.Contains(h2.privateOf(u.NewState), []byte(backupPendingKey)) {
		t.Fatalf("settings then gate: %s %q", diagText(u.Diagnostics), h2.fake.ops())
	}
}

// An apply saves its plan: the configuration's own values as planned, null included; a computed
// one the plan did not know, the install's.
func TestPlannedOver(t *testing.T) {
	t.Parallel()
	planned := types.ObjectValueMust(backupAttrTypes, map[string]attr.Value{
		"destination": types.StringNull(), "keep_local": types.Int64Value(2), "recipients": types.ListValueMust(types.StringType, []attr.Value{types.StringValue("age1a")}),
		"retention_days": types.Int64Unknown(), "schedule": types.StringValue("weekly"), "profile": types.StringUnknown(), "locked": types.BoolNull(),
	})
	got := backupValue(&installer.BackupSettings{Configured: true, Destination: "dav:b", Recipients: []string{"age1b"}, RetentionDays: 9, Schedule: "daily", Profile: "full", Locked: true})
	var d diag.Diagnostics
	out := plannedOver(context.Background(), planned, got, &d).Attributes()
	if !out["destination"].IsNull() || !out["keep_local"].Equal(types.Int64Value(2)) || !out["schedule"].Equal(types.StringValue("weekly")) ||
		!out["retention_days"].Equal(types.Int64Value(9)) || !out["profile"].Equal(types.StringValue("full")) || !out["locked"].Equal(types.BoolValue(true)) ||
		!out["recipients"].Equal(planned.Attributes()["recipients"]) || d.HasError() {
		t.Fatalf("%v %v", out, d)
	}
}

// madeAgainHarness is a Docker harness whose install, once made, awaits its data: what a
// --secrets install of a release running backups reports from Stowage 0.48.0.
func madeAgainHarness(t *testing.T) *harness {
	h := newHarness(t, nil)
	h.fake.set("status", reply{stdout: awaitingStatus(h)})
	return h
}

// A VM whose boot failed before it wrote an install (installed null, install_failed) is given the
// export: its first install is still to make.
func TestVMBootThatFailedTakesTheExport(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	failed := strings.Replace(vmStatusJSON("1.1.0", "boot-1", `,"install_failed":"the boot's install failed before it wrote the install"`),
		`"installed":{"version":"1.1.0","revision":1}`, `"installed":null`, 1)
	f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: failed})
	f.set("status", reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"restore_awaited":true`)})
	f.set("backup", reply{stdout: backupSettingsJSON})
	f.set("restore", reply{})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{
		"secrets_wo":          str(exportFile),
		"backup":              h.backupValue(map[string]tftypes.Value{"destination": str("dav:backups"), "recipients": strList("age1ops")}),
		"restore":             h.restoreValue("notes-full-x.pgdump.age"),
		"restore_identity_wo": str("AGE-SECRET-KEY-" + secretVal),
	})
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	given := false
	for _, in := range f.callsOf("install") {
		given = given || in.stdin != nil && untarAll(t, in.stdin)["secrets"] == exportFile
	}
	if errorsOf(a.Diagnostics) != "" || !given || len(f.callsOf("restore")) != 1 {
		t.Fatalf("given %v, restores %d: %s", given, len(f.callsOf("restore")), diagText(a.Diagnostics))
	}
}

// Both destination and keep_local, known only at apply: refused before anything runs, pending.
func TestBothTargetsAtApply(t *testing.T) {
	t.Parallel()
	both := types.ObjectValueMust(backupAttrTypes, map[string]attr.Value{
		"destination": types.StringValue("dav:b"), "keep_local": types.Int64Value(2), "recipients": types.ListValueMust(types.StringType, nil),
		"retention_days": types.Int64Null(), "schedule": types.StringNull(), "profile": types.StringNull(), "locked": types.BoolNull(),
	})
	m := installModel{Backup: both, Restore: types.ObjectNull(map[string]attr.Type{"object": types.StringType, "unrecorded": types.BoolType}),
		Docker: &dockerModel{}, ShortName: types.StringValue("notes")}
	var failed []string
	var diags diag.Diagnostics
	p := &memPrivate{}
	(&installResource{}).backupSteps(context.Background(), &m, backupPending{Config: true}, p, func(s, d string) { failed = append(failed, d) },
		func() (bool, error) { return true, nil }, &diags)
	if len(failed) != 1 || !strings.Contains(failed[0], "both destination and keep_local") || !bytes.Contains(p.data[backupPendingKey], []byte("config")) {
		t.Fatalf("failed %q, pending %q", failed, p.data[backupPendingKey])
	}
}

// A VM create applied again after its first install took the export (made_again, or
// restore_awaited from an installer before 0.48.0) and failed does not give it again; one still
// running (installed, installing) is told to wait; one a cut restore left half restored is
// refused, naming the restore that finishes it; nothing runs on either.
func TestVMCreateAgainAfterAFailedInstall(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ extra, want string }{
		// A failed first install is marked installing too: the failure says it ended.
		{`,"installing":true,"install_failed":"the stack did not come up","made_again":true`, "The VM's install took the secrets export"},
		{`,"installing":true,"made_again":true`, "The first install is still running"}, // its .env written: installed
		{`,"installing":true`, "The first install is still running"},
		// An installer before Stowage 0.48.0 says no made_again: restore_awaited does.
		{`,"restore_awaited":true`, "The VM's install took the secrets export"},
		{`,"made_again":true,"restore_awaited":true,"volumes_split":true`, "A restore left the VM's volumes half restored"},
	} {
		h, f := newVMHarness(t)
		f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", c.extra)})
		config := h.vmConfig("1.1.0", map[string]tftypes.Value{"secrets_wo": str(exportFile)})
		a := h.apply(nil, h.plan(nil, config).PlannedState, config)
		for _, in := range f.callsOf("install") {
			if in.stdin != nil && untarAll(t, in.stdin)["secrets"] != "" {
				t.Fatalf("%s: the export was given again", c.want)
			}
		}
		if !strings.Contains(diagText(a.Diagnostics), c.want) {
			t.Fatalf("%s: %s", c.want, diagText(a.Diagnostics))
		}
		if c.want == "The first install is still running" && (len(f.callsOf("install")) != 0 || !strings.Contains(errorsOf(a.Diagnostics), "`sudo notes-stow install`")) {
			t.Fatalf("still running: %q, %s", f.ops(), errorsOf(a.Diagnostics))
		}
		if c.want == "A restore left the VM's volumes half restored" && (len(f.callsOf("install")) != 0 || len(f.callsOf("restore")) != 0 ||
			!strings.Contains(errorsOf(a.Diagnostics), "`sudo notes-stow restore OBJECT --unrecorded --yes --identity -` over SSH (the identity on standard input), OBJECT the cut restore's")) {
			t.Fatalf("half restored: %q, %s", f.ops(), errorsOf(a.Diagnostics))
		}
	}
}

// The half-restored refusal names the configured backup.
func TestVMCreateRefusalsNameTheWayOn(t *testing.T) {
	t.Parallel()
	h, f := newVMHarness(t)
	f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: vmStatusJSON("1.1.0", "boot-1", `,"made_again":true,"restore_awaited":true,"volumes_split":true`)})
	config := h.vmConfig("1.1.0", map[string]tftypes.Value{
		"secrets_wo": str(exportFile), "restore": h.restoreValue("b1"), "restore_identity_wo": str("AGE-SECRET-KEY-1"),
		"backup": h.backupValue(map[string]tftypes.Value{"keep_local": num(2), "recipients": strList("age1ops")}),
	})
	if got := errorsOf(h.apply(nil, h.plan(nil, config).PlannedState, config).Diagnostics); !strings.Contains(got, "`sudo notes-stow restore b1 --unrecorded --yes --identity -`") {
		t.Fatalf("half restored: %s", got)
	}
}

// The ways by hand the warnings name: the full command per target, as the configuration says.
func TestRestoreByHand(t *testing.T) {
	t.Parallel()
	rs := &restoreModel{Object: types.StringValue("b1"), Unrecorded: types.BoolValue(true)}
	vm := &installModel{VM: &vmModel{ShortName: types.StringValue("notes")}}
	if got := restoreByHand(vm, rs, byHandAny); !strings.Contains(got, "`sudo notes-stow restore b1 --unrecorded --yes --identity -` over SSH") {
		t.Errorf("vm: %s", got)
	}
	rs.Unrecorded = types.BoolValue(false)
	docker := &installModel{Docker: &dockerModel{}, ID: types.StringValue("/srv/notes"), ShortName: types.StringValue("notes")}
	if got := restoreByHand(docker, rs, byHandAny); !strings.Contains(got, "`/srv/notes/notes-stow restore b1 --yes --identity - --dir /srv/notes` on the host") {
		t.Errorf("docker: %s", got)
	}
	if got := restoreByHand(docker, nil, byHandAny); !strings.Contains(got, "restore OBJECT --unrecorded --yes") || !strings.Contains(got, "`/srv/notes/notes-stow backup list --dir /srv/notes` names") {
		t.Errorf("docker, no restore: %s", got)
	}
	// Finishing a cut restore into an install made again: the object it named, always
	// --unrecorded (the install holds no records of the old machine's backups).
	vm.Installer = types.StringValue(image)
	if got := restoreByHand(vm, nil, byHandCut); !strings.Contains(got, "`sudo notes-stow restore OBJECT --unrecorded --yes --identity -`") ||
		!strings.Contains(got, "OBJECT the cut restore's (the first line of /usr/local/notes/.stowage-volumes-split on the target)") || strings.Contains(got, "backup list") {
		t.Errorf("split, no restore: %s", got)
	}
	if got := restoreByHand(docker, nil, byHandCut); !strings.Contains(got, "the first line of /srv/notes/.stowage-volumes-split") {
		t.Errorf("docker split, no restore: %s", got)
	}
	// rs says no --unrecorded: an install made again still needs it.
	for _, how := range []byHand{byHandCut, byHandMadeAgain} {
		if got := restoreByHand(vm, rs, how); !strings.Contains(got, "restore b1 --unrecorded --yes") || strings.Contains(got, "OBJECT") {
			t.Errorf("%d, a restore: %s", how, got)
		}
	}
}

// A first install with no .env written (installed null: the boot's install failed before it, or
// a create's run failed before it and took its records away) is given the export again: the
// installer takes it then.
func TestVMFailedBeforeItsEnvTakesTheExportAgain(t *testing.T) {
	t.Parallel()
	for _, extra := range []string{`,"install_failed":"the boot's install failed before it wrote the install"`, ``} {
		h, f := newVMHarness(t)
		unwritten := strings.Replace(vmStatusJSON("1.1.0", "boot-1", extra), `"installed":{"version":"1.1.0","revision":1}`, `"installed":null`, 1)
		f.then("status", reply{stdout: awaitingSecrets()}, reply{stdout: unwritten})
		config := h.vmConfig("1.1.0", map[string]tftypes.Value{"secrets_wo": str(exportFile)})
		a := h.apply(nil, h.plan(nil, config).PlannedState, config)
		given := false
		for _, in := range f.callsOf("install") {
			given = given || in.stdin != nil && untarAll(t, in.stdin)["secrets"] == exportFile
		}
		if !given || errorsOf(a.Diagnostics) != "" || strings.Contains(diagText(a.Diagnostics), "The VM's install took the secrets export") {
			t.Fatalf("%q: given %v: %s", extra, given, diagText(a.Diagnostics))
		}
	}
}

// The by-hand restore quotes a path or name that is not one shell word, and leaves out what is
// not known yet.
func TestRestoreByHandQuotesAndUnknowns(t *testing.T) {
	t.Parallel()
	docker := &installModel{Docker: &dockerModel{}, ID: types.StringValue("/srv/my apps/notes"), ShortName: types.StringValue("notes")}
	rs := &restoreModel{Object: types.StringValue("b1"), Unrecorded: types.BoolValue(false)}
	if got := restoreByHand(docker, rs, byHandAny); !strings.Contains(got, "`'/srv/my apps/notes/notes-stow' restore b1 --yes --identity - --dir '/srv/my apps/notes'`") {
		t.Errorf("quoted: %s", got)
	}
	if got := restoreByHand(docker, &restoreModel{Object: types.StringUnknown(), Unrecorded: types.BoolUnknown()}, byHandAny); !strings.Contains(got, "restore OBJECT --unrecorded") || !strings.Contains(got, "backup list") {
		t.Errorf("unknown: %s", got)
	}
	if got := restoreByHand(docker, &restoreModel{Object: types.StringValue("b1"), Unrecorded: types.BoolUnknown()}, byHandAny); !strings.Contains(got, "restore b1 [--unrecorded] --yes") {
		t.Errorf("unrecorded unknown: %s", got)
	}
}

// Backup settings added to an install made without them: the settings the configuration leaves
// out are planned unknown, so the apply saves the install's.
func TestBackupAddedByAnUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	state := h.create(h.dockerConfig(nil))
	h.fake.set("backup", reply{stdout: backupSettingsJSON})
	config := h.dockerConfig(map[string]tftypes.Value{"backup": h.backupValue(map[string]tftypes.Value{
		"destination": str("dav:backups"), "recipients": strList("age1ops")})})
	p := h.plan(state, config)
	if d := errorsOf(p.Diagnostics); d != "" {
		t.Fatal(d)
	}
	for _, k := range []string{"retention_days", "schedule", "profile", "locked"} {
		if h.backupAttr(t, p.PlannedState, k).IsKnown() {
			t.Errorf("%s planned known: %v", k, h.backupAttr(t, p.PlannedState, k))
		}
	}
	a := h.apply(state, p.PlannedState, config)
	if d := errorsOf(a.Diagnostics); d != "" || !h.backupAttr(t, a.NewState, "retention_days").Equal(num(30)) {
		t.Fatalf("%s %v", d, h.backupAttr(t, a.NewState, "retention_days"))
	}
}
