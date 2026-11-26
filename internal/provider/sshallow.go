package provider

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// sshAllowPath is vm.ssh_allow in the model.
var sshAllowPath = path.Root("vm").AtName("ssh_allow")

// maskedCIDR refuses an ssh_allow source that is not a CIDR written as its masked form, the one
// the VM answers (`ssh-allow --json` masks each source): state then never differs from the VM's
// answer by its spelling alone.
type maskedCIDR struct{}

var _ validator.String = maskedCIDR{}

func (maskedCIDR) Description(context.Context) string {
	return "a CIDR with its prefix, written masked (192.0.2.0/24, 2001:db8::/32)"
}

func (v maskedCIDR) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (maskedCIDR) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if !known(req.ConfigValue) {
		return
	}
	s := req.ConfigValue.ValueString()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "not a source with its prefix",
			fmt.Sprintf("%q is not a CIDR with its prefix: give one as 192.0.2.0/24 or 2001:db8::/32 (0.0.0.0/0 and ::/0 for every source).", s))
		return
	}
	if p.Addr().Is4In6() {
		// The VM takes an IPv4-mapped network as its IPv4 one, and answers so.
		hint := "write the IPv4 network (192.0.2.0/24)"
		if p.Bits() >= 96 {
			hint = fmt.Sprintf("write %q", netip.PrefixFrom(p.Addr().Unmap(), p.Bits()-96).Masked().String())
		}
		resp.Diagnostics.AddAttributeError(req.Path, "an IPv4 network written in IPv6",
			fmt.Sprintf("%q is an IPv4-mapped network, which the VM takes as IPv4: %s.", s, hint))
		return
	}
	if m := p.Masked().String(); m != s {
		resp.Diagnostics.AddAttributeError(req.Path, "a source not written masked",
			fmt.Sprintf("%q is the source %s, which the VM answers so: write %q.", s, m, m))
	}
}

// sshAllowPendingKey is the private state's key set when a create recorded ssh_allow as planned
// and could not apply it: the next plan then shows an update that applies it.
const sshAllowPendingKey = "ssh_allow_pending"

func sshAllowPending(ctx context.Context, p privateData) bool {
	if p == nil || reflect.ValueOf(p).IsNil() {
		return false
	}
	b, _ := p.GetKey(ctx, sshAllowPendingKey)
	return len(b) > 0
}

func setSSHAllowPending(ctx context.Context, p privateData, on bool, diags *diag.Diagnostics) {
	var b []byte
	if on {
		b = []byte("true")
	}
	diags.Append(p.SetKey(ctx, sshAllowPendingKey, b)...)
}

// sshSources are s's elements, sorted.
func sshSources(ctx context.Context, s types.Set, diags *diag.Diagnostics) []string {
	var out []string
	diags.Append(s.ElementsAs(ctx, &out, false)...)
	slices.Sort(out)
	return out
}

// sshSourcesSet is sources as ssh_allow's value.
func sshSourcesSet(sources []string, diags *diag.Diagnostics) types.Set {
	s, d := types.SetValueFrom(context.Background(), types.StringType, sources)
	diags.Append(d...)
	return s
}

// sameSources says whether a and b hold the same sources, whatever their order or repeats.
func sameSources(a, b []string) bool {
	return slices.Equal(slices.Compact(slices.Sorted(slices.Values(a))), slices.Compact(slices.Sorted(slices.Values(b))))
}

// applySSHAllow brings the VM's SSH sources to want: it reads them (`ssh-allow --json`) and,
// when they differ, sets them (`ssh-allow set`), then confirms the change over a new SSH
// connection (`ssh-allow confirm`, a run of its own: the runner never shares a connection). A
// confirm ssh cannot make is the VM refusing that connection: the window undoes the change by
// itself. A confirm that fails otherwise is followed by `ssh-allow revert`. A change already
// awaiting confirmation, read or refused at set, is an error naming the verbs that end it.
func applySSHAllow(ctx context.Context, v installer.VM, want []string) error {
	ctl := v.Short + "-stow"
	pendingErr := func(why string) error {
		return fmt.Errorf("%s: confirm it (`sudo %s ssh-allow confirm`, over a new SSH connection) or undo it (`sudo %s ssh-allow revert`) on the VM, then apply again", why, ctl, ctl)
	}
	cur, err := v.SSHAllow(ctx)
	if err != nil {
		return err
	}
	if cur.Pending != nil {
		return pendingErr(fmt.Sprintf("a change of SSH's sources awaits confirmation on the VM (%s, else back to %s in %d s)", strings.Join(cur.Sources, ", "), strings.Join(cur.Pending.Previous, ", "), cur.Pending.SecsLeft))
	}
	if cur.Sources != nil && sameSources(cur.Sources, want) {
		return nil
	}
	set, err := v.SSHAllowSet(ctx, want)
	if errors.Is(err, installer.ErrSSHPending) {
		return pendingErr(err.Error())
	}
	if err != nil {
		return err
	}
	if set.Pending == nil {
		return nil // applied for good: nothing to confirm
	}
	if _, err := v.SSHAllowConfirm(ctx); err != nil {
		if sshFailed(err) {
			return fmt.Errorf("the VM refused the new SSH connection that confirms the change of SSH's sources to %s (ssh_allow), which probably leaves out the address OpenTofu runs from: its window undoes the change by itself within %d s, back to %s. Add that address to ssh_allow beside the sources that let it in, and apply again (a platform firewall in front of the VM must let it through too). %w",
				strings.Join(want, ", "), set.Pending.SecsLeft, strings.Join(set.Pending.Previous, ", "), err)
		}
		if _, rerr := v.SSHAllowRevert(ctx); rerr != nil {
			return fmt.Errorf("confirming the change of SSH's sources failed: %w\n\nUndoing it failed too (its window undoes it within %d s): %v", err, set.Pending.SecsLeft, rerr)
		}
		return fmt.Errorf("confirming the change of SSH's sources failed, and it was undone (`ssh-allow revert`): %w", err)
	}
	return nil
}

