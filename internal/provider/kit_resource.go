package provider

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// kitTLSPrefix starts the keys a handoff gives that reach a kit's tls: the target's front.
const kitTLSPrefix = "KIT_TLS_"

// defaultKitExpires is how long a kit stowage_kit issues is good for, left out.
const defaultKitExpires = "1h"

type kitModel struct {
	ID         types.String    `tfsdk:"id"`
	Handoff    types.String    `tfsdk:"handoff"`
	Instance   types.String    `tfsdk:"instance"`
	Answers    types.Map       `tfsdk:"answers"`
	AnswersWO  types.Map       `tfsdk:"answers_wo"`
	Expires    types.String    `tfsdk:"expires"`
	Version    types.Int64     `tfsdk:"version"`
	For        types.String    `tfsdk:"for"`
	Gives      types.List      `tfsdk:"gives"`
	Docker     *kitDockerModel `tfsdk:"docker"`
	Kubernetes *kitK8sModel    `tfsdk:"kubernetes"`
	VM         *kitVMModel     `tfsdk:"vm"`
}

func (m *kitModel) target() kitTarget {
	return kitTarget{Docker: m.Docker, Kubernetes: m.Kubernetes, VM: m.VM}
}

// kitResource is stowage_kit: a handoff kit issued by an install, kept in this provider process
// for the ephemeral stowage_kit, which hands it to what takes it in the same apply.
type kitResource struct{ r installResource }

var (
	_ resource.ResourceWithConfigure      = (*kitResource)(nil)
	_ resource.ResourceWithValidateConfig = (*kitResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*kitResource)(nil)
)

// reissues says whether the change from state to plan issues a kit: answers, expires or version
// changed. Another change (how the install is reached) runs nothing.
func reissues(plan, state *kitModel) bool {
	return !plan.Answers.Equal(state.Answers) || !plan.Expires.Equal(state.Expires) || !plan.Version.Equal(state.Version)
}

// ModifyPlan keeps the kit's id, for and gives through a change that issues nothing.
func (k *kitResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	var plan, state kitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() || reissues(&plan, &state) {
		return
	}
	plan.ID, plan.For, plan.Gives = state.ID, state.For, state.Gives
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func newKitResource() resource.Resource { return &kitResource{} }

func (k *kitResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kit"
}

func (k *kitResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	k.r.Configure(ctx, req, resp)
}

func (k *kitResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A handoff kit issued by an install (`kit create HANDOFF --json`, the request on standard input or in a private file, never on a command line), " +
			"for the ephemeral `stowage_kit` to hand to what takes it in the same apply: `stowage_*_install`'s `kit_wo`, or `stowage_kit_take`'s. " +
			"The kit and its code are kept in the provider's process alone: never in state, a plan, a file of the root or a log (the runs that issue and take it pass it through a private temporary directory, removed when they end). A create, or a change of `answers`, `expires` or `version`, issues a kit; a refresh runs nothing. " +
			"Destroying it forgets the kit: what the handoff issued is the project's to revoke.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Computed: true,
				MarkdownDescription: "A random id, new at each issue: the ephemeral `stowage_kit` reads the kit by it."},
			"handoff": schema.StringAttribute{Required: true, PlanModifiers: replace,
				MarkdownDescription: "The handoff's name, as the issuing release's `describe --json` lists it in `handoffs`."},
			"instance": schema.StringAttribute{Optional: true, PlanModifiers: replace,
				MarkdownDescription: "The instance the kit is issued for; left out, the handoff's name (the installer refuses that when the handoff asks the instance)."},
			"answers": schema.MapAttribute{Optional: true, ElementType: types.StringType,
				MarkdownDescription: "The answers to the handoff's questions that are not secret, by key (`describe` `handoffs[].ask`). A question `secret` there given here is refused before anything is issued (`A secret answer in answers`): give it in `answers_wo`, as this is in plan and state. A change issues a kit again."},
			"answers_wo": schema.MapAttribute{Optional: true, WriteOnly: true, ElementType: types.StringType,
				MarkdownDescription: "The answers to the secret questions (and any other), write-only: sent in the request at each issue, never kept. A change of it alone issues nothing: bump `version`."},
			"expires": schema.StringAttribute{Optional: true, Computed: true, Default: stringdefault.StaticString(defaultKitExpires),
				MarkdownDescription: "How long the kit is good for, a duration (`30m`, `1h`, `168h`), after now; default `" + defaultKitExpires + "`. The installer caps it. A change issues a kit again."},
			"version": schema.Int64Attribute{Optional: true,
				MarkdownDescription: "Bump it to issue a kit again (with `answers_wo` as the configuration holds it), e.g. after an apply that issued one failed before it was taken."},
			"for": schema.StringAttribute{Computed: true,
				MarkdownDescription: "The project whose install takes the kit (`describe` `handoffs[].for`)."},
			"gives": schema.ListAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The keys the kit gives (`describe` `handoffs[].gives`)."},
		},
		Blocks: kitTargetBlocks("issues the kit"),
	}
}

