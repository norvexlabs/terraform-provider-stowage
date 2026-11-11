package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// vmAttributes are the vm block's: the SSH connection, the release, and what the provider learns.
func vmAttributes() map[string]schema.Attribute {
	vm := sshAttributes("The VM's address, reached over SSH by the system `ssh` binary (the VM's own resource's address output); required in this block.")
	vm["known_hosts_line"] = schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "The key the VM serves until the provider has read its own: `stowage_vm_seed`'s `known_hosts_line` (its one-time key) for a VM the provider installs, the VM's own key for an imported one. " +
			"That key alone is accepted (`StrictHostKeyChecking=yes`), never one learnt on first contact. Required to install, and for an imported state's first apply; once `host_key_line` is known it may be left out.",
	}
	vm["version"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The release the VM runs; required in this block. A change upgrades the VM (`sudo -n <short>-stow upgrade --version V --yes --json`, then its reboot followed), or rolls it back to `previous.version` with `allow_rollback`."}
	vm["channel"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The channel the VM follows, which its seed's `channel` set: a create, and an imported state's first apply, refuse one other than the VM's (state would record it). First-install only."}
	vm["short_name"] = schema.StringAttribute{Optional: true, MarkdownDescription: "The installer's short name, `stowage_vm_seed`'s `short_name`: the VM runs `<short_name>-stow`; required in this block."}
	vm["host_key_line"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "The VM's own SSH host key as a `known_hosts` line, read through the one-time key once its first install is done (`/etc/ssh/ssh_host_ed25519_key.pub`): every later connection accepts it.",
		PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
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
// (vm.host_key_line); a create, and a first apply of an imported state, refuse its absence.
func validateVM(m *installModel, diags *diag.Diagnostics) {
	v := m.VM
	line := v.KnownHostsLine
	if line.IsNull() {
		line = types.StringUnknown() // neither required nor parsed here
	}
	validateSSH(path.Root("vm"), v.Host, v.User, line, v.PrivateKeyFile, v.Port, false, diags)
	if v.Version.IsNull() {
		diags.AddAttributeError(path.Root("vm").AtName("version"), "vm.version is required", "Give the release the VM runs.")
	}
	switch {
	case v.ShortName.IsNull():
		diags.AddAttributeError(path.Root("vm").AtName("short_name"), "vm.short_name is required",
			"Give the installer's short name, stowage_vm_seed's short_name: the VM runs <short_name>-stow.")
	case known(v.ShortName) && !installer.ValidShortName(v.ShortName.ValueString()):
		diags.AddAttributeError(path.Root("vm").AtName("short_name"), "not an installer's short name",
			fmt.Sprintf("%q is not an installer's short name: give stowage_vm_seed's short_name.", v.ShortName.ValueString()))
	}
}

// planVM is the plan's part of a VM: vm.known_hosts_line kept while the one-time key is served;
// the VM's keys planned unknown where an update may change them; a change of vm.version back to
// the previous release is a rollback, refused without allow_rollback and alone.
func planVM(ctx context.Context, plan, state *installModel, resp *resource.ModifyPlanResponse) {
	if plan.VM == nil || state.VM == nil {
		return
	}
	if known(state.VM.SeedBootID) && plan.VM.KnownHostsLine.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("known_hosts_line"), "vm.known_hosts_line is still needed",
			"The VM still serves its one-time key (vm.seed_boot_id: its retirement failed, and no later boot was seen), which this line accepts: keep it until the VM reboots.")
	}
	// The keys an update may change besides the plan's own: the VM's own key when state has none
	// (an update learns it, and may record seed_boot_id), and seed_boot_id while it is set (a
	// later boot clears it). The plan holds state's values otherwise (UseStateForUnknown); the
	// release the VM reports is ModifyPlan's to plan unknown.
	if imported(state) || len(remoteChange(plan, state)) > 0 {
		if state.VM.HostKeyLine.IsNull() {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("host_key_line"), types.StringUnknown())...)
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("seed_boot_id"), types.StringUnknown())...)
		}
		if known(state.VM.SeedBootID) {
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("vm").AtName("seed_boot_id"), types.StringUnknown())...)
		}
	}
	if !known(plan.VM.Version) {
		return
	}
	desired := plan.VM.Version.ValueString()
	if !plan.VM.Version.Equal(state.VM.Version) {
		if prev := previousVersion(ctx, state); prev != "" && prev == desired {
			if !plan.AllowRollback.ValueBool() {
				resp.Diagnostics.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
					fmt.Sprintf("vm.version is now %s, the release the VM's last upgrade replaced (previous.version): reaching it is `rollback`, which reboots the VM into its passive system, the previous release (every input keeps its value). Set allow_rollback = true to roll back, or set vm.version back to %s.", prev, state.VM.Version.ValueString()))
			} else if changed := slices.DeleteFunc(remoteChange(plan, state), func(a string) bool { return a == "vm.version" }); len(changed) > 0 {
				vmRollbackAlone(&resp.Diagnostics, strings.Join(changed, ", "))
			}
		}
	}
}

