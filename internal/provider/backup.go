package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// A machine made again in one apply, on Docker and the VM: `backup` declares the install's backup
// settings (`backup config`), `restore` loads one backup into the install a create makes from a
// `secrets export` (`secrets_wo`). The provider runs no backup: the install's schedule does, and
// an upgrade takes one first.
//
// Invariant: a restore runs only while the install awaits its data — `status --json`
// restore_awaited (an install made by --secrets and not restored yet, whose release runs backups
// or whose hook took the export's provision values) or volumes_split, read right before the restore — so it never loads over an install's
// own data. A create's restore that fails is retried by the next apply under the same check
// (backupPendingKey), never run again once it succeeded.

var backupAttrTypes = map[string]attr.Type{
	"destination":    types.StringType,
	"keep_local":     types.Int64Type,
	"recipients":     types.ListType{ElemType: types.StringType},
	"retention_days": types.Int64Type,
	"schedule":       types.StringType,
	"profile":        types.StringType,
	"locked":         types.BoolType,
}

// backupComputed are backup's attributes the install gives when the configuration leaves them out.
var backupComputed = map[string]bool{"retention_days": true, "schedule": true, "profile": true, "locked": true}

type backupModel struct {
	Destination   types.String `tfsdk:"destination"`
	KeepLocal     types.Int64  `tfsdk:"keep_local"`
	Recipients    types.List   `tfsdk:"recipients"`
	RetentionDays types.Int64  `tfsdk:"retention_days"`
	Schedule      types.String `tfsdk:"schedule"`
	Profile       types.String `tfsdk:"profile"`
	Locked        types.Bool   `tfsdk:"locked"`
}

type restoreModel struct {
	Object     types.String `tfsdk:"object"`
	Unrecorded types.Bool   `tfsdk:"unrecorded"`
}

