package provider

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// interfaceType is an interfaces entry's.
var interfaceType = types.ObjectType{AttrTypes: map[string]attr.Type{"mac": types.StringType, "mode": types.StringType, "address": types.StringType}}

// vmAttributes are the vm block's: the SSH connection, the release, and what the provider learns.
func vmAttributes() map[string]schema.Attribute {
	vm := sshAttributes("The VM's address, reached over SSH by the system `ssh` binary (the VM's own resource's address output).")
	vm["known_hosts_line"] = schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "The key the VM serves until the provider has read its own: `stowage_vm_seed`'s `known_hosts_line` (its one-time key) for a VM the provider installs, the VM's own key for an imported one. " +
			"That key alone is accepted (`StrictHostKeyChecking=yes`), never one learnt on first contact. Required to install, and for an imported state's first apply; once `host_key_line` is known it may be left out.",
	}
	vm["version"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The release the VM runs. A change upgrades the VM (`sudo -n <short>-stow upgrade --version V --yes --json`, then its reboot followed), or rolls it back to `previous.version` with `allow_rollback`."}
	vm["channel"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The channel the VM follows, which its seed's `channel` set: a create, and an imported state's first apply, refuse one other than the VM's (state would record it). First-install only."}
	vm["short_name"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The installer's short name, `stowage_vm_seed`'s `short_name`: the VM runs `<short_name>-stow`."}
	vm["host_key_line"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "The VM's own SSH host key as a `known_hosts` line, read through the one-time key once its first install is done (`/etc/ssh/ssh_host_ed25519_key.pub`): every later connection accepts it.",
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
	vm["platform"] = schema.StringAttribute{
		Optional:            true,
		MarkdownDescription: "Where the VM runs, `stowage_vm_seed`'s `platform`: a wait that runs out names that platform's console, and a destroy checks the registry still holds a disk set for it (a warning when it holds none: a VM destroyed is not made again from it).",
	}
	vm["interfaces"] = schema.MapNestedAttribute{
		Optional: true,
		MarkdownDescription: "NICs of the release's secondary roles (`host.interfaces`) the VM's platform attached after its seed, by role: `mac`, the MAC the platform gave the NIC, and its addressing, `mode` (`dhcp`, `static` or `none`, the installer's default) and `address` (with `static`, IPv4 with its prefix). " +
			"A role added or changed runs `sudo -n <short>-stow network attach --role R --mac M [--mode] [--address]` on the VM over SSH, then reboots it (`<short>-stow reboot`) and waits for its boot's health verdict: the NIC is named from that boot. The platform's device comes first — the root adds it and restarts the VM (TrueNAS: `truenas_vm_power`'s `restart_triggers`), and this resource depends on that. A role taken out runs nothing: the VM keeps its record. A change of `interfaces` goes alone, without a change of `version`.",
		NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
			"mac":     schema.StringAttribute{Required: true, MarkdownDescription: "The NIC's MAC, as the platform gave it."},
			"mode":    schema.StringAttribute{Optional: true, MarkdownDescription: "`dhcp`, `static` or `none` (the link up, no address); left out, `none`.", Validators: []validator.String{stringvalidator.OneOf("dhcp", "static", "none")}},
			"address": schema.StringAttribute{Optional: true, MarkdownDescription: "With `mode = \"static\"`: the IPv4 address with its prefix, e.g. `192.0.2.20/24`."},
		}},
	}
	vm["ssh_allow"] = schema.SetAttribute{
		Optional:    true,
		ElementType: types.StringType,
		MarkdownDescription: "The sources SSH accepts on the VM, each a CIDR with its prefix written masked (`192.0.2.0/24`, `2001:db8::/32`, `0.0.0.0/0` for every IPv4 source): set over whatever set them before — `stowage_vm_seed`'s `ssh_allow`, which is the first boot's alone. " +
			"A change is made in place: the apply reads the VM's (`sudo -n <short>-stow ssh-allow --json` over SSH) and, when they differ, runs `sudo -n <short>-stow ssh-allow set <sources, sorted> --json`, which applies them at once and undoes them unless confirmed within the VM's window (90 s), then `sudo -n <short>-stow ssh-allow confirm --json` over a new SSH connection: that the VM lets it in proves the new sources hold the address OpenTofu runs from. " +
			"A confirm the VM does not let in fails the apply, and the window undoes the change by itself; a confirm that fails otherwise is followed by `ssh-allow revert --json`. A change already awaiting confirmation on the VM fails the apply, naming `ssh-allow confirm` and `revert`. " +
			"Applied after the apply's other steps, once the VM is back from any reboot; at create, after the install, and a failure there is a warning: the install is recorded, and the next plan shows an update that applies the sources again. " +
			"A refresh reads the VM's sources into state, so a change made on the VM shows in the plan (a change awaiting confirmation there is read as its sources, with a warning; a rule file that does not read as SSH's sources leaves state as it is, with a warning). " +
			"Left out, the provider never reads nor changes them. Each source is refused at plan unless it is a CIDR equal to its masked form (`192.0.2.9/24` is refused, naming `192.0.2.0/24`), an IPv4 network written IPv4-mapped is refused, naming its IPv4 form (`::ffff:203.0.113.0/120` → `203.0.113.0/24`), and an empty set is refused. " +
			"Changing the sources from an address the old ones refuse cannot reach the VM: add the new address beside the old, apply, then take the old out (the VM guide). Needs an OS built by Stowage " + installer.SSHAllowMinStowage + " or later (`<short>-stow ssh-allow`).",
		Validators: []validator.Set{setvalidator.SizeAtLeast(1), setvalidator.ValueStringsAre(maskedCIDR{})},
	}
	vm["seed_boot_id"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "Set only when the one-time key's retirement after the install failed: the boot the install finished in, during which the VM still serves that key (its next boot retires it). While it is set, a connection accepts `known_hosts_line` too. Null once a later boot is seen.",
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
	}
	return vm
}

// validateVM checks the vm block's arguments, and refuses what the VM target cannot give.
// known_hosts_line may be left out here: once the provider read the VM's own key, state holds it
// (host_key_line); a create, and a first apply of an imported state, refuse its absence.
func validateVM(m *installModel, diags *diag.Diagnostics) {
	v := m.VM
	line := v.KnownHostsLine
	if line.IsNull() {
		line = types.StringUnknown() // neither required nor parsed here
	}
	validateSSH(path.Root("vm"), v.Host, v.User, line, v.PrivateKeyFile, v.Port, false, diags)
	if v.Version.IsNull() {
		diags.AddAttributeError(path.Root("vm").AtName("version"), "version is required", "Give the release the VM runs.")
	}
	switch {
	case v.ShortName.IsNull():
		diags.AddAttributeError(path.Root("vm").AtName("short_name"), "short_name is required",
			"Give the installer's short name, stowage_vm_seed's short_name: the VM runs <short_name>-stow.")
	case known(v.ShortName) && !installer.ValidShortName(v.ShortName.ValueString()):
		diags.AddAttributeError(path.Root("vm").AtName("short_name"), "not an installer's short name",
			fmt.Sprintf("%q is not an installer's short name: give stowage_vm_seed's short_name.", v.ShortName.ValueString()))
	}
}

// planVM is the plan's part of a VM: known_hosts_line kept while the one-time key is served;
// the VM's keys planned unknown where an update may change them; a change of version back to
// the previous release is a rollback, refused without allow_rollback and alone.
func planVM(ctx context.Context, plan, state *installModel, resp *resource.ModifyPlanResponse) {
	if plan.VM == nil || state.VM == nil {
		return
	}
	if known(state.VM.SeedBootID) && plan.VM.KnownHostsLine.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("known_hosts_line"), "known_hosts_line is still needed",
			"The VM still serves its one-time key (seed_boot_id: its retirement failed, and no later boot was seen), which this line accepts: keep it until the VM reboots.")
	}
	// The keys an update may change besides the plan's own: the VM's own key when state has none
	// (an update learns it, and may record seed_boot_id), and seed_boot_id while it is set (a
	// later boot clears it). The plan holds state's values otherwise (UseStateForUnknown); the
	// release the VM reports is ModifyPlan's to plan unknown. A pending attach is such an update
	// too: it learns the key, and its reboot retires seed_boot_id.
	if imported(state) || len(remoteChange(plan, state)) > 0 || len(pendingRoles(ctx, resp.Private)) > 0 {
		if state.VM.HostKeyLine.IsNull() {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("host_key_line"), types.StringUnknown())...)
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("seed_boot_id"), types.StringUnknown())...)
		}
		if known(state.VM.SeedBootID) {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("seed_boot_id"), types.StringUnknown())...)
		}
	}
	attach := withPending(attachments(plan.VM.Interfaces, state.VM.Interfaces), plan.VM.Interfaces, pendingRoles(ctx, resp.Private))
	if !plan.VM.Version.Equal(state.VM.Version) && !imported(state) && len(attach) > 0 {
		interfacesWithVersion(&resp.Diagnostics)
	}
	if pending := pendingRoles(ctx, resp.Private); len(pending) > 0 {
		resp.Diagnostics.AddAttributeWarning(path.Root("vm").AtName("interfaces"), "NICs not attached yet",
			fmt.Sprintf("The create recorded interfaces %s, and attaching them failed: this apply attaches them (`network attach`), then reboots the VM.", strings.Join(pending, ", ")))
	}
	if !plan.VM.SSHAllow.IsNull() && sshAllowPending(ctx, resp.Private) {
		resp.Diagnostics.AddAttributeWarning(sshAllowPath, "SSH's sources not set yet",
			"The create recorded ssh_allow and did not set it on the VM (its warning said why): this apply sets it (`ssh-allow set`, then `ssh-allow confirm` over a new SSH connection).")
	}
	if imported(state) && len(attach) > 0 {
		resp.Diagnostics.AddAttributeWarning(path.Root("vm").AtName("interfaces"), "The first apply reads the VM's NICs",
			"The state was imported: the apply reads the NICs the VM names (`network --json`) and takes over each role it names by the MAC given, whatever its addressing (a role addressed otherwise than given is said in a warning naming the `network set --role` that aligns it; state records interfaces as configured). It attaches the others (`network attach`, with the addressing given) and reboots the VM to name them; when the VM's NICs cannot be read, it attaches them all and reboots. plan_dry_run reads them at plan.")
	}
	if !known(plan.VM.Version) {
		return
	}
	desired := plan.VM.Version.ValueString()
	if !plan.VM.Version.Equal(state.VM.Version) {
		if prev := previousVersion(ctx, state); prev != "" && prev == desired {
			// allow_rollback not known yet is the apply's to read: only a known false refuses now.
			switch {
			case !known(plan.AllowRollback):
				// Not known yet: the apply reads it and refuses or rolls back alone.
			case rollbackRefused(plan.AllowRollback):
				resp.Diagnostics.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
					fmt.Sprintf("version is now %s, the release the VM's last upgrade replaced (previous.version): reaching it is `rollback`, which reboots the VM into its passive system, the previous release (every input keeps its value). Set allow_rollback = true to roll back, or set version back to %s.", prev, state.VM.Version.ValueString()))
			default:
				if changed := slices.DeleteFunc(remoteChange(plan, state), func(a string) bool { return a == "version" }); len(changed) > 0 {
					vmRollbackAlone(&resp.Diagnostics, strings.Join(changed, ", "))
				}
			}
		}
	}
}

