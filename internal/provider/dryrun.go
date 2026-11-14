package provider

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// planDryRunTimeout bounds a plan's dry run: the new installer fetched, the target read.
const planDryRunTimeout = 10 * time.Minute

// planDryRun runs, with plan_dry_run, the dry run of the release change a plan makes alone, on
// each target (dockerPlanDryRun, k8sPlanDryRun, vmPlanDryRun). On Docker:
// the new release's `upgrade --dry-run` given what its upgrade would be (the inputs, secret inputs
// and ports it adds), or the kept installer's `rollback --dry-run`, the step chosen from the
// install's status as the apply chooses it. A plan that also re-runs the running release (other
// inputs, TLS, domains or ports) is not dry-run, with a warning: the apply's upgrade runs on the
// install that re-run leaves. A refusal is a plan error, its reason and fix the installer's; a
// failure is a plan error too. On the target: a private directory beside the install the new
// installer is fetched and extracted into, removed afterwards (its image stays in the daemon's
// store, as any fetch's), and the install's lock while the dry run reads it.
func (r *installResource) planDryRun(ctx context.Context, req resource.ModifyPlanRequest, plan, state *installModel, diags *diag.Diagnostics) {
	if !plan.PlanDryRun.ValueBool() || r.data == nil || diags.HasError() {
		return
	}
	var secretsWO types.Map
	if d := req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO); d.HasError() {
		diags.Append(d...)
		return
	}
	configKnown := configKnownFor(req)
	switch {
	case plan.Docker != nil && state.Docker != nil:
		r.dockerPlanDryRun(ctx, plan, state, secretsWO, configKnown, diags)
	case plan.Kubernetes != nil && state.Kubernetes != nil:
		r.k8sPlanDryRun(ctx, plan, state, secretsWO, configKnown, diags)
	case plan.VM != nil && state.VM != nil:
		r.vmPlanDryRun(ctx, plan, state, secretsWO, configKnown, diags)
	}
}

// k8sPlanDryRun runs, with plan_dry_run, the dry run of the release change a Kubernetes plan makes
// alone: the release read and the step chosen as the apply reads and chooses them (k8sPrepare:
// the installers fetched into a private directory on this machine, removed after), then the new
// release's `k8s upgrade --dry-run` given what its upgrade would be, or `k8s rollback --dry-run`.
// A plan that also re-runs the running release is not dry-run, with a warning.
func (r *installResource) k8sPlanDryRun(ctx context.Context, plan, state *installModel, secretsWO types.Map, configKnown bool, diags *diag.Diagnostics) {
	if plan.Installer.Equal(state.Installer) && !imported(state) {
		return
	}
	if !configKnown || (r.data.Kubernetes != nil && !connKnown(r.data.Kubernetes)) || !r.data.SettingsKnown {
		notKnownYet(diags, "a value of the configuration, the provider's kubernetes block or a setting a run reads")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, planDryRunTimeout)
	defer cancel()
	p := r.k8sPrepare(ctx, plan, state, stringMap(ctx, secretsWO, diags), diags, true)
	if p == nil {
		return
	}
	defer p.run.close()
	if imported(state) {
		var given []string
		if !plan.Kubernetes.Values.IsNull() {
			given = []string{"kubernetes.values"}
		}
		adoptionReport(diags, p.u, p.rerun, p.up, releaseChangeOf(p.step != stay || plan.Installer.ValueString() != p.runningRef), p.running, p.next == nil && p.step == stay, given...)
	}
	// One order on every target: stay, rollback (its prepare refused one beside a re-run), a
	// re-run beside the upgrade, the upgrade.
	switch {
	case p.step == stay:
	case p.step == rollback:
		if _, err := p.cur.DryRunRollback(ctx); err != nil {
			addInstallerError(diags, "The plan's dry run of the rollback", err)
		}
	case !p.rerun.Empty():
		noDryRun(diags, true)
	default:
		if _, err := p.nextK.DryRunUpgrade(ctx, p.up, r.registryCA()); err != nil {
			addInstallerError(diags, "The plan's dry run of the upgrade to "+p.next.Release.Version, err)
		}
	}
}