// backupAttributes are the model's attributes of this file, for installSchema.
func backupAttributes() map[string]schema.Attribute {
	keepString := []planmodifier.String{stringplanmodifier.UseStateForUnknown()}
	return map[string]schema.Attribute{
		"backup": schema.SingleNestedAttribute{
			Optional: true,
			MarkdownDescription: "The install's backup settings (`backup config`; Docker and the VM): set at create after the install, and again by an update whenever they change (`backup config … --json`, the flags given, the rclone config a file on standard input). " +
				"A refresh reads them back (`backup config --json`), so a change made on the target shows in the next plan. Left out, the provider leaves the install's settings as they are; taken out, it stops managing them and changes nothing on the target. " +
				"The provider runs no backup: the install's schedule does, and an upgrade takes one first.",
			Attributes: map[string]schema.Attribute{
				"destination": schema.StringAttribute{
					Optional:            true,
					MarkdownDescription: "Where backups go: a remote of the rclone config and a path, `remote:path` (`--destination`). Give it or `keep_local`.",
				},
				"keep_local": schema.Int64Attribute{
					Optional: true,
					// 0 is the install's "none": state could not hold it (the installer caps the top).
					Validators:          []validator.Int64{int64validator.AtLeast(1)},
					MarkdownDescription: "Keep the profile's N latest backups on the host rather than send them to a destination (`--keep-local`, which drops the destination and its rclone config): a tool reaching the host over SSH pulls them (`backup export`). Give it or `destination`. Needs Stowage 0.47.0 or later.",
				},
				"recipients": schema.ListAttribute{
					Required:            true,
					ElementType:         types.StringType,
					MarkdownDescription: "The age public keys (`age1…`) backups are encrypted to (`--recipient`, the list replaced): any one's private key decrypts them, and none is on the target.",
				},
				"retention_days": schema.Int64Attribute{
					Optional: true, Computed: true,
					MarkdownDescription: "Delete a profile's backups older than this after each backup; 0 keeps them all (`--retention-days`). Left out, the install's.",
					PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
				},
				"schedule": schema.StringAttribute{
					Optional: true, Computed: true,
					MarkdownDescription: "When the scheduled backup runs, a systemd calendar event (`--schedule`). Left out, the install's.",
					PlanModifiers:       keepString,
				},
				"profile": schema.StringAttribute{
					Optional: true, Computed: true,
					MarkdownDescription: "The profile a scheduled backup takes (`--profile`). Left out, the install's (the release's default profile unless set).",
					PlanModifiers:       keepString,
				},
				"locked": schema.BoolAttribute{
					Optional: true, Computed: true,
					MarkdownDescription: "The app socket may not change the settings (`--lock`, `--unlock`); the operator still does. Left out, the install's.",
					PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
				},
			},
		},
		"backup_rclone_config_wo": schema.StringAttribute{
			Optional: true, WriteOnly: true,
			MarkdownDescription: "The rclone config defining `backup.destination`'s remote and its credentials, write-only (`--rclone-config`, a file on standard input). Sent with every `backup config` the provider runs while `backup.destination` is set. A refresh that reads the install holding none (or one it does not run, saved through the app socket) takes `backup_rclone_config_wo_version` out of state, so the next plan sends it again.",
		},
		"backup_rclone_config_wo_version": schema.Int64Attribute{
			Optional:            true,
			MarkdownDescription: "Bump it to send `backup_rclone_config_wo` again when only the rclone config changed.",
		},
		"restore": schema.SingleNestedAttribute{
			Optional: true,
			MarkdownDescription: "A backup loaded into the install a create makes from a `secrets export` (`secrets_wo`): a machine made again. After the install and `backup`'s settings, the create runs `restore OBJECT --yes [--unrecorded]` (the identity a file on standard input), which loads the backup and runs the provision hook on it. " +
				"It loads only while the install awaits its data (`status --json` `restore_awaited` — from Stowage 0.48.0 every install made by `--secrets` whose release runs backups or whose hook is given the export's provision values, before only the latter — or `volumes_split`), read right before it: else it is dropped with a warning (`Not restored: the install does not await its data`), never loaded over an install's own data. " +
				"Taken at that create alone: a restore that fails leaves the install created, with a warning, and the next apply runs it again under the same check (fix `object` or the identity in between); once it succeeded, a change is recorded and nothing runs. Refused at plan without `secrets_wo` (or with an empty one) and `restore_identity_wo` in the create.",
			Attributes: map[string]schema.Attribute{
				"object": schema.StringAttribute{
					Required:            true,
					Validators:          []validator.String{objectValidator{}},
					MarkdownDescription: "The backup to load: its name in the destination (`backup list`), taken from the release the install runs.",
				},
				"unrecorded": schema.BoolAttribute{
					Optional: true, Computed: true, Default: booldefault.StaticBool(false),
					MarkdownDescription: "Load a backup this install recorded no digest of (`--unrecorded`): the records of a machine made again went with the old one. A backup whose digest differs from its record is refused whatever this says.",
				},
			},
		},
		"restore_identity_wo": schema.StringAttribute{
			Optional: true, WriteOnly: true,
			MarkdownDescription: "The age identity (`AGE-SECRET-KEY-…`) that decrypts `restore.object`, write-only (`--identity`, a file on standard input): used for the restore, never kept.",
		},
	}
}

// backupOf is m's backup settings; nil when left out or not known.
func backupOf(ctx context.Context, o types.Object, diags *diag.Diagnostics) *backupModel {
	if !known(o) {
		return nil
	}
	var b backupModel
	diags.Append(o.As(ctx, &b, basetypes.ObjectAsOptions{})...)
	return &b
}

// restoreOf is m's restore; nil when left out or not known.
func restoreOf(ctx context.Context, o types.Object, diags *diag.Diagnostics) *restoreModel {
	if !known(o) {
		return nil
	}
	var r restoreModel
	diags.Append(o.As(ctx, &r, basetypes.ObjectAsOptions{})...)
	return &r
}

// backupConfig is the `backup config` the settings b give: each value known, the rclone config
// with a destination.
func backupConfig(ctx context.Context, b *backupModel, rclone string, diags *diag.Diagnostics) installer.BackupConfig {
	var c installer.BackupConfig
	if known(b.Destination) {
		d := b.Destination.ValueString()
		c.Destination = &d
		c.RcloneConfig = rclone
	}
	if known(b.KeepLocal) {
		n := int(b.KeepLocal.ValueInt64())
		c.KeepLocal = &n
	}
	if known(b.Recipients) {
		diags.Append(b.Recipients.ElementsAs(ctx, &c.Recipients, false)...)
	}
	if known(b.RetentionDays) {
		n := int(b.RetentionDays.ValueInt64())
		c.RetentionDays = &n
	}
	if known(b.Schedule) {
		s := b.Schedule.ValueString()
		c.Schedule = &s
	}
	if known(b.Profile) {
		s := b.Profile.ValueString()
		c.Profile = &s
	}
	if known(b.Locked) {
		l := b.Locked.ValueBool()
		c.Lock = &l
	}
	return c
}