// interfacesWithVersion is the refusal of NICs attached with a release change.
func interfacesWithVersion(diags *diag.Diagnostics) {
	diags.AddAttributeError(path.Root("vm").AtName("interfaces"), "interfaces change with version",
		"A NIC attached is named by the VM's next boot, which a release change's reboot is not followed for: apply the interfaces alone (version as the VM runs it), then the version.")
}

// vmRollbackAlone is the refusal of a VM's rollback with other changes.
func vmRollbackAlone(diags *diag.Diagnostics, changed string) {
	diags.AddAttributeError(path.Root("allow_rollback"), "a rollback goes alone",
		fmt.Sprintf("version goes back to previous.version, a rollback, which reboots the VM into its passive system: %s changed with it are not applied with it. Apply the rollback alone, then the change.", changed))
}

// previousVersion is the version state's previous names; "" when none.
func previousVersion(ctx context.Context, m *installModel) string {
	if !known(m.Previous) {
		return ""
	}
	v, ok := m.Previous.Attributes()["version"].(types.String)
	if !ok {
		return ""
	}
	return v.ValueString()
}

// vm is the driver of m's VM, accepting the keys the VM may serve: its own once read, and the
// configuration's line until then — and beside its own while the boot that read it may still
// serve the one-time key (seed_boot_id).
func (r *installResource) vm(m *installModel) (installer.VM, error) {
	return r.vmWithin(m, 0)
}

// vmWithin is vm, its connection bounded to connect seconds (0: the default).
func (r *installResource) vmWithin(m *installModel, connect int) (installer.VM, error) {
	if r.data == nil {
		return installer.VM{}, errors.New("the provider is not configured")
	}
	v := m.VM
	c := Connection{
		Host: v.Host.ValueString(), Port: int(v.Port.ValueInt64()), User: v.User.ValueString(),
		KeyFile: v.PrivateKeyFile.ValueString(), KnownHostsLine: v.KnownHostsLine.ValueString(),
		ConnectTimeout: connect,
	}
	if known(v.HostKeyLine) {
		c.KnownHostsLine = v.HostKeyLine.ValueString()
		if known(v.SeedBootID) && known(v.KnownHostsLine) {
			c.AlsoKnownHostsLines = []string{v.KnownHostsLine.ValueString()}
		}
	}
	if c.Host == "" {
		return installer.VM{}, errors.New("host is not known")
	}
	if c.KnownHostsLine == "" {
		return installer.VM{}, errors.New("known_hosts_line is required to install: no host key to accept yet; give the VM's own key (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on its console), or stowage_vm_seed's known_hosts_line for a VM not installed yet")
	}
	run, err := r.data.NewRunner(c)
	if err != nil {
		return installer.VM{}, err
	}
	return installer.VM{Runner: run, Short: v.ShortName.ValueString()}, nil
}

// resourceTimeout is the bound a wait within the resource's timeouts names.
const resourceTimeout = "the timeout (the resource's timeouts block)"

// sshFailed says whether err is ssh's own failure: the VM not answering yet, or rebooting.
func sshFailed(err error) bool {
	var f *installer.FailedError
	return errors.As(err, &f) && f.Exit == installer.SSHFailure || errors.Is(err, installer.ErrCut)
}

// waitRanOut is a wait whose bound ran out: what it waited for, and its last attempt's error.
type waitRanOut string

func (e waitRanOut) Error() string { return string(e) }

// wait runs step every Poll until it says done, within ctx's deadline; what, said when the
// deadline ends, names what was waited for, with the step's last error, and bound the deadline.
func (r *installResource) wait(ctx context.Context, what, bound string, step func() (bool, error)) error {
	var last error
	timedOut := func() error {
		msg := fmt.Sprintf("%s when %s ran out", what, bound)
		if last != nil {
			msg += "; the last attempt: " + last.Error()
		}
		return waitRanOut(msg)
	}
	for {
		done, err := step()
		if ctx.Err() != nil {
			return timedOut() // a step the deadline cut tells nothing of the VM
		}
		if done {
			return err
		}
		if err != nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return timedOut()
		case <-time.After(r.data.Poll):
		}
	}
}

// settleVM reads the VM until its first install is not running any more. A read that fails is
// tried again: early in the first boot, sshd answers before the VM's daemon does.
func (r *installResource) settleVM(ctx context.Context, v installer.VM) (*installer.VMStatus, error) {
	var st *installer.VMStatus
	err := r.wait(ctx, "the VM's first install was still running, or its status could not be read,", resourceTimeout, func() (bool, error) {
		s, err := v.StatusOf(ctx, true)
		if err != nil {
			return false, err
		}
		st = s
		return !s.Installing || s.InstallFailed != "", nil
	})
	if err == nil && st != nil {
		r.noteRecorded(st.Values)
	}
	return st, err
}

// bootVerdict is what followed a reboot, from the VM's event journal: the health gate's verdict
// on the boot the VM settled in (healthy or kept_unhealthy; zero when none was read), the
// went_back verdicts of the boots judged on the way, the boot the VM ran at the last read, and
// whether that boot is Kairos's fallback (the passive system); back: the change was a rollback,
// whose passive boot is the fallback by choice.
type bootVerdict struct {
	final    installer.Event
	wentBack []installer.Event
	boot     string
	fallback bool
	back     bool
}

// rebooted reads the VM until the health gate judged a boot other than before's and the VM
// stays in it: the reboot an upgrade or a rollback scheduled, followed to its verdict. The gate
// judges every boot, after its boot unit has taken the install to the release the OS carries (a
// rollback's passive boot answers before that; it is never on trial), so the verdict, never the
// status alone, ends the wait. Each poll reads the journal after the entry before's status named;
// a boot whose last verdict is went_back is waited on (the gate reboots it into the passive
// system, whose boot is judged in turn; a went_back whose reboot failed leaves the VM in that
// boot, and the wait runs out naming it). The status is read once a verdict ends the wait,
// which it does when the VM runs the boot judged; on a timeout, the error names the verdicts read.
func (r *installResource) rebooted(ctx context.Context, v installer.VM, before *installer.VMStatus, back bool) (*installer.VMStatus, *bootVerdict, error) {
	var st *installer.VMStatus
	var after int64
	if before.Events != nil {
		after = *before.Events
	}
	var judged []installer.Event // each boot's last verdict, boots after before's, in the journal's order
	err := r.wait(ctx, "the VM had not come back from its reboot with the health gate's verdict on its boot", resourceTimeout, func() (bool, error) {
		evs, err := v.Events(ctx, after)
		if err != nil {
			return false, err // rebooting: ssh fails, or the daemon is not up yet
		}
		for _, e := range evs {
			after = e.Seq // as Stowage's own reader: a journal that started again is read on from its entries
			if e.Kind != installer.EvBootJudged || e.BootID == "" || e.BootID == before.BootID {
				continue
			}
			if i := slices.IndexFunc(judged, func(j installer.Event) bool { return j.BootID == e.BootID }); i >= 0 {
				judged[i] = e
			} else {
				judged = append(judged, e)
			}
		}
		if len(judged) == 0 {
			return false, errors.New("the journal holds no verdict yet on a boot after the change's")
		}
		e := judged[len(judged)-1]
		if e.Verdict == installer.VerdictWentBack {
			s, err := v.Status(ctx)
			if err != nil {
				return false, err
			}
			st = s // the boot the VM runs, for the timeout's account
			if s.BootID == e.BootID {
				return false, fmt.Errorf("the gate did not keep boot %s, and the VM still runs it", e.BootID)
			}
			return false, fmt.Errorf("the gate did not keep boot %s; the VM answers from boot %s, not judged yet", e.BootID, s.BootID)
		}
		s, err := v.Status(ctx)
		if err != nil {
			return false, err // the verdicts are kept: the next poll reads on
		}
		st = s
		if s.BootID != e.BootID {
			return false, fmt.Errorf("the gate judged boot %s, and the VM answers from boot %s", e.BootID, s.BootID)
		}
		return true, nil
	})
	verdict := &bootVerdict{back: back}
	for _, e := range judged {
		if e.Verdict == installer.VerdictWentBack {
			verdict.wentBack = append(verdict.wentBack, e)
		}
	}
	if st != nil {
		verdict.boot, verdict.fallback = st.BootID, st.Fallback
	}
	if err != nil {
		if d := verdictDetail("", "", verdict); d != "" {
			err = fmt.Errorf("%w. %s", err, d)
		}
		return st, verdict, err
	}
	verdict.final = judged[len(judged)-1]
	return st, verdict, nil
}

// applyVMStatus puts the VM's status into m: on a read, the release the VM runs replaces
// version, so the next plan proposes the change back (a fallback shows its real release).
func applyVMStatus(m *installModel, st *installer.VMStatus, read bool) {
	applyStatus(m, &st.Status, false)
	applyHeld(m, &st.Status, false)
	if read && st.Installed != nil {
		vm := *m.VM
		vm.Version = types.StringValue(st.Installed.Version)
		m.VM = &vm
	}
}

// unreadBoot is seed_boot_id when the VM reported no boot id: no boot clears it, so the one-time
// key stays accepted (the VM's reboot cannot be told from a read).
const unreadBoot = "unread"

// retireSeed forgets the one-time key once the VM answers from a later boot than the one its
// install finished in: that boot retired it.
func retireSeed(m *installModel, st *installer.VMStatus) {
	if known(m.VM.SeedBootID) && m.VM.SeedBootID.ValueString() != unreadBoot && st.BootID != "" && st.BootID != m.VM.SeedBootID.ValueString() {
		vm := *m.VM
		vm.SeedBootID = types.StringNull()
		m.VM = &vm
	}
}

func vmID(m *installModel) string {
	return m.VM.Host.ValueString() + "/" + m.VM.ShortName.ValueString() + "-stow"
}