// vmPlanDryRun runs, with plan_dry_run, the dry run of the release change a VM plan makes alone:
// the VM read and the step chosen as the apply reads and chooses them (vmPrepare, its refusals
// included), then `upgrade --version V --dry-run --json` or `rollback --dry-run --json` over SSH —
// every check the host daemon runs before its job, no reboot. A plan that also re-runs the running
// release is not dry-run, with a warning.
func (r *installResource) vmPlanDryRun(ctx context.Context, plan, state *installModel, secretsWO types.Map, configKnown bool, diags *diag.Diagnostics) {
	if plan.VM.Version.Equal(state.VM.Version) && !imported(state) {
		return
	}
	// The VM's dry run reads only the vm block's connection over SSH (r.vm: no provider setting
	// but the runner's factory): the provider's registry settings are not the VM's.
	if !configKnown {
		notKnownYet(diags, "a value of the configuration")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, planDryRunTimeout)
	defer cancel()
	p := r.vmPrepare(ctx, plan, state, stringMap(ctx, secretsWO, diags), diags)
	if p == nil {
		return
	}
	if imported(state) {
		adoptionReport(diags, p.u, p.now, p.later, releaseChangeOf(p.step != stay), p.running, false)
	}
	if p.step != stay && !known(state.VM.HostKeyLine) {
		diags.AddWarning("The VM's own host key is not known yet",
			"State has none: the apply reads it before the release change, and refuses the change when it cannot (`The VM's own host key is not known`). The dry run below runs under the key vm.known_hosts_line accepts.")
	}
	switch {
	case p.step == stay:
	case p.step == rollback:
		if _, err := p.v.DryRunRollback(ctx); err != nil {
			vmDryRunFailed(diags, "The plan's dry run of the VM's rollback", err)
		}
	case !p.now.Empty():
		noDryRun(diags, true)
	default:
		if _, err := p.v.DryRunUpgrade(ctx, plan.VM.Version.ValueString()); err != nil {
			vmDryRunFailed(diags, "The plan's dry run of the VM's upgrade to "+plan.VM.Version.ValueString(), err)
		}
		if !p.later.Empty() {
			diags.AddWarning("The dry run covers the upgrade alone",
				"What the plan gives that only "+plan.VM.Version.ValueString()+" declares (inputs, secret inputs, TLS, domains or ports) is given by the re-run after its reboot, which no dry run checks: a refusal of it then leaves the VM on the new release, and the next plan proposes the re-run again.")
		}
	}
}

// vmDryRunFailed is the plan error of a VM's dry run that failed or was refused — but for an OS
// whose installer has no --dry-run, a warning: the change is planned without one, and the release
// it brings dry-runs the next.
func vmDryRunFailed(diags *diag.Diagnostics, summary string, err error) {
	if errors.Is(err, installer.ErrNoDryRun) {
		diags.AddWarning("No dry run of this plan", "The VM's OS carries an installer without --dry-run (built with Stowage before 0.38.0): the change is planned without a dry run. Once the VM runs a release built with Stowage 0.38.0 or later, plans dry-run its changes.")
		return
	}
	addInstallerError(diags, summary, err)
}

