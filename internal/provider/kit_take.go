package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// kitLost is the refusal of a kit expected (its version given) and absent: the ephemeral
// stowage_kit is null outside the apply that issued it.
func kitLost(attr string) string {
	return fmt.Sprintf("kit_wo_version is given and %s is null: a kit stowage_kit issues is kept in the provider process of the apply that issued it, and an apply that failed after issuing it lost it (or kit_wo names nothing). "+
		"Bump the stowage_kit's version to issue a kit again, in the same apply: the project's script decides whether it can be issued again.", attr)
}

// readKitWO reads kit_wo and kit_code_wo from cfg.
func readKitWO(ctx context.Context, cfg tfsdk.Config, diags *diag.Diagnostics) (kit, code string) {
	var k, c types.String
	diags.Append(cfg.GetAttribute(ctx, path.Root("kit_wo"), &k)...)
	diags.Append(cfg.GetAttribute(ctx, path.Root("kit_code_wo"), &c)...)
	return k.ValueString(), c.ValueString()
}

// needKit refuses a kit expected and absent, or a kit without its code.
func needKit(kit, code string, diags *diag.Diagnostics) {
	switch {
	case kit == "":
		diags.AddAttributeError(path.Root("kit_wo"), "The kit is not here", kitLost("kit_wo"))
	case code == "":
		diags.AddAttributeError(path.Root("kit_code_wo"), "kit_code_wo is not given", "A kit opens with its code: give kit_code_wo with kit_wo (the ephemeral stowage_kit's code).")
	}
}

// kitWOAttributes are kit_wo, kit_code_wo and kit_wo_version, described for what takes the kit.
func kitWOAttributes(takes string, versionMods []planmodifier.String) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"kit_wo": schema.StringAttribute{Optional: true, WriteOnly: true,
			MarkdownDescription: "The armored kit, write-only: the ephemeral `stowage_kit`'s `kit`, or a kit made by hand (`file(\"x.kit\")`). Sent on standard input to " + takes + " (`--kit`)."},
		"kit_code_wo": schema.StringAttribute{Optional: true, WriteOnly: true,
			MarkdownDescription: "The kit's code, write-only (the ephemeral `stowage_kit`'s `code`), sent on standard input with the kit (`--kit-code-file`)."},
		"kit_wo_version": schema.StringAttribute{Optional: true, PlanModifiers: versionMods,
			MarkdownDescription: "Says a kit is given: `stowage_kit`'s `id` (new at each issue), or any value naming a kit made by hand. With it, a null `kit_wo` when the kit is given is an error naming the fix (bump the `stowage_kit`'s `version`), never an install without the kit."},
	}
}

type kitTakeModel struct {
	ID           types.String    `tfsdk:"id"`
	KitWO        types.String    `tfsdk:"kit_wo"`
	KitCodeWO    types.String    `tfsdk:"kit_code_wo"`
	KitWOVersion types.String    `tfsdk:"kit_wo_version"`
	Docker       *kitDockerModel `tfsdk:"docker"`
	Kubernetes   *kitK8sModel    `tfsdk:"kubernetes"`
	VM           *kitVMModel     `tfsdk:"vm"`
}

func (m *kitTakeModel) target() kitTarget {
	return kitTarget{Docker: m.Docker, Kubernetes: m.Kubernetes, VM: m.VM}
}

// kitTakeResource is stowage_kit_take: a kit taken by an install that exists, by its re-run.
type kitTakeResource struct{ r installResource }

var (
	_ resource.ResourceWithConfigure      = (*kitTakeResource)(nil)
	_ resource.ResourceWithValidateConfig = (*kitTakeResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*kitTakeResource)(nil)
)