// vmFill sets what a VM's state holds whatever the status: the ID, the short name, the registry,
// no write-only value.
func (r *installResource) vmFill(m *installModel, state *installModel) {
	if state != nil && known(state.ID) {
		m.ID = state.ID
	} else {
		m.ID = types.StringValue(vmID(m))
	}
	m.ShortName = m.VM.ShortName
	if state != nil && known(state.Registry) {
		m.Registry = state.Registry
	} else {
		m.Registry = types.StringValue(r.registry())
	}
	if state != nil && !state.RegistryCADigest.IsUnknown() && !imported(state) {
		m.RegistryCADigest = state.RegistryCADigest
	} else {
		m.RegistryCADigest = r.providerCA()
	}
	m.SecretInputsWO, m.GivenWO, m.SecretsWO = types.MapNull(types.StringType), types.StringNull(), types.StringNull()
}

func (r *installResource) createVM(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m installModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	timeout, diags := m.Timeouts.Create(ctx, createTimeout)
	resp.Diagnostics.Append(diags...)
	var secretsWO types.Map
	var given, exported types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("given_wo"), &given)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secrets_wo"), &exported)...)
	secrets := stringMap(ctx, secretsWO, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	v, err := r.vm(&m)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the VM", err.Error())
		return
	}
	desc, err := r.firstBoot(ctx, v)
	missing, probe := missingCtl(ctx, v, err)
	if missing {
		var f *installer.FailedError
		errors.As(err, &f)
		detail := "The VM runs the installer its seed carries (/usr/sbin/<short>-stow): set short_name to stowage_vm_seed's short_name."
		if f.Stderr != "" {
			detail = "The VM's shell: " + f.Stderr + "\n\n" + detail
		}
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("short_name"), "The VM has no "+v.Short+"-stow", detail)
		return
	}
	err = probeNote(err, v, probe)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Waiting for the VM's first boot", consoleHint(err, m.VM.Platform))
		return
	}
	if len(desc.Shapes) > 0 && !slices.Contains(desc.Shapes, "kairos") {
		resp.Diagnostics.AddError("No Kairos shape", fmt.Sprintf("The VM's %s %s offers %v, not kairos.", desc.Name, desc.Release.Version, desc.Shapes))
		return
	}
	// A create installs the release the VM's OS carries, never upgrades: an upgrade that failed
	// would leave a created object OpenTofu replaces at the next apply.
	if desired := m.VM.Version.ValueString(); desired != desc.Release.Version {
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("version"), "version is not the VM's release",
			fmt.Sprintf("The VM's OS carries %s %s (its seed's version), and a create installs that release: set version = %q (stowage_vm_seed's version), apply, then change version to upgrade.", desc.Name, desc.Release.Version, desc.Release.Version))
		return
	}
	refuseSecretInputs(&resp.Diagnostics, stringMap(ctx, m.Inputs, &resp.Diagnostics), desc)
	createRotations(ctx, resp.Private, m.SecretRotations, desc, installer.ShapeVM, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	change := installer.Change{Inputs: stringMap(ctx, m.Inputs, &resp.Diagnostics), SecretInputs: secrets, TLS: m.TLS.ValueString(),
		Domains: stringMap(ctx, m.Domains, &resp.Diagnostics), Ports: portMap(ctx, m.Ports, &resp.Diagnostics),
		AcmeCA: m.AcmeCA.ValueString(), AcmeCARoot: m.AcmeCARoot.ValueString()}
	kit, code := r.kit(&m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.kitLost {
		// A kit expected and absent: refused. The VM's install takes a kit whether it is its first
		// or the re-run of the one its first boot made, so an install there (or one that failed)
		// may have taken it: the refusal then also names going on without one. While an earlier
		// create's first install is marked as running, the create is refused too, naming the ways
		// on.
		st, err := v.Status(ctx)
		if err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the VM", err)
			return
		}
		// Once the run has ended, the create names both ways on; a run cut short is finished
		// first over SSH. Only an install whose .env is written reports installing.
		kitThen := fmt.Sprintf("apply again: the create then names both ways on (issue a kit again, or go on without it if the install took it). A run cut by a timeout stays marked installing: finish it over SSH by `sudo %s-stow install`, then apply again.", v.Short)
		if st != nil && stillInstalling(&st.Status, "an earlier create's, or the VM's first boot's", kitThen, &resp.Diagnostics) {
			return
		}
		detail := kitLost("kit_wo")
		if st != nil && (st.Installed != nil || st.InstallFailed != "") {
			detail += " If the VM's install took this kit in an earlier apply that failed after it, remove kit_wo_version, kit_wo and kit_code_wo instead: the create goes on without a kit (once that apply's run has ended: the installer refuses a run beside another)."
		}
		resp.Diagnostics.AddAttributeError(path.Root("kit_wo"), "The kit is not here", detail)
		return
	}

	// An install on the VM before this create's (an earlier apply's) that awaits its data took a
	// secrets export: it is not given again, as the installer refuses it once taken. One that does
	// not await it holds its own data: refused here, naming the way on.
	if strings.TrimSpace(exported.ValueString()) != "" {
		st, err := v.Status(ctx)
		if err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the VM", err)
			return
		}
		// The installer says whether its install took an export (made_again): the provider asks
		// rather than guesses.
		// Only an install whose .env is written reports installing: it took the export already.
		finish := fmt.Sprintf("`sudo %s-stow install` over SSH finishes it", v.Short)
		if st != nil && stillInstalling(&st.Status, "an earlier create's, or the VM's first boot's", "apply again. A run cut by a timeout stays marked installing: "+finish+".", &resp.Diagnostics) {
			return
		}
		switch {
		case st != nil && st.VolumesSplit:
			// The installer's re-run refuses it: the restore finishes it, by hand.
			resp.Diagnostics.AddError("A restore left the VM's volumes half restored",
				"status --json volumes_split: an earlier apply's restore was cut, or failed while swapping the volumes in. "+restoreByHand(&m, restoreOf(ctx, m.Restore, &resp.Diagnostics), byHandCut)+
					" finishes it (the services stay stopped until then); then apply again.")
			return
		case st != nil && (st.MadeAgain || st.RestoreAwaited):
			// Read only once its .env is written: installed, and the installer refuses the export
			// again. One with no .env (installed null) is a first install still to make: given the
			// export again. restore_awaited: only an install made by --secrets is marked, whatever
			// the installer's release (one before Stowage 0.48.0 says no made_again).
			exported = types.StringNull()
			resp.Diagnostics.AddAttributeWarning(path.Root("secrets_wo"), "The VM's install took the secrets export",
				"An earlier apply's install took it (status --json made_again, or restore_awaited): the export is not given again (the installer refuses it once taken), and the install is run again with the configuration's settings, without it.")
		case st != nil && st.Installed != nil && isSeededFile(exported.ValueString()):
			// A new install's secrets file makes a plain install, which says nothing of it: whose
			// install this is, the VM does not tell.
			resp.Diagnostics.AddAttributeError(path.Root("secrets_wo"), "The VM holds an install of its own",
				"The VM is installed, and secrets_wo is a new install's secrets file (no `export: true`), which the installer refuses on an install that exists. If an earlier apply's install took it, remove secrets_wo to take that install as it is (it holds those secrets); else make the VM again from a seed with await_secrets.")
			return
		case st != nil && st.Installed != nil:
			resp.Diagnostics.AddAttributeError(path.Root("secrets_wo"), "The VM holds an install of its own",
				"The VM is installed and its status says no secrets export made it (no made_again, nor restore_awaited): its seed did not await secrets — or, with an installer before Stowage 0.48.0, which says no made_again, an earlier apply's install took it and awaits nothing. Remove secrets_wo, restore and restore_identity_wo to take this install as it is (an install made from the export that way has none of its data loaded: "+restoreByHand(&m, restoreOf(ctx, m.Restore, &resp.Diagnostics), byHandMadeAgain)+" loads it, replacing the data the install holds now), or make the VM again from a seed with await_secrets.")
			return
		}
	}
	// The VM's own login first: its install reads the registry with it.
	if r.wo.login != "" {
		if err := v.RegistryLogin(ctx, r.wo.login); err != nil {
			addInstallerError(&resp.Diagnostics, "Giving the VM its registry login", err)
			return
		}
	}
	// A secrets export goes to the first install a seed's --await-secrets left waiting for it; the
	// installer refuses it on a VM whose first install ran.
	if _, err := v.Install(ctx, installer.VMInstall{Change: change, Given: given.ValueString(), Secrets: exported.ValueString(), Kit: kit, KitCode: code}); err != nil {
		addInstallerError(&resp.Diagnostics, "Installing "+desc.Name+" on the VM", err)
		return
	}
	st, err := r.settleVM(ctx, v)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the VM", consoleHint(err, m.VM.Platform))
		return
	}
	if st.InstallFailed != "" || st.Installed == nil {
		why := st.InstallFailed
		if why == "" {
			why = "status --json reports no release installed"
		}
		resp.Diagnostics.AddError("Installing "+desc.Name+" on the VM failed",
			fmt.Sprintf("%s. Apply again: the VM's install runs again and finishes it.", why))
		return
	}
	if !vmChannelCheck(&resp.Diagnostics, m.VM.Channel, st.Channel, "The install is done: apply again once channel is fixed, and the create goes on from it.") {
		return
	}
	vmc := *m.VM
	var warns []string
	vmc.HostKeyLine, vmc.SeedBootID, warns = r.learnKey(ctx, &m, v, st.BootID)
	for _, w := range warns {
		summary, detail, _ := strings.Cut(w, "\n")
		resp.Diagnostics.AddWarning(summary, detail)
	}
	m.VM = &vmc
	r.vmFill(&m, nil)
	applyVMStatus(&m, st, false)
	r.createBackup(ctx, &m, resp)
	if !m.VM.SSHAllow.IsNull() {
		// Set last, after the NICs' reboot: pending until then, so a create that stops before
		// it leaves the next plan an update that sets them.
		setSSHAllowPending(ctx, resp.Private, true, &resp.Diagnostics)
	}
	if attach := attachments(m.VM.Interfaces, types.MapNull(interfaceType)); len(attach) > 0 {
		// The install is made: recorded as planned, its interfaces marked pending in private
		// state until they are attached, so an attach that fails leaves a state consistent with
		// the plan and the next plan an update attaching them again (ModifyPlan). Not an error:
		// one would taint the install, whose replacement is a destroy.
		resp.Diagnostics.Append(setPending(ctx, resp.Private, attach)...)
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		if resp.Diagnostics.HasError() {
			return
		}
		keyed, kv, err := r.rekey(m, m.VM.HostKeyLine, m.VM.SeedBootID)
		if err != nil {
			resp.Diagnostics.AddWarning("The VM's NICs are not attached",
				"Reaching the VM under its keys: "+err.Error()+". The install is recorded, its interfaces pending: the next plan shows an update that attaches them again.")
			return
		}
		after, err := r.attach(ctx, kv, st, attach)
		var sick *unhealthyAttach
		if err != nil && !errors.As(err, &sick) {
			resp.Diagnostics.AddWarning("The VM's NICs are not attached",
				consoleHint(err, m.VM.Platform).Error()+". The install is recorded, its interfaces pending: the next plan shows an update that attaches them again.")
			return
		}
		// Attached, whatever the verdict: nothing pending.
		m = keyed
		applyVMStatus(&m, after, false)
		retireSeed(&m, after)
		resp.Diagnostics.Append(setPending(ctx, resp.Private, nil)...)
		if sick != nil {
			// A warning, as this path's: an error would taint the install.
			resp.Diagnostics.AddWarning("The VM came back unhealthy from attaching its NICs", consoleHint(sick, m.VM.Platform).Error()+".")
		}
	}
	r.createSSHAllow(ctx, &m, resp)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// pendingKey is the private state's key of the interfaces a create recorded and could not attach:
// their roles, a JSON list.
const pendingKey = "attach_pending"

// privateData is the framework's private state of a resource, as each request carries it.
type privateData interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// setPending records the roles of attach as pending in p; none removes the key.
func setPending(ctx context.Context, p privateData, attach []installer.Interface) diag.Diagnostics {
	if len(attach) == 0 {
		return p.SetKey(ctx, pendingKey, nil)
	}
	roles := make([]string, 0, len(attach))
	for _, i := range attach {
		roles = append(roles, i.Role)
	}
	b, _ := json.Marshal(roles) // a list of strings always marshals
	return p.SetKey(ctx, pendingKey, b)
}

// pendingRoles are the roles p holds pending; none without p.
func pendingRoles(ctx context.Context, p privateData) []string {
	if p == nil || reflect.ValueOf(p).IsNil() {
		return nil
	}
	b, d := p.GetKey(ctx, pendingKey)
	var roles []string
	if d.HasError() || len(b) == 0 || json.Unmarshal(b, &roles) != nil {
		return nil
	}
	return roles
}

// withPending is attach, with the interfaces of the pending roles plan still names, sorted by role.
func withPending(attach []installer.Interface, plan types.Map, pending []string) []installer.Interface {
	for _, role := range pending {
		if slices.ContainsFunc(attach, func(i installer.Interface) bool { return i.Role == role }) {
			continue
		}
		if i, ok := interfaceOf(plan, role); ok {
			attach = append(attach, i)
		}
	}
	slices.SortFunc(attach, func(a, b installer.Interface) int { return strings.Compare(a.Role, b.Role) })
	return attach
}

// notAttached is plan's interfaces as they stand before attach runs: the roles attach names as
// state has them (left out when state has none), every other role as planned.
func notAttached(plan, state types.Map, attach []installer.Interface) types.Map {
	if !known(plan) {
		return state
	}
	was := map[string]attr.Value{}
	if known(state) {
		was = state.Elements()
	}
	out := map[string]attr.Value{}
	for role, v := range plan.Elements() {
		if !slices.ContainsFunc(attach, func(i installer.Interface) bool { return i.Role == role }) {
			out[role] = v
		} else if old, ok := was[role]; ok {
			out[role] = old
		}
	}
	return types.MapValueMust(interfaceType, out)
}

// newNICs is attach without the roles the VM names already (`network --json`) by the MAC given,
// whatever their addressing: an imported VM's first apply attaches only what is new, and never
// attaches again (nor reboots for) a NIC the VM has. A role named with other addressing than the
// one given is said in a warning naming the `network set --role` that aligns it (realign); state
// records the configuration. When the VM's NICs cannot be read, every one is attached (and the VM
// reboots), said in a warning.
func newNICs(ctx context.Context, v installer.VM, attach []installer.Interface, diags *diag.Diagnostics) []installer.Interface {
	roles, err := v.NetworkRoles(ctx)
	if err != nil {
		diags.AddWarning("The VM's NICs could not be read",
			fmt.Sprintf("%v: every interface is attached (`network attach`), and the VM reboots to name them.", err))
		return attach
	}
	var left []installer.Interface
	var fixes []string
	for _, i := range attach {
		r, ok := namedRole(roles, i)
		if !ok {
			left = append(left, i)
		} else if fix := realign(r, i, v.Short); fix != "" {
			fixes = append(fixes, fix)
		}
	}
	if len(fixes) > 0 {
		slices.Sort(fixes)
		diags.AddAttributeWarning(path.Root("vm").AtName("interfaces"), "The VM addresses NICs otherwise than interfaces",
			"The VM names these roles by the MAC given, with other addressing: they are taken over as they are (nothing is attached for them, and the VM does not reboot), and state records interfaces as configured. To align the VM with the configuration, run on it:\n"+strings.Join(fixes, "\n"))
	}
	return left
}

// namedRole is the role of roles that names i: its role, by i's MAC.
func namedRole(roles []installer.NICRole, i installer.Interface) (installer.NICRole, bool) {
	for _, r := range roles {
		if r.Role == i.Role && r.MAC != "" && strings.EqualFold(r.MAC, i.MAC) {
			return r, true
		}
	}
	return installer.NICRole{}, false
}

// realign is "" when r, a role the VM names, has i's addressing, else the line saying how it
// differs and the commands that align it (`network set --role`, then `network confirm` within its
// window). An override (`network set --role`) has i's mode (left out: none) and, static, its
// address; a role without one reports only its addresses: a static i's address among them, none
// for mode none, any for dhcp (none yet: its lease is not there yet, which is no misalignment).
func realign(r installer.NICRole, i installer.Interface, short string) string {
	mode := cmp.Or(i.Mode, "none")
	var same bool
	var now string
	if o := r.Override; o != nil {
		same = o.Mode == mode && (mode != "static" || o.Address == i.Address)
		now = "its override (`network set --role`) is " + strings.TrimSpace(o.Mode+" "+o.Address)
	} else {
		switch mode {
		case "static":
			same = slices.Contains(r.Addresses, i.Address)
		case "none":
			same = len(r.Addresses) == 0
		default:
			same = true
		}
		now = "it has no address"
		if len(r.Addresses) > 0 {
			now = "it has " + strings.Join(r.Addresses, ", ")
		}
	}
	if same {
		return ""
	}
	want := mode
	set := fmt.Sprintf("sudo %s-stow network set --role %s --mode %s", short, i.Role, mode)
	if mode == "static" {
		want += " " + i.Address
		set += " --address " + i.Address
	}
	return fmt.Sprintf("- %s: %s, interfaces gives %s: `%s`, then `sudo %s-stow network confirm` once the VM answers.", i.Role, now, want, set, short)
}

// diskLookup bounds a destroy's check of the registry's disk set, and diskConnect, in seconds,
// its connection to the VM: a VM that does not answer by then (stopped, gone) is not checked.
const (
	diskLookup  = 2 * time.Minute
	diskConnect = 10
)

// vmDiskWarning warns, at the plan of a destroy of the VM's install (its VM destroyed next, so
// before the destroy is approved), when the registry holds no disk set a VM of its release boots
// on its platform: such a VM is not made again until one is published. Asked of the VM's own
// installer (DiskSet), over a connection bounded to diskConnect seconds; a VM that does not answer
// within them is not checked, said in a warning, and a check that fails is said too. Nothing
// without platform, nor without the VM's own key; it never stops the destroy.
func (r *installResource) vmDiskWarning(ctx context.Context, m *installModel, diags *diag.Diagnostics) {
	if !known(m.VM.Platform) || !known(m.VM.HostKeyLine) || r.data == nil {
		return
	}
	v, err := r.vmWithin(m, diskConnect)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, diskLookup)
		defer cancel()
		f := strings.Fields(m.VM.HostKeyLine.ValueString())
		err = v.DiskSet(ctx, m.VM.Platform.ValueString(), m.Registry.ValueString(), strings.Join(f[min(1, len(f)):], " "))
	}
	var refused *installer.RefusedError
	switch {
	case err == nil:
	case sshFailed(err):
		diags.AddWarning("Whether this VM can be made again was not checked",
			fmt.Sprintf("The VM did not answer over SSH within %d seconds (stopped, or gone): check the release has disks for %s before destroying the VM.", diskConnect, m.VM.Platform.ValueString()))
	case errors.As(err, &refused):
		diags.AddWarning("No disk set to make this VM again",
			fmt.Sprintf("%s. Until the release publishes one (stowage ship --disks), a VM destroyed is not made again from its seed.", refused.Reason))
	default:
		diags.AddWarning("Whether this VM can be made again is not known",
			fmt.Sprintf("Its registry's disk sets could not be read through the VM (%v): check the release has disks for %s before destroying the VM.", err, m.VM.Platform.ValueString()))
	}
}

// attach runs `network attach` on the VM for each NIC (attach), then reboots it and follows the
// reboot to the health gate's verdict, from before (the VM's status before): the NICs are named
// from that boot. The status after it; a verdict other than healthy is an *unhealthyAttach, with
// that status: the NICs are attached. A status without boot_id or events, whose reboot could not
// be followed, refuses before any NIC is attached.
func (r *installResource) attach(ctx context.Context, v installer.VM, before *installer.VMStatus, attach []installer.Interface) (*installer.VMStatus, error) {
	if before.BootID == "" || before.Events == nil {
		return nil, fmt.Errorf("the VM reports no boot_id or no event journal (%s), so the reboot that names the NICs could not be followed: nothing was attached. Read `sudo %s-stow status` on the VM",
			cmp.Or(before.EventsError, "no events_error either"), v.Short)
	}
	for _, i := range attach {
		if err := v.NetworkAttach(ctx, i); err != nil {
			return nil, err
		}
	}
	if err := v.Reboot(ctx); err != nil && !errors.Is(err, installer.ErrCut) {
		return nil, err
	}
	st, verdict, err := r.rebooted(ctx, v, before, false)
	if err != nil {
		return nil, err
	}
	if verdict.final.Verdict != installer.VerdictHealthy {
		return st, &unhealthyAttach{detail: verdictDetail("", "", verdict)}
	}
	return st, nil
}

// unhealthyAttach is attach's error when every NIC was attached and the VM came back from the
// reboot naming them unhealthy: the NICs are attached, and state records them so.
type unhealthyAttach struct{ detail string }

func (e *unhealthyAttach) Error() string {
	return "the VM came back unhealthy from the reboot that names its NICs: " + e.detail +
		" The NICs are attached, and state records them: no apply attaches them again; fix what the health gate found"
}