// dockerPlanDryRun is planDryRun on a Docker target.
func (r *installResource) dockerPlanDryRun(ctx context.Context, plan, state *installModel, secretsWO types.Map, configKnown bool, diags *diag.Diagnostics) {
	if plan.Installer.Equal(state.Installer) && !imported(state) {
		return
	}
	if !configKnown || !r.data.SettingsKnown {
		notKnownYet(diags, "a value of the configuration or a setting a run reads")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, planDryRunTimeout)
	defer cancel()
	d, err := r.docker(plan, state.ShortName.ValueString())
	if err != nil {
		diags.AddError("The plan's dry run cannot reach the Docker target", err.Error())
		return
	}
	if !strings.HasPrefix(d.WorkDir, "/") {
		// A home never resolved: the apply resolves nothing more, and names the install ./<name>.
		if imported(state) {
			diags.AddWarning("Taking over the install: its values are not read at plan",
				"Its work_dir is the SSH user's home, which the apply resolves: the apply compares the configuration with what the install records.")
		}
		return
	}
	st, err := d.StatusOf(ctx, imported(state))
	if err != nil || st == nil {
		if err != nil {
			addInstallerError(diags, "The plan's dry run: reading the install", err)
		}
		return // no install: the apply says so
	}
	if st.ShortName != "" && st.ShortName != d.Short {
		// The kept installer as the apply finds it, by the same constructor.
		if d, err = r.docker(plan, st.ShortName); err != nil {
			diags.AddError("The plan's dry run cannot reach the Docker target", err.Error())
			return
		}
	}
	running, err := d.KeptDescribe(ctx)
	if err != nil {
		addInstallerError(diags, "The plan's dry run: reading the installer the install keeps", err)
		return
	}
	if running == nil {
		diags.AddWarning("No dry run of this plan", "The install went since its status was read: the apply says what it finds.")
		return
	}
	secrets := stringMap(ctx, secretsWO, diags)
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: diags}
	if !u.keptChecks(d, st, running) {
		return // the apply's own errors, at plan
	}
	if u.adopt(ctx, st); diags.HasError() {
		return
	}
	// The step as the apply chooses it (releaseStep). The new installer, when it must be read, is
	// fetched into a plan's directory — but for a plan that re-runs the running release too, which
	// is told first, before anything is fetched: no dry run of it.
	var dir string
	defer func() {
		if dir == "" {
			return
		}
		// Its own bound: the dry run's may have run out.
		rm, done := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer done()
		if err := d.RemovePlanDir(rm, dir); err != nil {
			diags.AddWarning("The plan's dry run left its directory", err.Error()+" (a later plan's dry run removes it after an hour)")
		}
	}()
	fetchTo := func() (installer.Docker, bool) {
		if u.rerunCertain(ctx, running) {
			return installer.Docker{}, false
		}
		var err error
		if dir, err = d.PlanDir(ctx); err != nil {
			addInstallerError(diags, "The plan's dry run", err)
			return installer.Docker{}, false
		}
		nd := d
		nd.WorkDir = dir
		return nd, true
	}
	step, next := u.releaseStep(ctx, d, st, fetchTo)
	if diags.HasError() {
		return
	}
	// As the apply: the inputs checked (next nil for a rollback), then a rollback goes alone and
	// an upgrade beside a re-run is not dry-run.
	rerun, up := u.split(ctx, running, next)
	given := stringMap(ctx, plan.Inputs, diags)
	refuseSecretInputs(diags, given, running, next)
	if d.Local() {
		refuseRunEnvInputs(diags, nil, "", given, secrets)
	}
	if step == rollback && imported(state) {
		importedRollback(diags, path.Root("installer"), "installer", installerRuns(st), "previous.installer") // as the apply
	}
	// The step is the release change, but when the re-run skipped the fetch (step stay, nothing
	// fetched): then the installer the install records tells it, and none recorded leaves it open.
	change := noReleaseChange
	switch {
	case step != stay:
		change = releaseChanges
	case dir != "":
	case st.Installed == nil || st.Installed.Installer == "":
		change = releaseMayChange
	case plan.Installer.ValueString() != st.Installed.Installer:
		change = releaseChanges
	}
	guessed := next == nil && step == stay && dir == ""
	if imported(state) && step != rollback && change == releaseChanges {
		refuseNewRouteDomains(diags, rerun, running) // as the apply
	}
	if diags.HasError() {
		return
	}
	if imported(state) {
		adoptionReport(diags, u, rerun, up, change, running, guessed)
	}
	if step == stay {
		return
	}
	if step == rollback {
		if !rerun.Empty() {
			rollbackAlone(diags, "inputs, secret inputs, TLS, domains or ports")
			return
		}
		if _, err := d.Without(running).DryRunRollback(ctx); err != nil {
			addInstallerError(diags, "The plan's dry run of the rollback", err)
		}
		return
	}
	if !rerun.Empty() {
		noDryRun(diags, true)
		return
	}
	if _, err := d.Without(running, next).DryRunUpgrade(ctx, dir, up); err != nil {
		addInstallerError(diags, "The plan's dry run of the upgrade to "+next.Release.Version, err)
	}
}