// vmRollbackAlone is the refusal of a VM's rollback with other changes.
func vmRollbackAlone(diags *diag.Diagnostics, changed string) {
	diags.AddAttributeError(path.Root("allow_rollback"), "a rollback goes alone",
		fmt.Sprintf("vm.version goes back to previous.version, a rollback, which reboots the VM into its passive system: %s changed with it are not applied with it. Apply the rollback alone, then the change.", changed))
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
	if r.data == nil {
		return installer.VM{}, errors.New("the provider is not configured")
	}
	v := m.VM
	c := Connection{
		Host: v.Host.ValueString(), Port: int(v.Port.ValueInt64()), User: v.User.ValueString(),
		KeyFile: v.PrivateKeyFile.ValueString(), KnownHostsLine: v.KnownHostsLine.ValueString(),
	}
	if known(v.HostKeyLine) {
		c.KnownHostsLine = v.HostKeyLine.ValueString()
		if known(v.SeedBootID) && known(v.KnownHostsLine) {
			c.AlsoKnownHostsLines = []string{v.KnownHostsLine.ValueString()}
		}
	}
	if c.Host == "" {
		return installer.VM{}, errors.New("vm.host is not known")
	}
	if c.KnownHostsLine == "" {
		return installer.VM{}, errors.New("vm.known_hosts_line is required to install: no host key to accept yet; give the VM's own key (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on its console), or stowage_vm_seed's known_hosts_line for a VM not installed yet")
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

// wait runs step every Poll until it says done, within ctx's deadline; what, said when the
// deadline ends, names what was waited for, with the step's last error, and bound the deadline.
func (r *installResource) wait(ctx context.Context, what, bound string, step func() (bool, error)) error {
	var last error
	timedOut := func() error {
		msg := fmt.Sprintf("%s when %s ran out", what, bound)
		if last != nil {
			msg += "; the last attempt: " + last.Error()
		}
		return errors.New(msg)
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
		s, err := v.Status(ctx)
		if err != nil {
			return false, err
		}
		st = s
		return !s.Installing || s.InstallFailed != "", nil
	})
	return st, err
}

// rebooted reads the VM until it answers from a boot other than before, its health gate's trial
// over: the reboot an upgrade or a rollback scheduled, followed.
func (r *installResource) rebooted(ctx context.Context, v installer.VM, before string) (*installer.VMStatus, error) {
	var st *installer.VMStatus
	err := r.wait(ctx, "the VM had not come back from its reboot with its health gate's trial over", resourceTimeout, func() (bool, error) {
		s, err := v.Status(ctx)
		if err != nil {
			return false, err // rebooting: ssh fails, or the daemon is not up yet
		}
		st = s
		return s.BootID != "" && s.BootID != before && !s.OnTrial, nil
	})
	return st, err
}

// applyVMStatus puts the VM's status into m: on a read, the release the VM runs replaces
// vm.version, so the next plan proposes the change back (a fallback shows its real release).
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
	return "vm:" + m.VM.Host.ValueString() + "/" + m.VM.ShortName.ValueString() + "-stow"
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
		detail := "The VM runs the installer its seed carries (/usr/sbin/<short>-stow): set vm.short_name to stowage_vm_seed's short_name."
		if f.Stderr != "" {
			detail = "The VM's shell: " + f.Stderr + "\n\n" + detail
		}
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("short_name"), "The VM has no "+v.Short+"-stow", detail)
		return
	}
	err = probeNote(err, v, probe)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Waiting for the VM's first boot", err)
		return
	}
	if len(desc.Shapes) > 0 && !slices.Contains(desc.Shapes, "kairos") {
		resp.Diagnostics.AddError("No Kairos shape", fmt.Sprintf("The VM's %s %s offers %v, not kairos.", desc.Name, desc.Release.Version, desc.Shapes))
		return
	}
	// A create installs the release the VM's OS carries, never upgrades: an upgrade that failed
	// would leave a created object OpenTofu replaces at the next apply.
	if desired := m.VM.Version.ValueString(); desired != desc.Release.Version {
		resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("version"), "vm.version is not the VM's release",
			fmt.Sprintf("The VM's OS carries %s %s (its seed's version), and a create installs that release: set vm.version = %q (stowage_vm_seed's version), apply, then change vm.version to upgrade.", desc.Name, desc.Release.Version, desc.Release.Version))
		return
	}
	refuseSecretInputs(&resp.Diagnostics, stringMap(ctx, m.Inputs, &resp.Diagnostics), desc)
	if resp.Diagnostics.HasError() {
		return
	}
	change := installer.Change{Inputs: stringMap(ctx, m.Inputs, &resp.Diagnostics), SecretInputs: secrets, TLS: m.TLS.ValueString(),
		Domains: stringMap(ctx, m.Domains, &resp.Diagnostics), Ports: portMap(ctx, m.Ports, &resp.Diagnostics)}
	// A secrets export goes to the first install a seed's --await-secrets left waiting for it; the
	// installer refuses it on a VM whose first install ran.
	if _, err := v.Install(ctx, installer.VMInstall{Change: change, Given: given.ValueString(), Secrets: exported.ValueString()}); err != nil {
		addInstallerError(&resp.Diagnostics, "Installing "+desc.Name+" on the VM", err)
		return
	}
	st, err := r.settleVM(ctx, v)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the VM", err)
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
	if !vmChannelCheck(&resp.Diagnostics, m.VM.Channel, st.Channel, "The install is done: apply again once vm.channel is fixed, and the create goes on from it.") {
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
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
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

// learnKey retires the VM's one-time key by its own verb (retire), which gives the VM's own key;
// when it fails, the key is read alone (the OpenSSH host key file, through the connection, whose
// pinned key vouches for it). own is the key's line, null when neither gave it (the connection then
// keeps accepting the configuration's key alone); seed is the boot during which the VM may still
// serve the one-time key — null once its retirement is proved, else bootID (unreadBoot without
// one), while which a connection accepts both. warns are "summary\ndetail" warnings.
func (r *installResource) learnKey(ctx context.Context, m *installModel, v installer.VM, bootID string) (own, seed types.String, warns []string) {
	host := m.VM.Host.ValueString()
	own, err := r.retire(ctx, m, v, host)
	if err == nil {
		return own, types.StringNull(), nil
	}
	if !known(own) {
		key, rerr := v.HostKey(ctx)
		if rerr != nil {
			return types.StringNull(), types.StringNull(), []string{"The VM's own host key was not read\n" + err.Error() + "; reading it alone: " + rerr.Error() +
				". The connection keeps accepting vm.known_hosts_line alone, which the VM's next boot retires: give its own key there then (check it on the VM's console: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub)."}
		}
		own = types.StringValue(host + " " + key)
	}
	seed = types.StringValue(bootID)
	if bootID == "" {
		seed = types.StringValue(unreadBoot)
	}
	return own, seed, []string{"The VM's one-time host key was not retired\n" + err.Error() +
		". The VM serves it until its next boot, which retires it; until then the connection accepts it beside the VM's own (vm.seed_boot_id)."}
}

// retryRetire tries again a retirement that failed before (seed set): seed, null on success, and
// the warning ("summary\ndetail") on a failure again.
func (r *installResource) retryRetire(ctx context.Context, m *installModel, v installer.VM, own, seed types.String) (types.String, string) {
	got, err := r.retire(ctx, m, v, m.VM.Host.ValueString())
	if err == nil && !got.Equal(own) {
		err = fmt.Errorf("host-key printed %s, not the VM's key state holds (%s)", got.ValueString(), own.ValueString())
	}
	if err != nil {
		return seed, "The VM's one-time host key is still not retired\n" + err.Error() +
			". The connection keeps accepting it beside the VM's own (vm.seed_boot_id), and vm.known_hosts_line stays needed; the next update tries again, and the VM's next boot retires it."
	}
	return types.StringNull(), ""
}

// retire runs the VM's own verb, `sudo -n <short>-stow host-key --json` (Stowage's steps: run
// again, it changes nothing more), whose key is the VM's own: own, `<host> <key>`, null when the
// verb gave none. Then it proves the retirement — the verb exits 0 whether or not sshd took the
// reload — by a connection accepting own alone reading the key again: six attempts a poll apart
// (a minute in production), which also wait out sshd's reload, within the resource's timeout.
func (r *installResource) retire(ctx context.Context, m *installModel, v installer.VM, host string) (types.String, error) {
	key, err := v.RetireHostKey(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return types.StringNull(), fmt.Errorf("retiring the one-time host key: cut when %s ran out", resourceTimeout)
		}
		return types.StringNull(), err
	}
	own := types.StringValue(host + " " + key)
	only := *m
	vmo := *m.VM
	vmo.HostKeyLine, vmo.SeedBootID = own, types.StringNull()
	only.VM = &vmo
	ov, err := r.vm(&only)
	if err != nil {
		return own, err
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
			return own, fmt.Errorf("the VM did not answer under its own key alone when %s ran out", resourceTimeout)
		}
		if _, last = ov.HostKey(ctx); last == nil {
			return own, nil
		}
	}
	return own, fmt.Errorf("the VM did not answer under its own key alone in its six polls (a minute); the last attempt: %w", last)
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
// --version` — then gives later to the new release's re-run, and saves final (the plan's) filled
// from the VM's status; on a failure it saves was, the state before, with the VM's status.
func (u *vmUpdate) release(ctx context.Context, final, was *installModel, st *installer.VMStatus, desired string, later installer.Change, back bool) {
	verb := "Upgrading the VM to " + desired
	if back {
		verb = "Rolling the VM back to " + desired
	}
	var res *installer.Result
	var err error
	if back {
		res, err = u.v.Rollback(ctx)
	} else {
		res, err = u.v.Upgrade(ctx, desired)
	}
	if err != nil && !errors.Is(err, installer.ErrCut) {
		addInstallerError(u.diags, verb, err)
		return
	}
	var now *installer.VMStatus
	reboots := !(res != nil && res.Result == "nothing")
	if !reboots {
		if now, err = u.v.Status(ctx); err != nil {
			addInstallerError(u.diags, "Reading the VM", err)
			return // state stays as before the upgrade
		}
	} else if now, err = u.r.rebooted(ctx, u.v, st.BootID); err != nil {
		u.diags.AddError(verb, err.Error())
		return // state stays as before the upgrade: the response holds it
	}
	seed := was.VM.SeedBootID
	switch {
	case !known(seed):
	case reboots:
		seed = types.StringNull() // the followed reboot retired the one-time key
	case !u.tried:
		// No reboot: the retirement the key step left to it is tried here.
		var warn string
		if seed, warn = u.r.retryRetire(ctx, was, u.v, was.VM.HostKeyLine, seed); warn != "" {
			summary, detail, _ := strings.Cut(warn, "\n")
			u.diags.AddWarning(summary, detail)
		}
	}
	if !seed.Equal(was.VM.SeedBootID) {
		// Retired: state and the connection accept the VM's own key alone from here on.
		keyed, v, err := u.r.rekey(*was, was.VM.HostKeyLine, seed)
		*was = keyed
		if err != nil {
			u.diags.AddError("Cannot reach the VM", err.Error())
			u.failed(was, now, "", desired)
			return
		}
		u.v = v
	}
	runs := ""
	if now.Installed != nil {
		runs = now.Installed.Version
	}
	if runs != desired || (now.FailedBoot != nil && !now.FailedBoot.Same(st.FailedBoot) && now.FailedBoot.Release.Version == desired) {
		u.failed(was, now, verb, desired)
		return
	}
	if !later.Empty() {
		// What the new release adds is checked against it before it reaches a command line:
		// an input it declares secret goes in secret_inputs_wo.
		next, err := u.v.Describe(ctx)
		if err != nil {
			addInstallerError(u.diags, "Reading the VM's installer of "+desired, err)
			u.failed(was, now, "", desired)
			return
		}
		refuseSecretInputs(u.diags, later.Inputs, next)
		if u.diags.HasError() {
			u.failed(was, now, "", desired)
			return
		}
		if _, err := u.v.Install(ctx, installer.VMInstall{Change: later}); err != nil {
			addInstallerError(u.diags, "Giving the VM what "+desired+" adds", err)
			u.failed(was, now, "", desired)
			return
		}
		// A status that cannot be read now keeps the one read after the reboot: the release the
		// VM runs, and the re-run's inputs, which state then records.
		if after, err := u.v.Status(ctx); err != nil {
			addInstallerError(u.diags, "Reading the VM", err)
		} else {
			now = after
		}
	}
	u.r.vmFill(final, was)
	applyVMStatus(final, now, false)
	vm := *final.VM
	vm.SeedBootID = was.VM.SeedBootID // null once a followed reboot retired the one-time key
	final.VM = &vm
	retireSeed(final, now)
	u.save(final)
}