// firstBoot waits until the VM answers over SSH (ssh failing while it boots) and its boot unit has
// done its part (Boot), on its active system — a first boot resets from the recovery system, and
// one from a disk of another release writes this release's OS and reboots, both answering under
// the one-time key meanwhile — and returns the release its OS carries.
func (r *installResource) firstBoot(ctx context.Context, v installer.VM) (*installer.Describe, error) {
	var desc *installer.Describe
	err := r.wait(ctx, "the VM's first boot had not settled (answering over SSH, its boot unit done, on its active system)", resourceTimeout, func() (bool, error) {
		d, err := v.Describe(ctx)
		if err != nil {
			return !sshFailed(err), err // a missing installer included: createVM names it
		}
		// A boot unit that failed (its first install, for one) is the install's to finish, or to
		// say why not; one the boot's reboot cut (another release's OS written) is waited out.
		if err := v.Boot(ctx, d.Name); err != nil && sshFailed(err) {
			return false, err
		}
		st, err := v.Status(ctx)
		if err != nil {
			missing, _ := missingCtl(ctx, v, err)
			return missing, err // no installer of that name: no wait makes one
		}
		switch {
		case st.Boot == "recovery" && st.ResetFailed != "":
			// Kairos does not try its reset again: no later poll answers otherwise.
			return true, fmt.Errorf("the VM's first boot gave up in Kairos's recovery system, which stays: %s", st.ResetFailed)
		case st.Boot == "recovery":
			return false, errors.New("the VM answers from its recovery system")
		case st.RebootPending:
			return false, errors.New("the VM's boot scheduled a reboot")
		}
		again, err := v.Describe(ctx)
		if err != nil {
			return !sshFailed(err), err
		}
		if again.Release != d.Release {
			return false, errors.New("the VM's OS changed under its boot") // read it again
		}
		desc = d
		return true, nil
	})
	return desc, err
}

// learnKey retires the VM's one-time key by its own verb (retire), which gives the VM's own key
// whether or not the retirement took; when it gives none (a step that failed, an installer
// without the verb, a refusal), the key is read alone (the OpenSSH host key file, through the
// connection, whose pinned key vouches for it). own is the key's line, null when neither gave it
// (the connection then keeps accepting the configuration's key alone); seed is the boot during which
// the VM may still serve the one-time key — null once a connection accepting own alone answered,
// else bootID (unreadBoot without one), while which a connection accepts both. warns are
// "summary\ndetail" warnings.
func (r *installResource) learnKey(ctx context.Context, m *installModel, v installer.VM, bootID string) (own, seed types.String, warns []string) {
	own, err := r.retire(ctx, m, v, m.VM.Host.ValueString(), types.StringNull())
	if err == nil {
		return own, types.StringNull(), nil
	}
	if !known(own) && ctx.Err() == nil {
		// No key from the verb (a step that failed, an installer without it, a refusal): read
		// alone, so the connection accepts both keys whatever the verb left.
		if key, rerr := v.HostKey(ctx); rerr == nil {
			own = types.StringValue(m.VM.Host.ValueString() + " " + key)
		} else {
			err = fmt.Errorf("%w; reading it alone: %w", err, rerr)
		}
	}
	if !known(own) {
		return types.StringNull(), types.StringNull(), []string{"The VM's own host key was not read\n" + err.Error() +
			". The connection keeps accepting known_hosts_line alone, which the VM's next boot retires: give its own key there then (check it on the VM's console: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub)."}
	}
	seed = types.StringValue(bootID)
	if bootID == "" {
		seed = types.StringValue(unreadBoot)
	}
	return own, seed, []string{"The VM's one-time host key was not retired\n" + err.Error() + ". " + notRetiredHint(err)}
}

// notRetiredHint is what a failed retirement leaves: the verb refused or failed (the VM serves the
// one-time key until its next boot), or the verb ran and sshd did not answer under the VM's own key
// alone yet (its reload pending: the next update's retry needs no reboot).
func notRetiredHint(err error) string {
	var nr *installer.NotRetiredError
	switch {
	case errors.As(err, &nr):
		return "host-key retired the one-time key's files, but sshd did not take its reload: fix it on the VM as the installer says above (or reboot the VM), then apply a change; until then the connection accepts the one-time key beside the VM's own (seed_boot_id)."
	case errors.Is(err, errNotProved):
		return "host-key retired the one-time key's files, but sshd does not serve the VM's own key alone yet; until then the connection accepts the one-time key beside the VM's own (seed_boot_id), and the next update with a change tries again (no reboot needed)."
	}
	return "The VM serves it until its next boot, which retires it; until then the connection accepts it beside the VM's own (seed_boot_id)."
}

// errNotProved is a retirement whose verb ran but whose proof did not hold.
var errNotProved = errors.New("the retirement is not proved")

// retryRetire tries again a retirement that failed before (seed set): the VM's own key's line
// (own, state's), seed, null on success, and the warning ("summary\ndetail") on a failure again. The VM serving another key than own, state's,
// is an error (stop): the verb has retired the one-time key, and neither key state accepts is
// served any more.
func (r *installResource) retryRetire(ctx context.Context, m *installModel, v installer.VM, own, seed types.String) (types.String, types.String, string, error) {
	_, err := r.retire(ctx, m, v, m.VM.Host.ValueString(), own)
	if err != nil {
		if errors.Is(err, errOtherKey) {
			return own, seed, "", err
		}
		return own, seed, "The VM's one-time host key is still not retired\n" + err.Error() + ". " + notRetiredHint(err) +
			" known_hosts_line stays needed until then.", nil
	}
	// The same key: state's line stays as the plan holds it (its host field is never read, the
	// connection naming the key by its alias).
	return own, types.StringNull(), "", nil
}

// errOtherKey is a VM whose own key is not the one state records.
var errOtherKey = errors.New("the VM serves another key than state's host_key_line")

// sameKey says whether two known_hosts lines hold the same key, whatever their host fields.
func sameKey(a, b string) bool {
	ka, ea := target.ParseKnownHostsLine(a)
	kb, eb := target.ParseKnownHostsLine(b)
	return ea == nil && eb == nil && ka == kb
}

// retire runs the VM's own verb, `sudo -n <short>-stow host-key --json` (Stowage's steps, sshd's
// reload checked: run again, it changes nothing more), whose key is the VM's own: own, `<host>
// <key>`, null when the verb gave none (a refusal, a failure); a reload not taken gives the key
// with the verb's *NotRetiredError. With want known (state's key), another key stops there
// (errOtherKey). A retirement the verb says took is then checked by a connection accepting own
// alone, before state drops the one-time key from every connection: sshd's reload is a signal
// systemctl does not wait out (its listener reopens after), so six attempts a poll apart (a
// minute in production) within the resource's timeout let the next connection find it; the one
// observation that own alone is served, else errNotProved.
func (r *installResource) retire(ctx context.Context, m *installModel, v installer.VM, host string, want types.String) (types.String, error) {
	key, err := v.RetireHostKey(ctx)
	var nr *installer.NotRetiredError
	if err != nil && !errors.As(err, &nr) {
		if ctx.Err() != nil {
			return types.StringNull(), fmt.Errorf("retiring the one-time host key: cut when %s ran out", resourceTimeout)
		}
		return types.StringNull(), err
	}
	own := types.StringValue(host + " " + key)
	if known(want) {
		if _, perr := target.ParseKnownHostsLine(want.ValueString()); perr != nil {
			return own, fmt.Errorf("state's host_key_line: %w", perr)
		}
	}
	if known(want) && !sameKey(own.ValueString(), want.ValueString()) {
		return own, fmt.Errorf("%w: host-key printed %s, and state records %s. The one-time key is retired or being retired: give known_hosts_line %q (the key the VM's own verb printed, through the key state accepts), then `tofu state rm` this resource and `tofu import` it as <host>/<short>-stow", errOtherKey, key, want.ValueString(), own.ValueString())
	}
	if nr != nil {
		return own, err // its key known, the one-time key still served: no check of the own key alone
	}
	only := *m
	vmo := *m.VM
	vmo.HostKeyLine, vmo.SeedBootID = own, types.StringNull()
	only.VM = &vmo
	ov, err := r.vm(&only)
	if err != nil {
		return own, fmt.Errorf("%w: %w", errNotProved, err)
	}
	var last error
	for i := 0; i < 6; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(r.data.Poll):
			}
		}
		if ctx.Err() != nil {
			return own, fmt.Errorf("%w: the VM did not answer under its own key alone when %s ran out", errNotProved, resourceTimeout)
		}
		if _, last = ov.HostKey(ctx); last == nil {
			return own, nil
		}
	}
	return own, fmt.Errorf("%w: the VM did not answer under its own key alone in its six polls (a minute); the last attempt: %w", errNotProved, last)
}

// vmUpdate applies a release change to a VM in DESIGN § 13's order, after the re-run of the
// running release: the upgrade (or the rollback), its reboot followed, then the re-run of what the
// new release adds. A failure saves the release the VM runs.
type vmUpdate struct {
	r *installResource
	v installer.VM
	// tried: this update's key step tried a retirement that failed; release() does not again.
	tried bool
	diags *diag.Diagnostics
	save  func(*installModel) bool
}