// certainRerun says whether c, the plan's split against the running release alone, holds a
// change any new release leaves to the re-run: TLS, domains, a secret input, an input the running
// release declares that the plan gives (empty included), a port of a route it has.
func certainRerun(plan *installModel, running *installer.Describe, c installer.Change) bool {
	if c.TLS != "" || len(c.Domains) > 0 || len(c.SecretInputs) > 0 {
		return true
	}
	for k := range c.Ports {
		if running.HasRoute(k) {
			return true
		}
	}
	given := map[string]attr.Value{}
	if known(plan.Inputs) {
		given = plan.Inputs.Elements()
	}
	for k := range c.Inputs {
		if _, ok := given[k]; ok && running.Declares(k) {
			return true
		}
	}
	return false
}

// dryRunIgnores are the configuration's attributes no dry run reads: the destroy's and the
// operations' bounds.
var dryRunIgnores = map[string]bool{"deletion_protection": true, "delete_data": true, "timeouts": true}

// configKnownFor says whether every value of the configuration a dry run may read is known (all
// but dryRunIgnores): a dry run of what the plan does not know yet would mislead, whichever value
// the update reads.
func configKnownFor(req resource.ModifyPlanRequest) bool {
	var attrs map[string]tftypes.Value
	if err := req.Config.Raw.As(&attrs); err != nil {
		return false
	}
	for name, v := range attrs {
		if !dryRunIgnores[name] && !v.IsFullyKnown() {
			return false
		}
	}
	return true
}

// notKnownYet is the warning of a plan whose dry run is skipped as what holds not known yet:
// OpenTofu plans again at apply, with it known, and that plan runs the dry run.
func notKnownYet(diags *diag.Diagnostics, what string) {
	diags.AddWarning("No dry run of this plan yet", "If the release changes, its dry run waits for "+what+", not known yet: OpenTofu plans again at apply, with it known, and that plan runs it.")
}

// rerunCertain says, warning so, whether the plan holds a change any new release leaves to the
// re-run, told before the new installer is fetched: split decides only (it refuses nothing),
// against the running release alone.
func (u *update) rerunCertain(ctx context.Context, running *installer.Describe) bool {
	if certain, _ := u.split(ctx, running, running); certainRerun(u.plan, running, certain) {
		noDryRun(u.diags, false)
		return true
	}
	return false
}

// noDryRun is the warning of a plan whose upgrade comes after a re-run: read says whether the new
// installer was read (else what only it tells is the apply's to say).
func noDryRun(diags *diag.Diagnostics, read bool) {
	detail := "The plan also re-runs the running release (inputs, secret inputs, TLS, domains or ports it declares): the upgrade runs on the install that re-run leaves, which a dry run cannot show."
	if !read {
		detail += " The new installer is not read either: what only it tells (another digest of the release installed) is the apply's to say."
	}
	diags.AddWarning("No dry run of this plan", detail+" Apply the re-run alone first to have the upgrade dry-run.")
}

// wholyKnown says whether v and every value in it, at any depth, are known. The framework's own
// values always convert; one that would not is taken as not known.
func wholyKnown(v attr.Value) bool {
	if v == nil {
		return false
	}
	tv, err := v.ToTerraformValue(context.Background())
	return err == nil && tv.IsFullyKnown()
}
