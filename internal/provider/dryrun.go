package provider

import (
	"context"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// planDryRunTimeout bounds a plan's dry run: the new installer fetched, the target read.
const planDryRunTimeout = 10 * time.Minute

// planDryRun runs, with plan_dry_run, the dry run of the release change a Docker plan makes: the
// new release's `upgrade --dry-run` given what its upgrade would be (the inputs, secret inputs and
// ports it adds), or the kept installer's `rollback --dry-run`. A refusal is a plan error, its
// reason and fix the installer's; a failure is a plan error too, naming the dry run. Nothing on
// the target changes but a private directory beside the install, removed afterwards.
func (r *installResource) planDryRun(ctx context.Context, req resource.ModifyPlanRequest, plan, state *installModel, diags *diag.Diagnostics) {
	if !plan.PlanDryRun.ValueBool() || plan.Docker == nil || state.Docker == nil || imported(state) || r.data == nil ||
		!known(plan.Installer) || plan.Installer.Equal(state.Installer) || diags.HasError() {
		return
	}
	for _, v := range []types.String{plan.Docker.Host, plan.Docker.User, plan.Docker.KnownHostsLine, plan.Docker.PrivateKeyFile} {
		if v.IsUnknown() {
			return // the connection is known at apply
		}
	}
	ctx, cancel := context.WithTimeout(ctx, planDryRunTimeout)
	defer cancel()
	d, err := r.docker(plan, state.ShortName.ValueString())
	if err != nil {
		diags.AddError("The plan's dry run cannot reach the Docker target", err.Error())
		return
	}
	if prev := previousInstaller(ctx, state); prev != "" && prev == plan.Installer.ValueString() {
		if _, err := d.DryRunRollback(ctx); err != nil {
			addInstallerError(diags, "The plan's dry run of the rollback", err)
		}
		return
	}
	running, err := d.KeptDescribe(ctx)
	if err != nil || running == nil {
		if err == nil {
			return // no install any more: the apply says so
		}
		addInstallerError(diags, "The plan's dry run: reading the installer the install keeps", err)
		return
	}
	dir, err := d.PlanDir(ctx)
	if err != nil {
		addInstallerError(diags, "The plan's dry run", err)
		return
	}
	defer func() {
		if err := d.RemovePlanDir(context.WithoutCancel(ctx), dir); err != nil {
			diags.AddWarning("The plan's dry run left its directory", err.Error())
		}
	}()
	nd := d
	nd.WorkDir = dir
	next := r.fetchDescribe(ctx, nd, plan.Installer.ValueString(), diags)
	if next == nil {
		return
	}
	var secretsWO types.Map
	diags.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	secrets := map[string]string{}
	if known(secretsWO) {
		secrets = stringMap(ctx, secretsWO, diags)
	}
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: diags}
	_, up := u.split(ctx, running, next)
	if diags.HasError() {
		return
	}
	if _, err := d.Without(running, next).DryRunUpgrade(ctx, dir+"/"+d.Name+".sh", up); err != nil {
		addInstallerError(diags, "The plan's dry run of the upgrade to "+next.Release.Version, err)
	}
}