// ModifyPlan keeps id through a change that takes nothing: kit_wo_version the same (how the
// install is reached changed).
func (k *kitTakeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state kitTakeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || !plan.KitWOVersion.Equal(state.KitWOVersion) {
		return
	}
	plan.ID = state.ID
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func newKitTakeResource() resource.Resource { return &kitTakeResource{} }

func (k *kitTakeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kit_take"
}

func (k *kitTakeResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	k.r.Configure(ctx, req, resp)
}

func (k *kitTakeResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	attrs := kitWOAttributes("the install's re-run", nil)
	v := attrs["kit_wo_version"].(schema.StringAttribute)
	v.Optional, v.Required = false, true
	v.MarkdownDescription = "`stowage_kit`'s `id` (new at each issue): a change takes the kit again."
	attrs["kit_wo_version"] = v
	attrs["id"] = schema.StringAttribute{Computed: true, MarkdownDescription: "The kit's `kit_wo_version` it took last."}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A handoff kit taken by an install that exists: the install runs again given it (Docker: its kept installer `--kit FILE --kit-code-file FILE --no-input --json`; Kubernetes: `k8s upgrade --kit` by the installer of the release it runs; VM: `sudo -n <short_name>-stow install --kit … --no-input --json`), the kit and its code files on standard input. " +
			"The provider never opens the kit: the install takes what it holds as `--given`'s. A create, or a change of `kit_wo_version`, takes the kit; a refresh runs nothing; destroying it forgets it, and the inputs it gave stay on the install. " +
			"For a first install, give the kit to the install resource's `kit_wo` instead; this one is for an install that exists (a kit going back to the install that issued the other one).",
		Attributes: attrs,
		Blocks:     kitTargetBlocks("takes the kit"),
	}
}

func (k *kitTakeResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m kitTakeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateKitTarget(m.target(), &resp.Diagnostics)
}

func (k *kitTakeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	k.apply(ctx, req.Plan, req.Config, &resp.State, &resp.Diagnostics)
}