// release moves the VM from st to desired — by `rollback` when back is set, else `upgrade
// --version` given the inputs and secret inputs of later, which the boot into desired takes when
// the VM's `updates` says it does (takes_inputs: neither the VM's OS nor desired built before
// Stowage 0.43.0; else they stay in later) — then gives what later still holds
// to the new release's re-run, and saves final (the plan's) filled from the VM's status; on a
// failure it saves was, the state before, with the VM's status.
func (u *vmUpdate) release(ctx context.Context, final, was *installModel, st *installer.VMStatus, desired string, later installer.Change, back bool) {
	verb := "Upgrading the VM to " + desired
	if back {
		verb = "Rolling the VM back to " + desired
	}
	var res *installer.Result
	var err error
	given := later.Inputs // checked against desired once it runs
	if back {
		res, err = u.v.Rollback(ctx)
	} else {
		added := installer.Change{Inputs: later.Inputs, SecretInputs: later.SecretInputs}
		if len(added.Inputs)+len(added.SecretInputs) > 0 {
			takes, err := u.v.TakesInputs(ctx, desired)
			if err != nil {
				addInstallerError(u.diags, "Reading the VM's updates", err)
				return
			}
			if !takes {
				added = installer.Change{}
			}
		}
		res, err = u.v.Upgrade(ctx, desired, added)
		if errors.Is(err, installer.ErrNoUpgradeInputs) {
			added = installer.Change{}
			res, err = u.v.Upgrade(ctx, desired, added)
		}
		if len(added.Inputs)+len(added.SecretInputs) > 0 {
			later.Inputs, later.SecretInputs = nil, nil // the upgrade gave them
		}
	}
	if err != nil && !errors.Is(err, installer.ErrCut) {
		addInstallerError(u.diags, verb, err)
		return
	}
	var now *installer.VMStatus
	var verdict *bootVerdict
	reboots := !(res != nil && res.Result == "nothing")
	if !reboots {
		if now, err = u.v.StatusOf(ctx, true); err != nil {
			addInstallerError(u.diags, "Reading the VM", err)
			return // state stays as before the upgrade
		}
	} else if now, verdict, err = u.r.rebooted(ctx, u.v, st, back); err != nil {
		u.diags.AddError(verb, err.Error())
		return // state stays as before the upgrade: the response holds it
	}
	seed, line := was.VM.SeedBootID, was.VM.HostKeyLine
	switch {
	case !known(seed):
	case reboots:
		seed = types.StringNull() // the followed reboot retired the one-time key
	case !u.tried:
		// No reboot: the retirement the key step left to it is tried here.
		var warn string
		var stop error
		if line, seed, warn, stop = u.r.retryRetire(ctx, was, u.v, was.VM.HostKeyLine, seed); stop != nil {
			// The upgrade had nothing to do: the release is the one state holds.
			u.diags.AddError("The VM's own host key changed", stop.Error())
			return
		} else if warn != "" {
			summary, detail, _ := strings.Cut(warn, "\n")
			u.diags.AddWarning(summary, detail)
		}
	}
	if !seed.Equal(was.VM.SeedBootID) {
		// Retired: state and the connection accept the VM's own key alone from here on.
		keyed, v, err := u.r.rekey(*was, line, seed)
		*was = keyed
		if err != nil {
			u.diags.AddError("Cannot reach the VM", err.Error())
			u.failed(was, now, "", desired, nil, false)
			return
		}
		u.v = v
	}
	runs := ""
	if now.Installed != nil {
		runs = now.Installed.Version
	}
	if runs != desired || (verdict != nil && verdict.final.Verdict != installer.VerdictHealthy) {
		u.failed(was, now, verb, desired, verdict, !later.Empty())
		return
	}
	if len(given) > 0 || !later.Empty() {
		// An input the new release declares secret goes in secret_inputs_wo: refused before the
		// re-run puts it on a command line, and after an upgrade that gave it, so the next plan
		// moves it.
		next, err := u.v.Describe(ctx)
		if err != nil {
			addInstallerError(u.diags, "Reading the VM's installer of "+desired, err)
			u.failed(was, now, "", desired, nil, false)
			return
		}
		refuseSecretInputs(u.diags, given, next)
		if u.diags.HasError() {
			u.failed(was, now, "", desired, nil, false)
			return
		}
	}
	if !later.Empty() {
		if _, err := u.v.Install(ctx, installer.VMInstall{Change: later}); err != nil {
			addInstallerError(u.diags, "Giving the VM what "+desired+" adds", err)
			u.failed(was, now, "", desired, nil, false)
			return
		}
		// A status that cannot be read now keeps the one read after the reboot: the release the
		// VM runs, and the re-run's inputs, which state then records.
		if after, err := u.v.StatusOf(ctx, true); err != nil {
			addInstallerError(u.diags, "Reading the VM", err)
		} else {
			now = after
		}
	}
	u.r.vmFill(final, was)
	if now != nil {
		u.r.noteRecorded(now.Values)
	}
	applyVMStatus(final, now, false)
	vm := *final.VM
	vm.SeedBootID = was.VM.SeedBootID // null once a followed reboot retired the one-time key
	final.VM = &vm
	retireSeed(final, now)
	u.save(final)
}

// failed saves was with the release the VM runs and says why it is not desired, or runs it
// unhealthy: the health gate's verdicts when a reboot was followed (v). verb empty: the error is
// said already. adds: the new release adds what a re-run gives (inputs, domains, ports).
func (u *vmUpdate) failed(was *installModel, st *installer.VMStatus, verb, desired string, v *bootVerdict, adds bool) {
	m := *was
	if st != nil {
		applyVMStatus(&m, st, true)
		retireSeed(&m, st)
	}
	if verb != "" {
		runs := "no release"
		if st != nil && st.Installed != nil {
			runs = st.Installed.Version
		}
		next := " State holds the release the VM runs: the next plan proposes the change again."
		if runs == desired {
			next = " State holds " + desired + ", the release the VM runs, unhealthy: no plan changes that; fix what the gate found."
			if adds {
				next += " What " + desired + " adds (inputs, domains, ports) was not given: the next plan proposes that re-run."
			}
		}
		u.diags.AddError(verb+" failed", verdictDetail(runs, desired, v)+next+" `<short>-stow status` and `events` on the VM tell more.")
	}
	u.save(&m)
}

// verdictDetail says why the VM runs runs, not desired, or runs it unhealthy, from the gate's
// verdicts v (nil: no reboot was followed); desired empty: the verdicts alone, a wait that ran out.
func verdictDetail(runs, desired string, v *bootVerdict) string {
	if v == nil {
		return fmt.Sprintf("The VM runs %s, not %s.", runs, desired)
	}
	var parts []string
	for _, w := range v.wentBack {
		why := cmp.Or(w.Reason, "no reason given")
		if w.BootID == v.boot {
			parts = append(parts, fmt.Sprintf("The VM's health gate did not keep %s's boot (%s), and the VM still runs that boot: its next boot goes to the passive system.", eventRelease(w), why))
		} else {
			parts = append(parts, fmt.Sprintf("The VM's health gate did not keep %s's boot (%s): the VM went back to the passive system.", eventRelease(w), why))
		}
	}
	if len(v.wentBack) == 0 && v.fallback && !v.back {
		parts = append(parts, "Kairos booted the passive system: the upgrade's boot failed before the health gate judged it (its console or `journalctl -b -1` on the VM tells why).")
	}
	switch f := v.final; {
	case f.Verdict == installer.VerdictKeptUnhealthy && f.OnTrial != nil && *f.OnTrial:
		parts = append(parts, fmt.Sprintf("The VM's health gate found %s's boot unhealthy: %s. It kept that boot, as going back is refused: %s.", eventRelease(f), cmp.Or(f.Reason, "no reason given"), cmp.Or(f.Kept, "no reason given")))
	case f.Verdict == installer.VerdictKeptUnhealthy:
		parts = append(parts, fmt.Sprintf("The VM's health gate found %s's boot unhealthy: %s.", eventRelease(f), cmp.Or(f.Reason, "no reason given")))
	}
	if desired != "" {
		parts = append(parts, fmt.Sprintf("After its reboot the VM runs %s (wanted: %s).", runs, desired))
	}
	return strings.Join(parts, " ")
}

// eventRelease is the version an event names, "a release" when it names none.
func eventRelease(e installer.Event) string {
	if e.Release == nil {
		return "a release"
	}
	return e.Release.Version
}