// backupValue is the settings s as backup's value: a destination or keep_local left out when the
// install has none, so settings removed on the target show as a change.
func backupValue(s *installer.BackupSettings) types.Object {
	recips := make([]attr.Value, 0, len(s.Recipients))
	for _, r := range s.Recipients {
		recips = append(recips, types.StringValue(r))
	}
	dest, keep := types.StringNull(), types.Int64Null()
	if s.Configured && s.Destination != "" {
		dest = types.StringValue(s.Destination)
	}
	if s.Configured && s.KeepLocal > 0 {
		keep = types.Int64Value(int64(s.KeepLocal))
	}
	return types.ObjectValueMust(backupAttrTypes, map[string]attr.Value{
		"destination":    dest,
		"keep_local":     keep,
		"recipients":     types.ListValueMust(types.StringType, recips),
		"retention_days": types.Int64Value(int64(s.RetentionDays)),
		"schedule":       types.StringValue(s.Schedule),
		"profile":        types.StringValue(s.Profile),
		"locked":         types.BoolValue(s.Locked),
	})
}

// validateBackup checks backup and restore against the rest of the configuration, as far as the
// provider's own use of them goes (the installer refuses the rest): a destination or keep_local,
// the rclone config with a destination, its version with it, the identity with a restore. A
// value not known yet is not checked.
func validateBackup(ctx context.Context, m *installModel, diags *diag.Diagnostics) {
	if m.Kubernetes != nil {
		return // the kubernetes type has none of them
	}
	if b := backupOf(ctx, m.Backup, diags); b != nil {
		p := path.Root("backup")
		switch {
		case b.Destination.IsNull() && b.KeepLocal.IsNull():
			diags.AddAttributeError(p, "backup needs a destination or keep_local",
				"Give destination (`remote:path` of the rclone config, with backup_rclone_config_wo) or keep_local (the backups kept on the host).")
		case known(b.Destination) && known(b.KeepLocal):
			// State holds the one the install keeps: both cannot be what it holds.
			diags.AddAttributeError(p.AtName("keep_local"), "destination and keep_local",
				"keep_local keeps the backups on the host and drops the destination: give one of them.")
		case known(b.KeepLocal) && !m.BackupRcloneConfigWO.IsNull():
			diags.AddAttributeError(path.Root("backup_rclone_config_wo"), "backup_rclone_config_wo with keep_local",
				"Backups kept on the host need no rclone config: remove backup_rclone_config_wo (and its version).")
		}
	} else if m.Backup.IsNull() && (!m.BackupRcloneConfigWO.IsNull() || !m.BackupRcloneConfigWOVersion.IsNull()) {
		diags.AddAttributeError(path.Root("backup_rclone_config_wo"), "backup_rclone_config_wo without backup",
			"The rclone config is backup.destination's: give the backup settings, or remove backup_rclone_config_wo and its version.")
	}
	if !m.BackupRcloneConfigWO.IsNull() && m.BackupRcloneConfigWOVersion.IsNull() {
		diags.AddAttributeError(path.Root("backup_rclone_config_wo_version"), "backup_rclone_config_wo_version is required",
			"A write-only value is never compared: the version is what sends the rclone config again, when it changes or when a refresh reads the install holding none. Give backup_rclone_config_wo_version = 1, and bump it with each new rclone config.")
	}
	if known(m.BackupRcloneConfigWOVersion) && m.BackupRcloneConfigWO.IsNull() {
		diags.AddAttributeError(path.Root("backup_rclone_config_wo_version"), "backup_rclone_config_wo_version without backup_rclone_config_wo",
			"The version says when the rclone config is sent again, and none is given: give backup_rclone_config_wo with it, or remove the version.")
	}
	if m.Restore.IsNull() && !m.RestoreIdentityWO.IsNull() {
		diags.AddAttributeError(path.Root("restore_identity_wo"), "restore_identity_wo without restore",
			"The identity decrypts restore.object: give restore, or remove restore_identity_wo.")
	}
}