func (k *kitTakeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state kitTakeModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if plan.KitWOVersion.Equal(state.KitWOVersion) {
		// How the install is reached changed alone: recorded, nothing taken.
		plan.ID, plan.KitWO, plan.KitCodeWO = state.ID, types.StringNull(), types.StringNull()
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	k.apply(ctx, req.Plan, req.Config, &resp.State, &resp.Diagnostics)
}

// apply gives the kit to the install, then records the version taken.
func (k *kitTakeResource) apply(ctx context.Context, plan tfsdk.Plan, cfg tfsdk.Config, state *tfsdk.State, diags *diag.Diagnostics) {
	var m kitTakeModel
	diags.Append(plan.Get(ctx, &m)...)
	kit, code := readKitWO(ctx, cfg, diags)
	if diags.HasError() {
		return
	}
	if needKit(kit, code, diags); diags.HasError() {
		return
	}
	ops := k.r.kitOn(ctx, m.target(), diags)
	if ops == nil {
		return
	}
	defer ops.close()
	if !installedThere(ctx, ops, diags) {
		return
	}
	if _, err := ops.take(ctx, kit, code); err != nil {
		addInstallerError(diags, "Taking the kit", err)
	}
	if diags.HasError() {
		return
	}
	m.ID = m.KitWOVersion
	m.KitWO, m.KitCodeWO = types.StringNull(), types.StringNull()
	diags.Append(state.Set(ctx, &m)...)
}

func (k *kitTakeResource) Read(context.Context, resource.ReadRequest, *resource.ReadResponse) {
	// Nothing remote: the kit was taken; what it gave is the install's.
}

func (k *kitTakeResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
	// Forgotten: what the kit gave stays on the install.
}

// defaultKitTimeout bounds stowage_kit_check's wait for a verdict, left out.
const defaultKitTimeout = "10m"

type kitCheckModel struct {
	ID         types.String    `tfsdk:"id"`
	Handoff    types.String    `tfsdk:"handoff"`
	Instance   types.String    `tfsdk:"instance"`
	Triggers   types.Map       `tfsdk:"triggers"`
	Timeout    types.String    `tfsdk:"timeout"`
	Proven     types.Bool      `tfsdk:"proven"`
	Verdict    types.String    `tfsdk:"verdict"`
	FailOnNot  types.Bool      `tfsdk:"fail_if_not_proven"`
	Docker     *kitDockerModel `tfsdk:"docker"`
	Kubernetes *kitK8sModel    `tfsdk:"kubernetes"`
	VM         *kitVMModel     `tfsdk:"vm"`
}

func (m *kitCheckModel) target() kitTarget {
	return kitTarget{Docker: m.Docker, Kubernetes: m.Kubernetes, VM: m.VM}
}

// The verdicts stowage_kit_check records.
const (
	verdictProven    = "proven"
	verdictNotProven = "not proven"
	verdictUnknown   = "unknown"
)

// kitCheckResource is stowage_kit_check: the issuing install's `kit check HANDOFF INSTANCE`,
// tried until it answers or its timeout runs out.
type kitCheckResource struct{ r installResource }

var (
	_ resource.ResourceWithConfigure      = (*kitCheckResource)(nil)
	_ resource.ResourceWithValidateConfig = (*kitCheckResource)(nil)
)

func newKitCheckResource() resource.Resource { return &kitCheckResource{} }

func (k *kitCheckResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kit_check"
}

func (k *kitCheckResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	k.r.Configure(ctx, req, resp)
}

func (k *kitCheckResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Proves a kit was taken: the issuing install's `kit check HANDOFF INSTANCE` (Kubernetes: `k8s kit check`; VM: `sudo -n <short_name>-stow kit check`), run at create and again every poll while it answers unknown (its exit 2, or no answer: ssh, sudo or the installer failing), until it says proven or not proven or `timeout` runs out. " +
			"`proven` records it; by default not proven, or no verdict by the timeout, is an error (`fail_if_not_proven`). A refresh runs the check once more while `proven` is false, so a later plan sees a proof that came after. Destroying it runs nothing.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true, MarkdownDescription: "`HANDOFF/INSTANCE`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"handoff": schema.StringAttribute{Required: true, PlanModifiers: replace, MarkdownDescription: "The handoff, as the issuing release's `describe` lists it."},
			"instance": schema.StringAttribute{Optional: true, PlanModifiers: replace,
				MarkdownDescription: "The instance the kit was issued for (`stowage_kit`'s `instance`); left out, the handoff's name."},
			"triggers": schema.MapAttribute{Optional: true, ElementType: types.StringType, PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace()},
				MarkdownDescription: "Values whose change runs the check again, e.g. `{ take = stowage_kit_take.x.id }` or the target install's `id`: the check runs after what it names."},
			"timeout": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(defaultKitTimeout),
				MarkdownDescription: "How long the check is tried while it answers unknown, a duration; default `" + defaultKitTimeout + "`."},
			"fail_if_not_proven": schema.BoolAttribute{Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "Whether not proven, or no verdict by the timeout, fails the apply (default true). False records `proven` and `verdict` alone, for a `postcondition` or an output to read; the resource is created either way, and a refresh checks again while `proven` is false."},
			"proven": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the check said proven.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}},
			"verdict": schema.StringAttribute{Computed: true, MarkdownDescription: "`proven`, `not proven` or `unknown` (no verdict by the timeout).",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
		},
		Blocks: kitTargetBlocks("issued the kit"),
	}
}

func (k *kitCheckResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m kitCheckModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateKitTarget(m.target(), &resp.Diagnostics)
	if known(m.Timeout) {
		if _, err := kitExpires(m.Timeout.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("timeout"), "timeout is not a duration", err.Error())
		}
	}
}

// instanceOf is the instance m names: its own, or the handoff's name.
func (m *kitCheckModel) instanceOf() string {
	if i := m.Instance.ValueString(); i != "" {
		return i
	}
	return m.Handoff.ValueString()
}

func (k *kitCheckResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m kitCheckModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	timeout, err := kitExpires(m.Timeout.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("timeout"), "timeout is not a duration", err.Error())
		return
	}
	verdict, last := k.check(ctx, &m, timeout, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue(m.Handoff.ValueString() + "/" + m.instanceOf())
	m.Verdict = types.StringValue(verdict)
	m.Proven = types.BoolValue(verdict == verdictProven)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
	if verdict == verdictProven {
		return
	}
	summary, detail := notProven(&m, verdict, timeout, last)
	if m.FailOnNot.ValueBool() {
		// An error after the state is set taints it: the next apply checks again.
		resp.Diagnostics.AddError(summary, detail+" The next apply runs the check again.")
		return
	}
	resp.Diagnostics.AddWarning(summary, detail)
}