func (r *installResource) readVM(ctx context.Context, m *installModel, resp *resource.ReadResponse) {
	if m.VM.KnownHostsLine.IsNull() && !known(m.VM.HostKeyLine) {
		return // an imported state: the configuration gives the connection, at the first apply
	}
	timeout, diags := m.Timeouts.Read(ctx, readTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	v, err := r.vm(m)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the VM", err.Error())
		return
	}
	st, err := v.StatusOf(ctx, true)
	if err != nil {
		if sshFailed(err) {
			unreachable(&resp.Diagnostics, "VM", m.VM.Host.ValueString(), vmKeyWay(m), err)
			return
		}
		addInstallerError(&resp.Diagnostics, "Reading the VM", renamed(ctx, v, err))
		return
	}
	if st.Installed == nil && !st.Installing && st.InstallFailed == "" {
		resp.State.RemoveResource(ctx) // no install: the next apply installs again
		return
	}
	applyVMStatus(m, st, true)
	retireSeed(m, st)
	r.readBack(m, st.Values)
	r.readBackup(ctx, m, &resp.Diagnostics)
	readSSHAllow(ctx, v, m, &resp.Diagnostics)
	resp.Diagnostics.Append(dropUnrecorded(ctx, m, &st.Status)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
}

// updateVM runs within ctx's deadline, the update's timeout (Update).
func (r *installResource) updateVM(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse, plan, state *installModel) {
	var secretsWO types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	secrets := stringMap(ctx, secretsWO, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	save := func(m *installModel) bool {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
		return !resp.Diagnostics.HasError()
	}
	p := r.vmPrepare(ctx, plan, state, secrets, false, &resp.Diagnostics)
	if p == nil {
		return
	}
	conn, v, st, step, u := p.conn, p.v, p.st, p.step, p.u
	now, later := p.now, p.later // what the running release's re-run, and the new release's, are given
	// The NICs this update attaches: an imported state's, those the VM does not name yet.
	attach := withPending(attachments(plan.VM.Interfaces, state.VM.Interfaces), plan.VM.Interfaces, pendingRoles(ctx, req.Private))
	if imported(state) && len(attach) > 0 {
		attach = newNICs(ctx, v, attach, &resp.Diagnostics)
	}
	if step != stay && len(attach) > 0 {
		// An imported state's release change is told here (its version is null at plan).
		interfacesWithVersion(&resp.Diagnostics)
		return
	}
	var err error
	desired := plan.VM.Version.ValueString()
	// The keys, before anything else changes on the VM: its own, when state has none yet (it
	// could not be read at create; an imported state), learnt as a create learns it — read, the
	// one-time key retired; or the retirement a create could not make, tried again. Either is
	// saved at once: the VM may no longer serve the key state accepted. own and seed are what
	// every state this update saves holds.
	own, seed := state.VM.HostKeyLine, state.VM.SeedBootID
	var warns []string
	switch {
	case !known(own):
		own, seed, warns = r.learnKey(ctx, &conn, v, st.BootID)
	case known(seed) && seed.ValueString() != unreadBoot && st.BootID != "" && st.BootID != seed.ValueString():
		seed = types.StringNull() // a later boot retired it
	case known(seed) && step == stay:
		// A release change's reboot retires it anyway; a re-run alone tries again.
		var warn string
		var stop error
		if own, seed, warn, stop = r.retryRetire(ctx, &conn, v, own, seed); stop != nil {
			resp.Diagnostics.AddError("The VM's own host key changed", stop.Error())
			return
		} else if warn != "" {
			warns = append(warns, warn)
		}
	}
	for _, w := range warns {
		summary, detail, _ := strings.Cut(w, "\n")
		resp.Diagnostics.AddWarning(summary, detail)
	}
	if !own.Equal(state.VM.HostKeyLine) || !seed.Equal(state.VM.SeedBootID) {
		learnt := *state
		learnt.VM = withKeys(planConn(state.VM, plan.VM), own, seed)
		if !save(&learnt) {
			return
		}
		if _, v, err = r.rekey(conn, own, seed); err != nil {
			resp.Diagnostics.AddError("Cannot reach the VM", err.Error())
			return
		}
	}
	if step != stay && !known(own) {
		resp.Diagnostics.AddError("The VM's own host key is not known",
			"It could not be read (the warning says why). The release change's reboot would retire the key this connection accepts, and could not be followed: nothing was run. Give known_hosts_line the VM's own key (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on its console), then apply again.")
		return
	}
	// was is state as the VM stands before the release change: after the re-run, the plan's
	// inputs it took, the release unchanged.
	was := *state
	was.VM = withKeys(planConn(state.VM, plan.VM), own, seed)
	if login := r.login(plan, state, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	} else if login != "" {
		if err := v.RegistryLogin(ctx, login); err != nil {
			addInstallerError(&resp.Diagnostics, "Giving the VM its registry login", err)
			return
		}
	}
	if !now.Empty() {
		if _, err := v.Install(ctx, installer.VMInstall{Change: now}); err != nil {
			addInstallerError(&resp.Diagnostics, "Reconfiguring the VM", err)
			return
		}
		if step != stay {
			p := u.partial(ctx, now)
			if resp.Diagnostics.HasError() {
				return
			}
			p.VM = withKeys(*p.VM, own, seed)
			p.VM.Version = state.VM.Version
			r.vmFill(p, state)
			applyVMStatus(p, st, false)
			p.Status = state.Status
			if !save(p) {
				return
			}
			was = *p
		}
	}
	final := *plan
	final.VM = withKeys(*plan.VM, own, seed) // whatever the plan held (planVM)
	if step == stay {
		after, err := v.StatusOf(ctx, true)
		if err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the VM", err)
			return
		}
		r.noteRecorded(after.Values)
		var sick *unhealthyAttach // the NICs attached, the VM's boot unhealthy
		if len(attach) == 0 {
			resp.Diagnostics.Append(setPending(ctx, resp.Private, nil)...)
		} else {
			// The re-run's state first: an attach that fails leaves the roles it attaches as
			// state had them (and the pending ones pending), so the next plan attaches them again;
			// the others — an imported VM's roles taken over — as planned, never attached again.
			done := final
			vmd := *final.VM
			vmd.Interfaces = notAttached(plan.VM.Interfaces, state.VM.Interfaces, attach)
			done.VM = &vmd
			r.vmFill(&done, state)
			applyVMStatus(&done, after, false)
			retireSeed(&done, after)
			if !save(&done) {
				return
			}
			attached, err := r.attach(ctx, v, after, attach)
			if err != nil && !errors.As(err, &sick) {
				addInstallerError(&resp.Diagnostics, "Attaching the VM's NICs", consoleHint(err, plan.VM.Platform))
				return
			}
			// Attached, whatever the verdict: recorded as planned, nothing pending.
			after = attached
			resp.Diagnostics.Append(setPending(ctx, resp.Private, nil)...)
		}
		r.vmFill(&final, state)
		applyVMStatus(&final, after, false)
		retireSeed(&final, after)
		if save(&final) && sick != nil {
			addInstallerError(&resp.Diagnostics, "Attaching the VM's NICs", consoleHint(sick, plan.VM.Platform))
		}
		return
	}
	// learnKey retired nothing in this update (state had no key, and a seed came back): release()
	// does not try again in the same apply. The key step's own retry runs only without a release
	// change, so release() never follows it.
	tried := !known(state.VM.HostKeyLine) && known(seed)
	vu := &vmUpdate{r: r, v: v, tried: tried, diags: &resp.Diagnostics, save: save}
	vu.release(ctx, &final, &was, st, desired, later, step == rollback)
}

// vmPrep is what an update of a VM reads and decides before it changes anything: the apply's,
// and the plan's dry run's.
type vmPrep struct {
	conn       installModel // the plan's connection, with the keys state learnt
	v          installer.VM
	st         *installer.VMStatus
	step       releaseStep
	u          *update
	now, later installer.Change
	running    *installer.Describe // the VM's installer, describe --json
}

// daemonGrace bounds the reads of a status run that fails (exit 1) with no sign of a boot before
// it: about two minutes at the 10 s poll.
const daemonGrace = 12

// answers reads the VM's status (with its values when imported), again, within its bound, while it
// shows what a restart in this apply leaves (the root's power resource,
// which this one depends on, may restart the VM to give it a NIC or change its memory): ssh
// failing as a VM down or booting fails (bootingSSH: no route, refused, timed out, reset), a
// status run that fails (exit 1: early in a boot, sshd answers before the VM's daemon does; with no
// ssh failure before it, for daemonGrace reads), a
// boot not settled (boot_settled false: the install is the boot's until its unit and the health
// gate ended; a status that does not say, from an OS built with an older Stowage, is taken as it
// is). Anything else stops at once, as no later try answers otherwise: ssh's other failures (a
// refused login, another host key, a host that does not resolve), a refusal, no installer (the
// rename route), a status this provider cannot read. A VM that never answers — off, or a wrong
// host or port — waits out the bound. atPlan: plan_dry_run's read, bounded by planDryRunTimeout,
// which the timeout's message names; else the resource's timeouts block's.
func (r *installResource) answers(ctx context.Context, v installer.VM, imported, atPlan bool) (*installer.VMStatus, error) {
	what, bound := "the VM did not answer over SSH, or its boot did not settle,", resourceTimeout
	if atPlan {
		bound = fmt.Sprintf("plan_dry_run's bound (%d minutes)", int(planDryRunTimeout.Minutes()))
	}
	var st *installer.VMStatus
	booting, failures := false, 0
	err := r.wait(ctx, what, bound, func() (bool, error) {
		s, err := v.StatusOf(ctx, imported)
		var failed *installer.FailedError
		switch {
		case err == nil:
		case bootingSSH(err):
			booting = true
			return false, err
		case errors.As(err, &failed) && failed.Exit == 1:
			// Past a boot, until the daemon answers; else for daemonGrace reads, as one that never
			// answers is no boot.
			failures++
			return !booting && failures > daemonGrace, err
		default:
			return true, err
		}
		st = s
		if s.BootSettled != nil && !*s.BootSettled {
			return false, errors.New("the VM's boot is not settled (boot_settled is false: its install unit or health gate still runs, or their state cannot be read; `systemctl status <name> <name>-health` on the VM tells)")
		}
		return true, nil
	})
	return st, err
}

// bootingSSH says whether err is ssh's own failure as a VM down or booting gives it: no route to
// it, its port closed or not answering, the connection cut. A refused login, another host key or
// a host name that does not resolve is never one.
func bootingSSH(err error) bool {
	if !sshFailed(err) || sshRefused(err) {
		return false
	}
	return errors.Is(err, installer.ErrCut) || sshBooting.MatchString(err.Error())
}

// sshBooting matches ssh's messages of a host down or booting.
var sshBooting = regexp.MustCompile(`(?i)no route to host|connection refused|connection timed out|operation timed out|connection reset|closed by remote host|connection closed|network is unreachable|host is down`)

// sshRefused says whether err is ssh's own failure on a refused login or another host key served:
// what unreachable tells apart, and no wait mends.
func sshRefused(err error) bool {
	if err == nil || !sshFailed(err) {
		return false
	}
	msg := err.Error()
	return sshLoginRefused.MatchString(msg) || hostKeyFailed(msg)
}

// vmKeyWay is unreachable's way on for a VM that serves another host key than m, a state, accepts.
func vmKeyWay(m *installModel) string {
	if !known(m.VM.HostKeyLine) {
		// No own key recorded: the connection accepts state's known_hosts_line alone
		// (installResource.vm), which an apply that does not read saves.
		return "A read accepts the key in state's known_hosts_line alone (the VM's own key is not recorded yet): " +
			"check the new key on the VM's console, give it in known_hosts_line, then `tofu apply -refresh=false`, which saves a change of the connection alone without running anything on the VM."
	}
	return "A read accepts the VM's own key state holds (host_key_line), and the seed's line beside it while seed_boot_id is set. " +
		"A VM made again from its seed is a new install: `tofu state rm` this resource, and the next apply installs it under the seed's key. " +
		"A VM whose own key changed: check the new key on its console, give it in known_hosts_line, " +
		"then `tofu state rm` this resource and `tofu import` it again (`" + vmID(m) + "`)."
}

// vmPrepare reads the VM and decides the update's step as the apply does, with every refusal it
// makes before it runs anything: nil on one (said in diags). atPlan: plan_dry_run's.
func (r *installResource) vmPrepare(ctx context.Context, plan, state *installModel, secrets map[string]string, atPlan bool, diags *diag.Diagnostics) *vmPrep {
	// The connection is the plan's, with the keys state learnt.
	conn := *plan
	vmc := *plan.VM
	vmc.HostKeyLine, vmc.SeedBootID = state.VM.HostKeyLine, state.VM.SeedBootID
	conn.VM = &vmc
	v, err := r.vm(&conn)
	if err != nil {
		diags.AddError("Cannot reach the VM", err.Error())
		return nil
	}
	// The read waits for a VM another resource of the apply restarts, or that a boot still brings
	// up; at plan too (plan_dry_run's, of a release change, which the host daemon refuses until the
	// boot settled; OpenTofu plans again at apply, perhaps right after such a restart).
	st, err := r.answers(ctx, v, imported(state), atPlan)
	if sshRefused(err) {
		unreachable(diags, "VM", plan.VM.Host.ValueString(), vmKeyWay(state), err)
		return nil
	}
	if errors.As(err, new(waitRanOut)) {
		err = consoleHint(err, plan.VM.Platform) // the boot it waited for shows there
	}
	if err != nil {
		addInstallerError(diags, "Reading the VM", renamed(ctx, v, err))
		return nil
	}
	if st.Installed == nil {
		diags.AddError("No install to change", "The VM holds no install: `tofu state rm` this resource and apply to install again.")
		return nil
	}
	running, err := v.Describe(ctx)
	if err != nil {
		addInstallerError(diags, "Reading the VM's installer", err)
		return nil
	}
	step := vmStep(plan, state, st, diags)
	if step == rollback && imported(state) {
		importedRollback(diags, path.Root("vm").AtName("version"), "version", st.Installed.Version+", the release it runs", "previous.version")
	}
	if diags.HasError() {
		return nil
	}
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: diags}
	u.adopt(ctx, &st.Status)
	if diags.HasError() {
		return nil
	}
	now, later := vmSplit(ctx, u, running, step == upgrade)
	refuseSecretInputs(diags, stringMap(ctx, plan.Inputs, diags), running)
	if diags.HasError() {
		return nil
	}
	if step == rollback && !now.Empty() {
		vmRollbackAlone(diags, "inputs, secret inputs, tls, domains or ports")
		return nil
	}
	// An imported state records the configuration's channel, first-install only from then on:
	// it must be the channel the VM follows.
	if imported(state) && !vmChannelCheck(diags, plan.VM.Channel, st.Channel, "Nothing was run.") {
		return nil
	}
	if step != stay && st.BootID == "" {
		diags.AddError("The VM reports no boot_id",
			"Its status has no boot_id, so the release change's reboot could not be followed: nothing was run. Read `<short>-stow status` on the VM.")
		return nil
	}
	if step != stay && st.Events == nil {
		diags.AddError("The VM reports no event journal",
			fmt.Sprintf("Its status has no events (%s), so the health gate's verdict on the release change's reboot could not be read: nothing was run. Read `<short>-stow status` on the VM.", cmp.Or(st.EventsError, "no events_error either")))
		return nil
	}
	return &vmPrep{conn: conn, v: v, st: st, step: step, u: u, now: now, later: later, running: running}
}