func (k *kitResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m kitModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateKitTarget(m.target(), &resp.Diagnostics)
	if known(m.Expires) {
		if _, err := kitExpires(m.Expires.ValueString()); err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("expires"), "expires is not a duration", err.Error())
		}
	}
	if known(m.Answers) && known(m.AnswersWO) {
		for _, key := range slices.Sorted(maps.Keys(m.Answers.Elements())) {
			if _, ok := m.AnswersWO.Elements()[key]; ok {
				resp.Diagnostics.AddAttributeError(path.Root("answers").AtMapKey(key), "An answer given twice",
					fmt.Sprintf("%s is in both answers and answers_wo: give it in one of them.", key))
			}
		}
	}
}

// kitExpires is expires as a duration after now: a Go duration, more than zero.
func kitExpires(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q: give a duration such as 30m, 1h or 168h (%v)", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q: a kit expires after now: give a duration more than zero", s)
	}
	return d, nil
}

func (k *kitResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m kitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	k.issue(ctx, &m, req.Config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (k *kitResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m, state kitModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !reissues(&m, &state) {
		resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
		return
	}
	k.issue(ctx, &m, req.Config, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if k.r.data != nil && k.r.data.Kits != nil {
		k.r.data.Kits.forget(state.ID.ValueString())
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// issue issues m's kit on its install and keeps it in the process, under m's new id: the
// handoff read from the release's describe, a secret question answered in answers and a handoff
// giving TLS material refused before anything is issued.
func (k *kitResource) issue(ctx context.Context, m *kitModel, cfg tfsdk.Config, diags *diag.Diagnostics) {
	if k.r.data == nil || k.r.data.Kits == nil {
		diags.AddError("The provider is not configured", "the provider block was not read")
		return
	}
	var wo types.Map
	diags.Append(cfg.GetAttribute(ctx, path.Root("answers_wo"), &wo)...)
	answers := stringMap(ctx, m.Answers, diags)
	secret := stringMap(ctx, wo, diags)
	expires, err := kitExpires(m.Expires.ValueString())
	if err != nil {
		diags.AddAttributeError(path.Root("expires"), "expires is not a duration", err.Error())
	}
	if diags.HasError() {
		return
	}
	ops := k.r.kitOn(ctx, m.target(), diags)
	if ops == nil {
		return
	}
	defer ops.close()
	handoff := m.Handoff.ValueString()
	func() {
		desc := describeThere(ctx, ops, diags)
		if desc == nil {
			return
		}
		h := desc.Handoff(handoff)
		if h == nil {
			diags.AddAttributeError(path.Root("handoff"), "No such handoff", noHandoff(desc, handoff))
			return
		}
		if refuseKitHandoff(h, answers, diags); diags.HasError() {
			return
		}
		all := map[string]string{}
		maps.Copy(all, answers)
		maps.Copy(all, secret)
		issued, err := ops.create(ctx, handoff, installer.KitRequest{Instance: m.Instance.ValueString(), Answers: all, Expires: expires.Nanoseconds()})
		if err != nil {
			addInstallerError(diags, "Issuing the kit "+handoff, err)
			return
		}
		id, err := k.r.data.Kits.put(*issued)
		if err != nil {
			diags.AddError("Keeping the kit", fmt.Sprintf("%v; %v", err, installer.ErrKitLost))
			return
		}
		m.ID = types.StringValue(id)
		m.For = types.StringValue(h.For)
		gives, d := types.ListValueFrom(ctx, types.StringType, h.Gives)
		diags.Append(d...)
		m.Gives = gives
	}()
}

// noHandoff says the release issues no handoff of that name, and which it issues.
func noHandoff(desc *installer.Describe, name string) string {
	if len(desc.Handoffs) == 0 {
		return fmt.Sprintf("%s %s issues no kit (describe --json lists no handoffs; an installer made before Stowage %s lists none): the handoff %q is not one of its.",
			desc.Name, desc.Release.Version, installer.KitMinStowage, name)
	}
	names := make([]string, 0, len(desc.Handoffs))
	for _, h := range desc.Handoffs {
		names = append(names, h.Name)
	}
	return fmt.Sprintf("%s %s issues no handoff %q; it issues %s.", desc.Name, desc.Release.Version, name, strings.Join(names, ", "))
}

// refuseKitHandoff refuses, before anything is issued, what the provider cannot keep: a secret
// question answered in answers (which state holds), and a handoff giving TLS material (a kit's
// tls moves its target's front to byo, which the target's install resource does not hold).
func refuseKitHandoff(h *installer.Handoff, answers map[string]string, diags *diag.Diagnostics) {
	for _, a := range h.Ask {
		if _, ok := answers[a.Key]; ok && a.Secret {
			diags.AddAttributeError(path.Root("answers").AtMapKey(a.Key), "A secret answer in answers",
				fmt.Sprintf("The handoff %s asks %s as a secret (%s), and answers is kept in state: give it in answers_wo.", h.Name, a.Key, a.Prompt))
		}
	}
	if tls := slices.DeleteFunc(slices.Clone(h.Gives), func(k string) bool { return !strings.HasPrefix(k, kitTLSPrefix) }); len(tls) > 0 {
		diags.AddAttributeError(path.Root("handoff"), "A handoff giving TLS material",
			fmt.Sprintf("The handoff %s gives %s, which a kit carries as the target's front TLS: taking it moves the target to tls byo, which its install resource's tls does not say. Issue it by hand (`<short>-stow kit create %s`) and give it with --kit, outside OpenTofu.",
				h.Name, strings.Join(tls, ", "), h.Name))
	}
}

func (k *kitResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// Nothing remote: the kit was issued; the process kept it while it ran.
}

func (k *kitResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m kitModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if k.r.data != nil && k.r.data.Kits != nil {
		k.r.data.Kits.forget(m.ID.ValueString())
	}
}

// kitEphemeral is the ephemeral stowage_kit: the kit stowage_kit issued in this provider process,
// by its id; null when this process holds none (a later plan or apply, or another provider
// configuration's process).
type kitEphemeral struct{ data *Data }

type kitEphemeralModel struct {
	ID   types.String `tfsdk:"id"`
	Kit  types.String `tfsdk:"kit"`
	Code types.String `tfsdk:"code"`
}

var _ ephemeral.EphemeralResourceWithConfigure = (*kitEphemeral)(nil)

func newKitEphemeral() ephemeral.EphemeralResource { return &kitEphemeral{} }

func (e *kitEphemeral) Metadata(_ context.Context, req ephemeral.MetadataRequest, resp *ephemeral.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_kit"
}

func (e *kitEphemeral) Configure(_ context.Context, req ephemeral.ConfigureRequest, resp *ephemeral.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	d, ok := req.ProviderData.(*Data)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	e.data = d
}

func (e *kitEphemeral) Schema(_ context.Context, _ ephemeral.SchemaRequest, resp *ephemeral.SchemaResponse) {
	resp.Schema = eschema.Schema{
		MarkdownDescription: "The kit `stowage_kit` issued, in the same apply, for a write-only attribute: `kit_wo` and `kit_code_wo` of `stowage_*_install` or `stowage_kit_take`. " +
			"Null outside the apply that issued it (a later plan, another provider configuration): the kit was kept in that apply's provider process alone. Use the same provider configuration as the `stowage_kit`.",
		Attributes: map[string]eschema.Attribute{
			"id":   eschema.StringAttribute{Required: true, MarkdownDescription: "`stowage_kit`'s `id`."},
			"kit":  eschema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "The armored kit."},
			"code": eschema.StringAttribute{Computed: true, Sensitive: true, MarkdownDescription: "Its code."},
		},
	}
}

func (e *kitEphemeral) Open(ctx context.Context, req ephemeral.OpenRequest, resp *ephemeral.OpenResponse) {
	var m kitEphemeralModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	m.Kit, m.Code = types.StringNull(), types.StringNull()
	if e.data != nil && e.data.Kits != nil {
		if k, ok := e.data.Kits.get(m.ID.ValueString()); ok {
			m.Kit, m.Code = types.StringValue(k.Kit), types.StringValue(k.Code)
		}
	}
	resp.Diagnostics.Append(resp.Result.Set(ctx, &m)...)
}