// failed saves was with the release the VM runs and says why it is not desired: the health
// gate's verdict when it judged the new boot. verb empty: the error is said already.
func (u *vmUpdate) failed(was *installModel, st *installer.VMStatus, verb, desired string) {
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
		detail := fmt.Sprintf("After its reboot the VM runs %s, not %s.", runs, desired)
		if st != nil && st.FailedBoot != nil && st.FailedBoot.Release.Version == desired {
			fb := st.FailedBoot
			detail = fmt.Sprintf("The VM's health gate found %s's boot unhealthy: %s.", desired, fb.Reason)
			if fb.Kept != "" {
				detail += fmt.Sprintf(" It kept that boot, as going back is refused: %s.", fb.Kept)
			} else {
				detail += fmt.Sprintf(" The VM went back to %s.", runs)
			}
		}
		u.diags.AddError(verb+" failed", detail+" State holds the release the VM runs: the next plan proposes the change again. `<short>-stow status` and `events` on the VM tell more.")
	}
	u.save(&m)
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
	st, err := v.Status(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the VM", renamed(ctx, v, err))
		return
	}
	if st.Installed == nil && !st.Installing && st.InstallFailed == "" {
		resp.State.RemoveResource(ctx) // no install: the next apply installs again
		return
	}
	applyVMStatus(m, st, true)
	retireSeed(m, st)
	resp.Diagnostics.Append(dropUnrecorded(ctx, m, &st.Status)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
}