// vmStep is the release step an update of a VM makes from st, its status (installed): an error in
// diags for a rollback without allow_rollback.
func vmStep(plan, state *installModel, st *installer.VMStatus, diags *diag.Diagnostics) releaseStep {
	desired := plan.VM.Version.ValueString()
	switch {
	// The release changes only when the plan changes it: a VM that runs another release than
	// state says (a refresh skipped) is the next refresh's to show.
	case plan.VM.Version.Equal(state.VM.Version) && !imported(state):
	case st.Installed.Version == desired:
	case st.Previous != nil && st.Previous.Version == desired:
		if rollbackRefused(plan.AllowRollback) {
			diags.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
				fmt.Sprintf("version is %s, the release the VM's last upgrade replaced: reaching it is `rollback`, which reboots the VM into the previous release (every input keeps its value). Set allow_rollback = true to roll back.", desired))
			return stay
		}
		return rollback
	default:
		return upgrade
	}
	return stay
}

// rekey is m with the keys given, and the driver accepting them: every change of the keys an
// update makes goes through it, so no connection outlives the keys it was built with.
func (r *installResource) rekey(m installModel, own, seed types.String) (installModel, installer.VM, error) {
	m.VM = withKeys(*m.VM, own, seed)
	v, err := r.vm(&m)
	return m, v, err
}

// withKeys is v with the VM's keys an update holds: its own key and seed_boot_id.
func withKeys(v vmModel, own, seed types.String) *vmModel {
	v.HostKeyLine, v.SeedBootID = own, seed
	return &v
}

// shortNameFixed refuses a short_name other than state's: state names the installer the VM's
// seed (or the import ID) gave, and the provider runs no other.
func shortNameFixed(diags *diag.Diagnostics, was, now string) {
	firstInstallOnlyFix(diags, path.Root("vm").AtName("short_name"), "short_name", was, now,
		fmt.Sprintf("Set it back to %q. %s", was, renameFix))
}

// renameFix is what to do when the VM runs another installer than state names.
const renameFix = "When the VM runs another installer (a release that renamed it, an import under a wrong name; `ls /usr/sbin/*-stow` on the VM names it): " +
	"give known_hosts_line the VM's own key — state's host_key_line (when seed_boot_id is set too, the VM still serves its one-time key: reboot the VM first, which retires it), " +
	"else the key checked on the VM's console (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub) —, then `tofu state rm` this resource and `tofu import` it as <host>/<short>-stow."

// missingCtl says, for err, a failed run of v's installer, whether the VM's shell finds no such
// installer (HasCtl: whatever the shell, its language or the exit it gave a missing command —
// 127, or tcsh's 1); ssh's own failure is not looked into. probe is the probe's own failure.
func missingCtl(ctx context.Context, v installer.VM, err error) (missing bool, probe error) {
	var f *installer.FailedError
	if !errors.As(err, &f) || f.Exit == 0 || f.Exit == installer.SSHFailure || ctx.Err() != nil {
		return false, nil
	}
	has, perr := v.HasCtl(ctx)
	return perr == nil && !has, perr
}

// probeNote is err, with the probe's own failure when missingCtl could not check.
func probeNote(err error, v installer.VM, probe error) error {
	if probe != nil {
		return fmt.Errorf("%w\n\n(Whether the VM has %s-stow could not be checked: %v.)", err, v.Short, probe)
	}
	return err
}

// renamed adds renameFix to a status read that found no installer v names on the VM, and says
// when that could not be checked.
func renamed(ctx context.Context, v installer.VM, err error) error {
	missing, probe := missingCtl(ctx, v, err)
	if missing {
		return fmt.Errorf("%w\n\nThe VM has no %s-stow. %s", err, v.Short, renameFix)
	}
	return probeNote(err, v, probe)
}

// vmChannelCheck says whether the configured channel is the VM's (or left out); else it is an
// error ending with after, naming the channel to set.
func vmChannelCheck(diags *diag.Diagnostics, configured types.String, vmChannel, after string) bool {
	if !known(configured) || configured.ValueString() == vmChannel {
		return true
	}
	detail := fmt.Sprintf("The VM follows the channel %q (its seed's); channel says %q, which state would record, first-install only from then on. Set channel to %q, or leave it out. %s", vmChannel, configured.ValueString(), vmChannel, after)
	if vmChannel == "" {
		detail = fmt.Sprintf("The VM's status reports no channel; channel says %q, which state would record. Leave channel out. %s", configured.ValueString(), after)
	}
	diags.AddAttributeError(path.Root("vm").AtName("channel"), "channel is not the VM's", detail)
	return false
}

// planConn is s (state's vm block) with p's (the plan's) connection: the one this update reaches
// the VM by, which a state saved on its way keeps — an imported state holds none.
func planConn(s, p *vmModel) vmModel {
	v := *s
	v.Host, v.Port, v.User, v.PrivateKeyFile, v.KnownHostsLine, v.ShortName = p.Host, p.Port, p.User, p.PrivateKeyFile, p.KnownHostsLine, p.ShortName
	return v
}

// vmSplit sorts an update's changes between the re-run of the running release (now) and what goes
// with the release that adds it (later): an input or a secret input the running release does not
// declare, which the upgrade gives the boot into the new release (the re-run after its reboot on
// an OS whose upgrade takes no input), and the port or domain of a route state does not record,
// which the re-run after the reboot gives; everything else goes now, where the installer refuses
// what it does not take.
func vmSplit(ctx context.Context, u *update, running *installer.Describe, upgrading bool) (now, later installer.Change) {
	plan, state := u.plan, u.was()
	now = installer.Change{Inputs: map[string]string{}, SecretInputs: map[string]string{}, Domains: map[string]string{}, Ports: map[string]string{}}
	later = installer.Change{Inputs: map[string]string{}, SecretInputs: map[string]string{}, Domains: map[string]string{}, Ports: map[string]string{}}
	was := stringMap(ctx, state.Inputs, u.diags)
	given := stringMap(ctx, plan.Inputs, u.diags)
	for k, v := range given {
		if old, ok := was[k]; ok && old == v {
			continue
		}
		if upgrading && !running.Declares(k) {
			later.Inputs[k] = v
		} else {
			now.Inputs[k] = v
		}
	}
	bump := !plan.SecretInputsWOVersion.Equal(state.SecretInputsWOVersion)
	for _, k := range slices.Sorted(maps.Keys(was)) {
		// An input moved to secret_inputs_wo is not emptied: its answer goes with a new
		// secret_inputs_wo_version, which the plan requires (planMovedSecrets).
		if _, secret := u.secrets[k]; secret {
			continue
		}
		if _, ok := given[k]; !ok && running.Declares(k) {
			now.Inputs[k] = ""
		}
	}
	for k, v := range u.secrets {
		switch {
		case u.adopted != nil && slices.Contains(u.recorded, k):
			// Taken over: a secret the install records is kept, not compared.
		case upgrading && !running.Declares(k):
			later.SecretInputs[k] = v
		case u.adopted != nil:
			now.SecretInputs[k] = v // one the install does not record yet
		case bump:
			now.SecretInputs[k] = v
		}
	}
	if known(plan.TLS) && !plan.TLS.Equal(state.TLS) {
		now.TLS = plan.TLS.ValueString()
	}
	now.AcmeCA, now.AcmeCARoot = acmeChange(plan, state)
	for _, c := range []struct {
		plan, state types.Map
		now, later  map[string]string
		ports       bool
	}{{plan.Domains, state.Domains, now.Domains, later.Domains, false}, {plan.Ports, state.Ports, now.Ports, later.Ports, true}} {
		var oldV, newV map[string]string
		if c.ports {
			oldV, newV = portMap(ctx, c.state, u.diags), portMap(ctx, c.plan, u.diags)
		} else {
			oldV, newV = stringMap(ctx, c.state, u.diags), stringMap(ctx, c.plan, u.diags)
		}
		for k, v := range newV {
			old, ok := oldV[k]
			switch {
			case ok && old == v:
			// A key state never held goes after the upgrade; an imported state's, when the
			// install records its values (adopted), only for a route the running release lacks.
			case upgrading && !ok && (u.adopted != nil && !running.HasRoute(k) && !hasL4Route(running, k) || u.adopted == nil && !imported(state)):
				c.later[k] = v
			default:
				c.now[k] = v
			}
		}
	}
	return now, later
}

// hostName is a host name an import ID may give: letters, digits, dots and hyphens.
var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`)

// importVM reads an import ID `<host>/<short>-stow`: the VM and the installer it runs. The
// configuration gives the connection — known_hosts_line the VM's own key — at the first apply.
func (r *installResource) importVM(ctx context.Context, id string, resp *resource.ImportStateResponse) {
	host, file, ok := strings.Cut(id, "/")
	short, isStow := strings.CutSuffix(file, "-stow")
	if !ok || !isStow || !installer.ValidShortName(short) || target.ValidateHostUser(host, "u") != nil || net.ParseIP(host) == nil && !hostName.MatchString(host) {
		resp.Diagnostics.AddError("Import ID", fmt.Sprintf("%q: want <host>/<short>-stow, the VM's address and the installer it runs (`ls /usr/sbin/*-stow` on it)", id))
		return
	}
	null := types.StringNull()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vm"), &vmModel{
		Host: types.StringValue(host), Port: types.Int64Null(), User: null, PrivateKeyFile: null, KnownHostsLine: null,
		Version: null, Channel: null, ShortName: types.StringValue(short), HostKeyLine: null, SeedBootID: null,
		Platform: null, Interfaces: types.MapNull(interfaceType), SSHAllow: types.SetNull(types.StringType)})...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("short_name"), short)...)
	for a, v := range map[string]bool{"deletion_protection": true, "delete_data": false, "allow_rollback": false} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(a), v)...)
	}
}
