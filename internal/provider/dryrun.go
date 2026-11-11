package provider

import (
	"context"
	"slices"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// planDryRunTimeout bounds a plan's dry run: the new installer fetched, the target read.
const planDryRunTimeout = 10 * time.Minute

// planDryRun runs, with plan_dry_run, the dry run of the release change a Docker plan makes alone:
// the new release's `upgrade --dry-run` given what its upgrade would be (the inputs, secret inputs
// and ports it adds), or the kept installer's `rollback --dry-run`, the step chosen from the
// install's status as the apply chooses it. A plan that also re-runs the running release (other
// inputs, TLS, domains or ports) is not dry-run, with a warning: the apply's upgrade runs on the
// install that re-run leaves. A refusal is a plan error, its reason and fix the installer's; a
// failure is a plan error too. On the target: a private directory beside the install the new
// installer is fetched and extracted into, removed afterwards (its image stays in the daemon's
// store, as any fetch's), and the install's lock while the dry run reads it.
func (r *installResource) planDryRun(ctx context.Context, req resource.ModifyPlanRequest, plan, state *installModel, diags *diag.Diagnostics) {
	if !plan.PlanDryRun.ValueBool() || plan.Docker == nil || state.Docker == nil || imported(state) || r.data == nil ||
		!known(plan.Installer) || plan.Installer.Equal(state.Installer) || diags.HasError() {
		return
	}
	var secretsWO types.Map
	diags.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	for _, v := range []attr.Value{plan.Docker.Host, plan.Docker.User, plan.Docker.KnownHostsLine, plan.Docker.PrivateKeyFile, plan.Docker.Port,
		plan.Docker.WorkDir, plan.Inputs, plan.Ports, plan.TLS, plan.Domains, secretsWO} {
		if !wholyKnown(v) {
			return // known at apply: a dry run of what the plan does not know yet would mislead
		}
	}
	ctx, cancel := context.WithTimeout(ctx, planDryRunTimeout)
	defer cancel()
	d, err := r.docker(plan, state.ShortName.ValueString())
	if err != nil {
		diags.AddError("The plan's dry run cannot reach the Docker target", err.Error())
		return
	}
	st, err := d.Status(ctx)
	if err != nil || st == nil {
		if err != nil {
			addInstallerError(diags, "The plan's dry run: reading the install", err)
		}
		return // no install: the apply says so
	}
	desired := plan.Installer.ValueString()
	switch {
	case st.Installed != nil && st.Installed.Installer == desired:
		return // nothing to upgrade to
	case st.Previous != nil && st.Previous.Installer == desired:
		if changed := slices.DeleteFunc(remoteChange(plan, state), func(a string) bool { return a == "installer" }); len(changed) > 0 {
			return // a rollback goes alone: refused at plan already
		}
		if _, err := d.DryRunRollback(ctx); err != nil {
			addInstallerError(diags, "The plan's dry run of the rollback", err)
		}
		return
	}
	running, err := d.KeptDescribe(ctx)
	if err != nil || running == nil {
		if err != nil {
			addInstallerError(diags, "The plan's dry run: reading the installer the install keeps", err)
		}
		return
	}
	dir, err := d.PlanDir(ctx)
	if err != nil {
		addInstallerError(diags, "The plan's dry run", err)
		return
	}
	defer func() {
		// Its own bound: the dry run's may have run out.
		rm, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer done()
		if err := d.RemovePlanDir(rm, dir); err != nil {
			diags.AddWarning("The plan's dry run left its directory", err.Error()+" (a later plan's dry run removes it after an hour)")
		}
	}()
	nd := d
	nd.WorkDir = dir
	next := r.fetchDescribe(ctx, nd, desired, diags)
	if next == nil {
		return
	}
	secrets := stringMap(ctx, secretsWO, diags)
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: diags}
	rerun, up := u.split(ctx, running, next)
	refuseSecretInputs(diags, stringMap(ctx, plan.Inputs, diags), running, next)
	if d.Local() {
		refuseRunEnvInputs(diags, nil, "", stringMap(ctx, plan.Inputs, diags), secrets)
	}
	if diags.HasError() {
		return
	}
	if !rerun.Empty() {
		diags.AddWarning("No dry run of this plan",
			"The plan also re-runs the running release (inputs, secret inputs, TLS, domains or ports it declares): the upgrade runs on the install that re-run leaves, which a dry run cannot show. Apply the re-run alone first to have the upgrade dry-run.")
		return
	}
	if _, err := d.Without(running, next).DryRunUpgrade(ctx, dir, up); err != nil {
		addInstallerError(diags, "The plan's dry run of the upgrade to "+next.Release.Version, err)
	}
}

// wholyKnown says whether v and every element in it are known.
func wholyKnown(v attr.Value) bool {
	if v == nil || v.IsUnknown() {
		return false
	}
	if m, ok := v.(types.Map); ok && !m.IsNull() {
		for _, e := range m.Elements() {
			if e.IsUnknown() {
				return false
			}
		}
	}
	return true
}