// createSSHAllow applies m's ssh_allow after a create's other steps, which marked it pending: a
// failure is a warning (an error would taint the install, whose replacement is a destroy), the
// sources recorded as planned and left pending, so the next plan shows an update that applies
// them; a success clears the mark.
func (r *installResource) createSSHAllow(ctx context.Context, m *installModel, resp *resource.CreateResponse) {
	if m.VM == nil || m.VM.SSHAllow.IsNull() {
		return
	}
	want := sshSources(ctx, m.VM.SSHAllow, &resp.Diagnostics)
	v, err := r.vm(m)
	if err == nil {
		err = applySSHAllow(ctx, v, want)
	}
	if err != nil {
		resp.Diagnostics.AddAttributeWarning(sshAllowPath, "SSH's sources are not set on the VM",
			err.Error()+"\n\nThe install is recorded: the next plan shows an update that sets ssh_allow again.")
		return // pending since createVM marked it
	}
	setSSHAllowPending(ctx, resp.Private, false, &resp.Diagnostics)
}

// sshAllowDue says whether an update sets the VM's SSH sources: ssh_allow managed, and changed or
// pending from the create. When due, plan's ssh_allow is set back to state's, so every state the
// update's other steps save records the sources the VM holds; updateSSHAllow records want once
// they are set.
func sshAllowDue(ctx context.Context, p privateData, plan, state *installModel) (want types.Set, due bool) {
	if plan.VM == nil || state.VM == nil || plan.VM.SSHAllow.IsNull() || plan.VM.SSHAllow.IsUnknown() {
		return types.Set{}, false
	}
	want = plan.VM.SSHAllow
	if want.Equal(state.VM.SSHAllow) && !sshAllowPending(ctx, p) {
		return want, false
	}
	vmc := *plan.VM
	vmc.SSHAllow = state.VM.SSHAllow
	plan.VM = &vmc
	return want, true
}

// updateSSHAllow sets the VM's SSH sources to want, the update's last step, and records them.
// A failure leaves state's (as the steps before saved it), the pending mark as it was.
func (r *installResource) updateSSHAllow(ctx context.Context, resp *resource.UpdateResponse, want types.Set) {
	var m installModel
	if resp.Diagnostics.Append(resp.State.Get(ctx, &m)...); resp.Diagnostics.HasError() || m.VM == nil {
		return
	}
	sources := sshSources(ctx, want, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	v, err := r.vm(&m)
	if err == nil {
		err = applySSHAllow(ctx, v, sources)
	}
	if err != nil {
		// err.Error() whole: a refusal of a confirm or a revert comes after a change was made.
		resp.Diagnostics.AddAttributeError(sshAllowPath, "Setting SSH's sources on the VM", err.Error())
		return
	}
	vmc := *m.VM
	vmc.SSHAllow = want
	m.VM = &vmc
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	setSSHAllowPending(ctx, resp.Private, false, &resp.Diagnostics)
}

// readSSHAllow refreshes a managed ssh_allow from the VM's `ssh-allow --json`: a change awaiting
// confirmation there is read as its sources, with a warning; a rule file that does not read as
// SSH's sources, or a read that fails, leaves state's, with a warning.
func readSSHAllow(ctx context.Context, v installer.VM, m *installModel, diags *diag.Diagnostics) {
	if m.VM == nil || m.VM.SSHAllow.IsNull() {
		return
	}
	a, err := v.SSHAllow(ctx)
	if err != nil {
		diags.AddAttributeWarning(sshAllowPath, "Reading SSH's sources on the VM failed", installerDetail(err)+". ssh_allow is left as state has it.")
		return
	}
	if a.Sources == nil {
		diags.AddAttributeWarning(sshAllowPath, "SSH's sources on the VM are not known",
			fmt.Sprintf("`%s-stow ssh-allow --json`: %s. ssh_allow is left as state has it; an apply sets the configuration's again.", v.Short, a.Rules))
		return
	}
	if p := a.Pending; p != nil {
		diags.AddAttributeWarning(sshAllowPath, "A change of SSH's sources awaits confirmation on the VM",
			fmt.Sprintf("The VM accepts %s until %s (%d s), then goes back to %s unless `sudo %s-stow ssh-allow confirm` keeps the change (`ssh-allow revert` undoes it now). ssh_allow is read as the change's sources.",
				strings.Join(a.Sources, ", "), p.Deadline, p.SecsLeft, strings.Join(p.Previous, ", "), v.Short))
	}
	if cur := sshSources(ctx, m.VM.SSHAllow, diags); sameSources(cur, a.Sources) {
		return // state's spelling kept
	}
	vmc := *m.VM
	vmc.SSHAllow = sshSourcesSet(a.Sources, diags)
	m.VM = &vmc
}