func (r *installResource) updateVM(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse, plan, state *installModel) {
	timeout, diags := plan.Timeouts.Update(ctx, updateTimeout)
	resp.Diagnostics.Append(diags...)
	var secretsWO types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	secrets := stringMap(ctx, secretsWO, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	save := func(m *installModel) bool {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
		return !resp.Diagnostics.HasError()
	}
	// The connection is the plan's, with the keys state learnt.
	conn := *plan
	vmc := *plan.VM
	vmc.HostKeyLine, vmc.SeedBootID = state.VM.HostKeyLine, state.VM.SeedBootID
	conn.VM = &vmc
	v, err := r.vm(&conn)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the VM", err.Error())
		return
	}
	st, err := v.Status(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the VM", renamed(ctx, v, err))
		return
	}
	if st.Installed == nil {
		resp.Diagnostics.AddError("No install to change", "The VM holds no install: `tofu state rm` this resource and apply to install again.")
		return
	}
	running, err := v.Describe(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the VM's installer", err)
		return
	}
	desired := plan.VM.Version.ValueString()
	step := stay
	switch {
	// The release changes only when the plan changes it: a VM that runs another release than
	// state says (a refresh skipped) is the next refresh's to show.
	case plan.VM.Version.Equal(state.VM.Version) && !imported(state):
	case st.Installed.Version == desired:
	case st.Previous != nil && st.Previous.Version == desired:
		if !plan.AllowRollback.ValueBool() {
			resp.Diagnostics.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
				fmt.Sprintf("vm.version is %s, the release the VM's last upgrade replaced: reaching it is `rollback`, which reboots the VM into the previous release (every input keeps its value). Set allow_rollback = true to roll back.", desired))
			return
		}
		step = rollback
	default:
		step = upgrade
	}
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: &resp.Diagnostics}
	now, later := vmSplit(ctx, u, running, step == upgrade)
	refuseSecretInputs(&resp.Diagnostics, stringMap(ctx, plan.Inputs, &resp.Diagnostics), running)
	if resp.Diagnostics.HasError() {
		return
	}
	if step == rollback && !now.Empty() {
		vmRollbackAlone(&resp.Diagnostics, "inputs, secret inputs, tls, domains or ports")
		return
	}
	// An imported state records the configuration's vm.channel, first-install only from then on:
	// it must be the channel the VM follows.
	if imported(state) && !vmChannelCheck(&resp.Diagnostics, plan.VM.Channel, st.Channel, "Nothing was run.") {
		return
	}
	if step != stay && st.BootID == "" {
		resp.Diagnostics.AddError("The VM reports no boot_id",
			"Its status has no boot_id, so the release change's reboot could not be followed: nothing was run. Read `<short>-stow status` on the VM.")
		return
	}
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
		if seed, warn = r.retryRetire(ctx, &conn, v, own, seed); warn != "" {
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
			"It could not be read (the warning says why). The release change's reboot would retire the key this connection accepts, and could not be followed: nothing was run. Give vm.known_hosts_line the VM's own key (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on its console), then apply again.")
		return
	}
	// was is state as the VM stands before the release change: after the re-run, the plan's
	// inputs it took, the release unchanged.
	was := *state
	was.VM = withKeys(planConn(state.VM, plan.VM), own, seed)
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
		after, err := v.Status(ctx)
		if err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the VM", err)
			return
		}
		r.vmFill(&final, state)
		applyVMStatus(&final, after, false)
		retireSeed(&final, after)
		save(&final)
		return
	}
	// learnKey retired nothing in this update (state had no key, and a seed came back): release()
	// does not try again in the same apply. The key step's own retry runs only without a release
	// change, so release() never follows it.
	tried := !known(state.VM.HostKeyLine) && known(seed)
	vu := &vmUpdate{r: r, v: v, tried: tried, diags: &resp.Diagnostics, save: save}
	vu.release(ctx, &final, &was, st, desired, later, step == rollback)
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