// planBackupCreate refuses, at a create's plan, a restore not made from a secrets export or given
// no identity: a restore loads into the install that create makes, and only one made again from
// an export (export: true, an installer built with Stowage 0.49.0 or later) awaits it. It runs
// after the backup settings are set: a plain install would send its backups to the old
// destination, the restore dropped.
func planBackupCreate(m *installModel, diags *diag.Diagnostics) {
	if known(m.SecretsWO) && strings.TrimSpace(m.SecretsWO.ValueString()) == "" {
		diags.AddAttributeError(path.Root("secrets_wo"), "secrets_wo is empty",
			"secrets_wo is a decrypted `secrets export`, or a new install's secrets file: give it, or remove secrets_wo (an empty file is an export that failed).")
	}
	if m.Restore.IsNull() {
		return
	}
	if !m.SecretsWO.IsNull() && !known(m.SecretsWO) {
		diags.AddAttributeError(path.Root("secrets_wo"), "secrets_wo is not known at plan",
			"With restore, the plan reads secrets_wo to tell a `secrets export` (export: true), which a restore follows, from a new install's secrets file, whose backups the restore's settings would send to the old destination: give a value known at plan (not one from a resource made in the same apply).")
	}
	isSeeded := known(m.SecretsWO) && isSeededFile(m.SecretsWO.ValueString())
	if isSeeded {
		diags.AddAttributeError(path.Root("secrets_wo"), "secrets_wo is no secrets export",
			"restore needs a release whose installer is built with Stowage 0.49.0 or later, and an install made again from a `secrets export`, which says `export: true`. A secrets file without it seeds a new install, which awaits no data: give the export, or remove restore and restore_identity_wo. An export an installer built with Stowage 0.48.0 or earlier wrote lacks the mark: add the line `export: true` to the decrypted file.")
	}
	if m.SecretsWO.IsNull() {
		diags.AddAttributeError(path.Root("restore"), "restore without secrets_wo",
			"A restore loads into the install this create makes, and the provider restores only one made again from a `secrets export` (secrets_wo), which the installer refuses on an install that exists: give secrets_wo (and, on a VM, a seed with await_secrets), or restore by hand.")
	}
	if m.RestoreIdentityWO.IsNull() {
		diags.AddAttributeError(path.Root("restore_identity_wo"), "restore_identity_wo is not given",
			"The restore needs the age identity that decrypts restore.object: give restore_identity_wo.")
	}
}

// isSeededFile reports whether secrets reads as a new install's secrets file: a mapping without
// the `export: true` a `secrets export` writes.
func isSeededFile(secrets string) bool {
	f, ok := secretsFileOf(secrets)
	return ok && !f.Export
}

// secretsFile is what the provider reads of a decrypted secrets file: whether `secrets export`
// wrote it (export: true). The rest is the installer's to judge.
type secretsFile struct {
	Export bool `yaml:"export"`
}

// secretsFileOf reads secrets as a secrets file: ok only when it is one YAML mapping that decodes
// so. Anything else (empty, blank, a scalar, a list, YAML that does not parse) is left to the
// installer, which says why it refuses it.
func secretsFileOf(secrets string) (secretsFile, bool) {
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(secrets), &n); err != nil || n.Kind != yaml.DocumentNode || len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return secretsFile{}, false
	}
	var f secretsFile
	if err := n.Content[0].Decode(&f); err != nil {
		return secretsFile{}, false
	}
	return f, true
}

// planBackupUpdate warns of a restore changed once taken, refuses a pending restore given no
// identity, and says whether a create left a step of this file pending, which the update runs.
func planBackupUpdate(ctx context.Context, p privateData, plan, state *installModel, identity types.String, diags *diag.Diagnostics) bool {
	pending := getBackupPending(ctx, p)
	plan.Backup = backupAdded(plan.Backup, state.Backup, diags)
	if pending.Restore && !plan.Restore.IsNull() && identity.IsNull() {
		diags.AddAttributeError(path.Root("restore_identity_wo"), "restore_identity_wo is not given",
			"The create's restore did not succeed, and this apply runs it again: give restore_identity_wo, the age identity that decrypts restore.object.")
	}
	if !pending.Restore && !plan.Restore.Equal(state.Restore) && !plan.Restore.IsNull() {
		diags.AddAttributeWarning(path.Root("restore"), "restore is taken at create alone",
			"The install was restored (or made without one): the change is recorded, and nothing is restored. A machine made again restores in the create that makes it; "+restoreByHand(plan, restoreOf(ctx, plan.Restore, diags), byHandAny)+" restores an install that exists.")
	}
	return pending.Config || pending.Restore
}