// notProven says a check gave no proof: its verdict, and the last error of an unanswered run.
func notProven(m *kitCheckModel, verdict string, timeout time.Duration, last error) (string, string) {
	what := fmt.Sprintf("kit check %s %s", m.Handoff.ValueString(), m.instanceOf())
	if verdict == verdictNotProven {
		return "The kit is not proven", fmt.Sprintf("%s says not proven: the install the kit was for has not taken it, or its check fails. A refresh checks again while proven is false.", what)
	}
	detail := fmt.Sprintf("%s gave no verdict in %s (timeout): it answered unknown each time.", what, timeout)
	if last != nil {
		detail = fmt.Sprintf("%s gave no verdict in %s (timeout); its last run: %v.", what, timeout, last)
	}
	return "The kit's check gave no verdict", detail + " A refresh checks again while proven is false."
}

// check runs the check until it says proven or not proven, or timeout runs out (unknown): the
// verdict, and the last run's error when the last run gave no answer.
func (k *kitCheckResource) check(ctx context.Context, m *kitCheckModel, timeout time.Duration, diags *diag.Diagnostics) (string, error) {
	ops := k.r.kitOn(ctx, m.target(), diags)
	if ops == nil {
		return "", nil
	}
	defer ops.close()
	deadline := time.Now().Add(timeout)
	desc := describeThere(ctx, ops, diags)
	if desc == nil {
		return "", nil
	}
	h := desc.Handoff(m.Handoff.ValueString())
	switch {
	case h == nil:
		diags.AddAttributeError(path.Root("handoff"), "No such handoff", noHandoff(desc, m.Handoff.ValueString()))
		return "", nil
	case !h.Check:
		diags.AddAttributeError(path.Root("handoff"), "A handoff without a check",
			fmt.Sprintf("The handoff %s declares no check: `kit check` answers unknown for it every time. Remove the stowage_kit_check, or give the handoff a check in the project.", h.Name))
		return "", nil
	}
	poll := defaultPoll
	if k.r.data != nil && k.r.data.Poll > 0 {
		poll = k.r.data.Poll
	}
	var last error
	for {
		var v installer.KitCheck
		var err error
		v, err = ops.check(ctx, m.Handoff.ValueString(), m.instanceOf())
		last = err
		switch {
		case err == nil && v == installer.KitProven:
			return verdictProven, nil
		case err == nil && v == installer.KitNotProven:
			return verdictNotProven, nil
		}
		wait := min(poll, time.Until(deadline))
		if wait <= 0 {
			return verdictUnknown, last
		}
		select {
		case <-ctx.Done():
			return verdictUnknown, ctx.Err()
		case <-time.After(wait):
		}
	}
}

func (k *kitCheckResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m kitCheckModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || m.Proven.ValueBool() {
		return
	}
	// One run, no wait: an unreachable install, or another unknown, keeps state as it is.
	var d diag.Diagnostics
	verdict, last := k.check(ctx, &m, 0, &d)
	if d.HasError() || verdict == verdictUnknown {
		why := ""
		switch {
		case d.HasError():
			why = d.Errors()[0].Summary() + ": " + d.Errors()[0].Detail()
		case last != nil:
			why = last.Error()
		default:
			why = "it answered unknown"
		}
		resp.Diagnostics.AddWarning("The kit's check gave no verdict", fmt.Sprintf("kit check %s %s, run again by the refresh: %s. State keeps verdict %s.", m.Handoff.ValueString(), m.instanceOf(), why, m.Verdict.ValueString()))
		return
	}
	m.Verdict = types.StringValue(verdict)
	m.Proven = types.BoolValue(verdict == verdictProven)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (k *kitCheckResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	// timeout and fail_if_not_proven alone change in place: recorded, nothing runs.
	var m, state kitCheckModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID, m.Proven, m.Verdict = state.ID, state.Proven, state.Verdict
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (k *kitCheckResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {
}