// shortNameFixed refuses a vm.short_name other than state's: state names the installer the VM's
// seed (or the import ID) gave, and the provider runs no other.
func shortNameFixed(diags *diag.Diagnostics, was, now string) {
	firstInstallOnlyFix(diags, path.Root("vm").AtName("short_name"), "vm.short_name", was, now,
		fmt.Sprintf("Set it back to %q. %s", was, renameFix))
}

// renameFix is what to do when the VM runs another installer than state names.
const renameFix = "When the VM runs another installer (a release that renamed it, an import under a wrong name; `ls /usr/sbin/*-stow` on the VM names it): " +
	"give vm.known_hosts_line the VM's own key — state's vm.host_key_line (when vm.seed_boot_id is set too, the VM still serves its one-time key: reboot the VM first, which retires it), " +
	"else the key checked on the VM's console (ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub) —, then `tofu state rm` this resource and `tofu import` it as vm:<host>/<short>-stow."

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

// vmChannelCheck says whether the configured vm.channel is the VM's (or left out); else it is an
// error ending with after, naming the channel to set.
func vmChannelCheck(diags *diag.Diagnostics, configured types.String, vmChannel, after string) bool {
	if !known(configured) || configured.ValueString() == vmChannel {
		return true
	}
	detail := fmt.Sprintf("The VM follows the channel %q (its seed's); vm.channel says %q, which state would record, first-install only from then on. Set vm.channel to %q, or leave it out. %s", vmChannel, configured.ValueString(), vmChannel, after)
	if vmChannel == "" {
		detail = fmt.Sprintf("The VM's status reports no channel; vm.channel says %q, which state would record. Leave vm.channel out. %s", configured.ValueString(), after)
	}
	diags.AddAttributeError(path.Root("vm").AtName("channel"), "vm.channel is not the VM's", detail)
	return false
}