// backupAdded is planned with its computed attributes the configuration leaves out unknown when
// state holds no settings: UseStateForUnknown copies a null from a parent null in state (it
// checks only that the resource has state), and the apply fills them from the install.
func backupAdded(planned, state types.Object, diags *diag.Diagnostics) types.Object {
	if !state.IsNull() || !known(planned) {
		return planned
	}
	out := map[string]attr.Value{}
	for k, v := range planned.Attributes() {
		if backupComputed[k] && v.IsNull() {
			v = unknownOf(backupAttrTypes[k])
		}
		out[k] = v
	}
	o, d := types.ObjectValue(backupAttrTypes, out)
	diags.Append(d...)
	return o
}

// backupChanged says whether an update runs `backup config`: the settings or the rclone config's
// version changed, the settings still given.
func backupChanged(plan, state *installModel) bool {
	if plan.Backup.IsNull() {
		return false
	}
	return !plan.Backup.Equal(state.Backup) ||
		(!plan.BackupRcloneConfigWOVersion.Equal(state.BackupRcloneConfigWOVersion) && !plan.BackupRcloneConfigWOVersion.IsNull())
}

// backupPendingKey is the private state's key of the steps a create could not finish.
const backupPendingKey = "backup_pending"

type backupPending struct {
	Config  bool `json:"config,omitempty"`
	Restore bool `json:"restore,omitempty"`
}

func getBackupPending(ctx context.Context, p privateData) backupPending {
	var b backupPending
	if p == nil || reflect.ValueOf(p).IsNil() {
		return b
	}
	raw, d := p.GetKey(ctx, backupPendingKey)
	if d.HasError() || len(raw) == 0 || json.Unmarshal(raw, &b) != nil {
		return backupPending{}
	}
	return b
}

func setBackupPending(ctx context.Context, p privateData, b backupPending, diags *diag.Diagnostics) {
	if !b.Config && !b.Restore {
		diags.Append(p.SetKey(ctx, backupPendingKey, nil)...)
		return
	}
	raw, _ := json.Marshal(b) // two booleans always marshal
	diags.Append(p.SetKey(ctx, backupPendingKey, raw)...)
}

// backupTarget is a target's backup verbs: Docker's and the VM's.
type backupTarget interface {
	BackupSettings(ctx context.Context) (*installer.BackupSettings, error)
	BackupConfig(ctx context.Context, c installer.BackupConfig) (*installer.BackupSettings, error)
	Restore(ctx context.Context, object, identity string, unrecorded bool) error
}

// backupTargetOf is m's target for those verbs.
func (r *installResource) backupTargetOf(m *installModel) (backupTarget, error) {
	switch targetKind(m) {
	case "docker":
		d, err := r.docker(m, m.ShortName.ValueString())
		return d, err
	case "vm":
		v, err := r.vm(m)
		return v, err
	}
	return nil, fmt.Errorf("the %s target has no backup settings", targetKind(m))
}

// backupSteps runs, on the install m holds, the steps due: `backup config` with the settings m
// gives (m's backup then set from the answer), then the restore, which runs only when mayRestore,
// asked right before it, says so. Each that fails is told by fail, the steps left recorded
// pending in p, so the next plan shows an update that runs them: a create's fail is a warning,
// as an error would taint an install whose replacement is a destroy.
func (r *installResource) backupSteps(ctx context.Context, m *installModel, due backupPending, p privateData, fail func(summary, detail string), mayRestore func() (bool, error), diags *diag.Diagnostics) {
	rs := restoreOf(ctx, m.Restore, diags)
	if due.Restore && rs == nil {
		due.Restore = false // taken out of the configuration: nothing to restore
	}
	left := due
	defer func() { setBackupPending(ctx, p, left, diags) }()
	if due.Restore && r.wo.identity == "" {
		// Refused at plan: reached only by an apply given no identity its plan had.
		fail("The restore did not run", "restore_identity_wo is not given: give it, and the next apply restores "+rs.Object.ValueString()+".")
		due.Restore = false
	}
	if b := backupOf(ctx, m.Backup, diags); due.Config && b != nil && known(b.Destination) && known(b.KeepLocal) {
		// Refused at validate when both were known then: state can hold only one.
		fail("Setting the backup settings failed", "backup gives both destination and keep_local: give one of them. The step stays pending, and runs (with any restore left) at the next apply.")
		return
	}
	if !due.Config && !due.Restore {
		return
	}
	t, err := r.backupTargetOf(m)
	if err != nil {
		fail("The backup settings and the restore did not run", err.Error()+". The install is recorded: the next plan shows an update that runs them.")
		return
	}
	if due.Config {
		b := backupOf(ctx, m.Backup, diags)
		switch {
		case b == nil:
			left.Config = false
		default:
			s, err := t.BackupConfig(ctx, backupConfig(ctx, b, r.wo.rclone, diags))
			if err != nil {
				then := "the next plan shows an update that sets them again"
				if left.Restore {
					then += ", then restores"
				}
				fail("Setting the backup settings failed", installerDetail(err)+"\n\nThe install is recorded: "+then+".")
				return
			}
			left.Config = false
			m.Backup = backupValue(s)
			rcloneHeld(m, s, false, diags)
		}
	}
	if !due.Restore {
		return
	}
	ok, err := mayRestore()
	switch {
	case err != nil:
		fail("Reading the install before the restore", installerDetail(err)+". The restore stays pending: apply again.")
		return
	case !ok:
		left.Restore = false
		diags.AddAttributeWarning(path.Root("restore"), "Not restored: the install does not await its data",
			"status --json reports neither restore_awaited nor volumes_split: an install was there before this create, an earlier restore finished on the target after all, or the install awaits nothing (its release runs no backups, and no provision values reached a hook; before Stowage 0.48.0, only those values mark it). Nothing is loaded over its data; if the backup did not load, "+restoreByHand(m, rs, byHandAny)+" restores it by hand, replacing the data the install holds now.")
		return
	}
	if err := t.Restore(ctx, rs.Object.ValueString(), r.wo.identity, rs.Unrecorded.ValueBool()); err != nil {
		fail("Restoring "+rs.Object.ValueString()+" failed",
			installerDetail(err)+"\n\nThe install is recorded, its restore pending: fix what the installer says (restore.object, restore.unrecorded, restore_identity_wo), and the next apply restores again.")
		return
	}
	left.Restore = false
}

// rcloneHeld handles settings s that name a destination and hold no rclone config the backups
// run with: a refresh (read) takes backup_rclone_config_wo_version out of m, so the next plan
// sends it again; an apply, which saves what its plan said, warns.
func rcloneHeld(m *installModel, s *installer.BackupSettings, read bool, diags *diag.Diagnostics) {
	if !s.Configured || s.Destination == "" || s.RcloneConfigHeld() {
		return
	}
	if read {
		m.BackupRcloneConfigWOVersion = types.Int64Null()
		return
	}
	diags.AddAttributeWarning(path.Root("backup_rclone_config_wo"), "The install holds no rclone config its backups run with",
		"Its backups to "+s.Destination+" fail until one is saved: give backup_rclone_config_wo (not empty) and bump backup_rclone_config_wo_version, then apply.")
}

// readBackup puts the install's backup settings into m on a refresh, when m manages them; a read
// that fails is a warning (the settings are left as state has them).
func (r *installResource) readBackup(ctx context.Context, m *installModel, diags *diag.Diagnostics) {
	if m.Backup.IsNull() || targetKind(m) == "kubernetes" {
		return
	}
	t, err := r.backupTargetOf(m)
	if err == nil {
		var s *installer.BackupSettings
		if s, err = t.BackupSettings(ctx); err == nil {
			m.Backup = backupValue(s)
			rcloneHeld(m, s, true, diags)
			return
		}
	}
	diags.AddWarning("Reading the backup settings failed", installerDetail(err)+". They are left as state has them.")
}

// installerDetail is err as a diagnostic's detail: a refusal's reason and fix, or the failure.
func installerDetail(err error) string {
	var refused *installer.RefusedError
	if errors.As(err, &refused) {
		detail := "The installer refused it (" + refused.Verb + ", exit 3), before anything changed on the target: " + refused.Reason
		if refused.Fix != "" {
			detail += " (" + refused.Fix + ")"
		}
		return detail
	}
	return err.Error()
}