// planConn is s (state's vm block) with p's (the plan's) connection: the one this update reaches
// the VM by, which a state saved on its way keeps — an imported state holds none.
func planConn(s, p *vmModel) vmModel {
	v := *s
	v.Host, v.Port, v.User, v.PrivateKeyFile, v.KnownHostsLine, v.ShortName = p.Host, p.Port, p.User, p.PrivateKeyFile, p.KnownHostsLine, p.ShortName
	return v
}

// vmSplit sorts an update's changes between the re-run of the running release (now) and the re-run
// after the upgrade (later): the VM's upgrade takes no input, so an input or a secret input the
// running release does not declare waits for the release that adds it, and so does the port or
// domain of a route state does not record; everything else goes now, where the installer
// refuses what it does not take.
func vmSplit(ctx context.Context, u *update, running *installer.Describe, upgrading bool) (now, later installer.Change) {
	plan, state := u.plan, u.state
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
		case upgrading && !running.Declares(k):
			later.SecretInputs[k] = v
		case bump:
			now.SecretInputs[k] = v
		}
	}
	if known(plan.TLS) && !plan.TLS.Equal(state.TLS) {
		now.TLS = plan.TLS.ValueString()
	}
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
			case upgrading && !ok && !imported(state):
				c.later[k] = v
			default:
				c.now[k] = v
			}
		}
	}
	return now, later
}

// importVM reads an import ID `vm:<host>/<short>-stow`: the VM and the installer it runs. The
// configuration gives the connection — vm.known_hosts_line the VM's own key — at the first apply.
func (r *installResource) importVM(ctx context.Context, id string, resp *resource.ImportStateResponse) {
	host, file, ok := strings.Cut(id, "/")
	short, isStow := strings.CutSuffix(file, "-stow")
	if !ok || !isStow || !installer.ValidShortName(short) || target.ValidateHostUser(host, "u") != nil {
		resp.Diagnostics.AddError("Import ID", fmt.Sprintf("%q: want vm:<host>/<short>-stow, the VM's address and the installer it runs (`ls /usr/sbin/*-stow` on it)", "vm:"+id))
		return
	}
	null := types.StringNull()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("vm"), &vmModel{
		Host: types.StringValue(host), Port: types.Int64Null(), User: null, PrivateKeyFile: null, KnownHostsLine: null,
		Version: null, Channel: null, ShortName: types.StringValue(short), HostKeyLine: null, SeedBootID: null})...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), "vm:"+id)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("short_name"), short)...)
	for a, v := range map[string]bool{"deletion_protection": true, "delete_data": false, "allow_rollback": false} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(a), v)...)
	}
}