// createBackup runs a create's steps of this file on the install it made: the settings, then the
// restore, which loads only while the install awaits its data (restore_awaited (from Stowage
// 0.48.0, a --secrets install whose release runs backups or whose hook took provision values); volumes_split), read right before it.
func (r *installResource) createBackup(ctx context.Context, m *installModel, resp *resource.CreateResponse) {
	planned := m.Backup
	due := backupPending{Config: !m.Backup.IsNull(), Restore: !m.Restore.IsNull()}
	r.backupSteps(ctx, m, due, resp.Private, resp.Diagnostics.AddWarning, func() (bool, error) { return r.awaitsData(ctx, m) }, &resp.Diagnostics)
	m.Backup = plannedOver(ctx, planned, m.Backup, &resp.Diagnostics)
}

// plannedOver is got (the settings the install answered) with each value planned known in its
// place: an apply saves what its plan said, and the next refresh reads what the install holds.
func plannedOver(ctx context.Context, planned, got types.Object, diags *diag.Diagnostics) types.Object {
	if !known(planned) || !known(got) {
		return withoutUnknowns(got, diags)
	}
	out := map[string]attr.Value{}
	pa, ga := planned.Attributes(), got.Attributes()
	for k, v := range ga {
		// The configuration's alone (not computed): as planned, null included. A computed one:
		// as planned when it was known, else the install's.
		if p, ok := pa[k]; ok && !p.IsUnknown() && (!p.IsNull() || !backupComputed[k]) {
			v = p
		}
		out[k] = v
	}
	o, d := types.ObjectValue(backupAttrTypes, out)
	diags.Append(d...)
	return withoutUnknowns(o, diags)
}

// withoutUnknowns is o with each value not known null: settings a step could not read back are
// saved as the plan knew them, the rest left out until a refresh reads them.
func withoutUnknowns(o types.Object, diags *diag.Diagnostics) types.Object {
	if o.IsUnknown() {
		return types.ObjectNull(backupAttrTypes)
	}
	if o.IsNull() {
		return o
	}
	out := map[string]attr.Value{}
	for k, v := range o.Attributes() {
		if v.IsUnknown() {
			v = nullOf(backupAttrTypes[k])
		}
		out[k] = v
	}
	n, d := types.ObjectValue(backupAttrTypes, out)
	diags.Append(d...)
	return n
}

// nullOf is the null value of t, one of backupAttrTypes'.
func nullOf(t attr.Type) attr.Value {
	switch t {
	case types.StringType:
		return types.StringNull()
	case types.Int64Type:
		return types.Int64Null()
	case types.BoolType:
		return types.BoolNull()
	}
	return types.ListNull(types.StringType)
}

// unknownOf is the unknown value of t, one of backupComputed's types.
func unknownOf(t attr.Type) attr.Value {
	switch t {
	case types.StringType:
		return types.StringUnknown()
	case types.Int64Type:
		return types.Int64Unknown()
	}
	return types.BoolUnknown()
}

// updateBackup runs an update's steps of this file, after the target's: `backup config` when the
// settings changed or a create left them pending, then a restore a create left pending, which
// runs only while the install still awaits its data (one cut short may have finished on the
// target: the VM's is a job of its daemon). A step that fails is an error, left pending; settings
// not applied keep state's, so the next plan shows them again.
func (r *installResource) updateBackup(ctx context.Context, p privateData, resp *resource.UpdateResponse, plan, state *installModel) {
	pending := getBackupPending(ctx, p)
	due := backupPending{Config: backupChanged(plan, state) || pending.Config && !plan.Backup.IsNull(), Restore: pending.Restore}
	if !due.Config && !due.Restore {
		if pending.Config {
			setBackupPending(ctx, resp.Private, backupPending{}, &resp.Diagnostics) // the settings taken out
		}
		return
	}
	var m installModel
	if resp.Diagnostics.Append(resp.State.Get(ctx, &m)...); resp.Diagnostics.HasError() {
		return
	}
	planned := plan.Backup
	r.backupSteps(ctx, &m, due, resp.Private, resp.Diagnostics.AddError, func() (bool, error) { return r.awaitsData(ctx, &m) }, &resp.Diagnostics)
	if due.Config && getBackupPending(ctx, resp.Private).Config {
		m.Backup = withoutUnknowns(state.Backup, &resp.Diagnostics)
	} else {
		m.Backup = plannedOver(ctx, planned, m.Backup, &resp.Diagnostics)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// awaitsData reports whether the install m holds waits for its data: given --secrets and not
// restored yet (restore_awaited), or a restore left its volumes half restored (volumes_split).
func (r *installResource) awaitsData(ctx context.Context, m *installModel) (bool, error) {
	switch targetKind(m) {
	case "docker":
		d, err := r.docker(m, m.ShortName.ValueString())
		if err != nil {
			return false, err
		}
		st, err := d.Status(ctx)
		if err != nil || st == nil {
			return false, err
		}
		return st.RestoreAwaited || st.VolumesSplit, nil
	case "vm":
		v, err := r.vm(m)
		if err != nil {
			return false, err
		}
		st, err := v.Status(ctx)
		if err != nil || st == nil {
			return false, err
		}
		return st.RestoreAwaited || st.VolumesSplit, nil
	}
	return false, fmt.Errorf("the %s target has no restore", targetKind(m))
}

// objectValidator refuses a restore.object that is no backup's name (installer.ValidObject): the
// rule restore runs with, checked at validate.
type objectValidator struct{}

func (objectValidator) Description(context.Context) string {
	return "a backup's name (`backup list`): no leading -, no blank"
}

func (v objectValidator) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (objectValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if !known(req.ConfigValue) {
		return
	}
	if err := installer.ValidObject(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "restore.object is no backup's name", err.Error())
	}
}

// byHand says what a restore by hand loads: into any install (--unrecorded as the configuration
// says), into an install made again from an export (always --unrecorded: it holds no records of
// the old machine's backups), or the restore a cut one left half done (made again too).
type byHand int

const (
	byHandAny byHand = iota
	byHandMadeAgain
	byHandCut
)

// restoreByHand is the restore an operator runs on m's target, the identity on standard input:
// of rs's object, else, finishing a cut restore, of the object it named (the first line of the
// install's .stowage-volumes-split), else of one the install's `backup list` names; through sudo
// over SSH on the VM (its install in /usr/local/<name>), with the install's directory on Docker.
func restoreByHand(m *installModel, rs *restoreModel, how byHand) string {
	object, unrecorded := "OBJECT", " --unrecorded"
	if rs != nil && known(rs.Object) {
		object = shellWord(rs.Object.ValueString())
		switch {
		case how != byHandAny:
		case !known(rs.Unrecorded):
			unrecorded = " [--unrecorded]"
		case !rs.Unrecorded.ValueBool():
			unrecorded = ""
		}
	} else {
		rs = nil
	}
	var cmd, list, split string
	if targetKind(m) == "vm" && m.VM != nil {
		ctl := "sudo " + m.VM.ShortName.ValueString() + "-stow"
		cmd = fmt.Sprintf("`%s restore %s%s --yes --identity -` over SSH (the identity on standard input)", ctl, object, unrecorded)
		list = fmt.Sprintf("`%s backup list`", ctl)
		name := installName(m)
		if name == "" {
			name = "<name>"
		}
		split = "/usr/local/" + name + "/.stowage-volumes-split"
	} else {
		dir := shellWord(m.ID.ValueString())
		ctl := shellWord(m.ID.ValueString() + "/" + m.ShortName.ValueString() + "-stow")
		cmd = fmt.Sprintf("`%s restore %s%s --yes --identity - --dir %s` on the host (the identity on standard input)", ctl, object, unrecorded, dir)
		list = fmt.Sprintf("`%s backup list --dir %s`", ctl, dir)
		split = m.ID.ValueString() + "/.stowage-volumes-split"
	}
	switch {
	case rs == nil && how == byHandCut:
		return cmd + ", OBJECT the cut restore's (the first line of " + split + " on the target),"
	case rs == nil:
		return cmd + ", OBJECT one " + list + " names,"
	}
	return cmd
}

// shellWord is s as one shell word for a hint: as it is when it holds only safe characters, else
// quoted (shellQuote).
func shellWord(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=+@,") == "" {
		return s
	}
	return shellQuote(s)
}
