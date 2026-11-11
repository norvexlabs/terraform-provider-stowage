package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// TLSModes are the installer's --tls modes; none is loopback mode.
var TLSModes = []string{"acme", "dns-cloudflare", "byo", "internal"}

var previousType = map[string]attr.Type{
	"version":   types.StringType,
	"revision":  types.Int64Type,
	"installer": types.StringType,
}

type installModel struct {
	ID                    types.String   `tfsdk:"id"`
	Installer             types.String   `tfsdk:"installer"`
	Inputs                types.Map      `tfsdk:"inputs"`
	SecretInputsWO        types.Map      `tfsdk:"secret_inputs_wo"`
	SecretInputsWOVersion types.Int64    `tfsdk:"secret_inputs_wo_version"`
	GivenWO               types.String   `tfsdk:"given_wo"`
	SecretsWO             types.String   `tfsdk:"secrets_wo"`
	TLS                   types.String   `tfsdk:"tls"`
	Domains               types.Map      `tfsdk:"domains"`
	Ports                 types.Map      `tfsdk:"ports"`
	Address               types.String   `tfsdk:"address"`
	AllowRollback         types.Bool     `tfsdk:"allow_rollback"`
	DeletionProtection    types.Bool     `tfsdk:"deletion_protection"`
	DeleteData            types.Bool     `tfsdk:"delete_data"`
	Registry              types.String   `tfsdk:"registry"`
	ShortName             types.String   `tfsdk:"short_name"`
	Version               types.String   `tfsdk:"version"`
	Revision              types.Int64    `tfsdk:"revision"`
	Channel               types.String   `tfsdk:"channel"`
	Status                types.String   `tfsdk:"status"`
	Previous              types.Object   `tfsdk:"previous"`
	Docker                *dockerModel   `tfsdk:"docker"`
	Kubernetes            *k8sModel      `tfsdk:"kubernetes"`
	VM                    *vmModel       `tfsdk:"vm"`
	Timeouts              timeouts.Value `tfsdk:"timeouts"`
}

type dockerModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	User           types.String `tfsdk:"user"`
	PrivateKeyFile types.String `tfsdk:"private_key_file"`
	KnownHostsLine types.String `tfsdk:"known_hosts_line"`
	WorkDir        types.String `tfsdk:"work_dir"`
}

type vmModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	User           types.String `tfsdk:"user"`
	PrivateKeyFile types.String `tfsdk:"private_key_file"`
	KnownHostsLine types.String `tfsdk:"known_hosts_line"`
	Version        types.String `tfsdk:"version"`
	Channel        types.String `tfsdk:"channel"`
	ShortName      types.String `tfsdk:"short_name"`
	HostKeyLine    types.String `tfsdk:"host_key_line"`
	SeedBootID     types.String `tfsdk:"seed_boot_id"`
}

func sshAttributes(hostDesc string) map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"host": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: hostDesc,
		},
		"port": schema.Int64Attribute{
			Optional:            true,
			MarkdownDescription: "SSH port; left out, 22.",
			Validators:          []validator.Int64{int64validator.Between(1, 65535)},
		},
		"user": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "SSH user; required with `host`.",
		},
		"private_key_file": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "Path of the SSH private key on the machine running OpenTofu; left out, ssh's agent and default keys.",
		},
		"known_hosts_line": schema.StringAttribute{
			Optional:            true,
			MarkdownDescription: "The host's `known_hosts` line (`host keytype base64`); required with `host`. That key alone is accepted (`StrictHostKeyChecking=yes`), never one learnt on first contact.",
		},
	}
}

func installSchema(ctx context.Context) schema.Schema {
	docker := sshAttributes("The Docker host, reached over SSH by the system `ssh` binary. Left out, the install is on the machine running OpenTofu.")
	docker["work_dir"] = schema.StringAttribute{
		Optional:            true,
		MarkdownDescription: "The directory on the target holding `<name>.sh` and the install directory `<name>/`; left out, the SSH user's home (or OpenTofu's working directory, locally). First-install only.",
	}
	vm := vmAttributes()

	computedString := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc,
			PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
	}
	return schema.Schema{
		MarkdownDescription: "One install of a Stowage product on one target: a Docker host, a Kubernetes cluster or a Kairos VM. " +
			"Exactly one of the `docker`, `kubernetes` and `vm` blocks.",
		Attributes: map[string]schema.Attribute{
			"id": computedString("`<work_dir>/<name>` on Docker; `<namespace>/<release>` on Kubernetes."),
			"installer": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The installer image by digest, `<prefix>/<name>-installer@sha256:<64 hex>`, as `ship` prints it: an installer carries one release, so the digest is the release. Required with `docker` and `kubernetes`; the VM's release is `vm.version`.",
				Validators:          []validator.String{installerValidator{}},
			},
			"inputs": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Non-secret inputs, given as `--set KEY=VALUE` (on the target's command line, and in state). An input the installer's `describe` marks secret is refused here: give it in `secret_inputs_wo`.",
			},
			"secret_inputs_wo": schema.MapAttribute{
				Optional:            true,
				WriteOnly:           true,
				ElementType:         types.StringType,
				MarkdownDescription: "Secret inputs, write-only (never in plan or state), sent to the target on standard input as an answers file. A change is applied only when `secret_inputs_wo_version` changes.",
			},
			"secret_inputs_wo_version": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Bump it to apply a change of `secret_inputs_wo`.",
			},
			"given_wo": schema.StringAttribute{
				Optional:            true,
				WriteOnly:           true,
				MarkdownDescription: "What another install gives (the installer's `--given` YAML), write-only, used at create, on standard input.",
			},
			"secrets_wo": schema.StringAttribute{
				Optional:            true,
				WriteOnly:           true,
				MarkdownDescription: "A decrypted `secrets export` of a machine made again (the installer's `--secrets`), write-only, used at create, on standard input. Docker only: refused with `kubernetes`, whose release keeps its secrets in the cluster, and with `vm`, whose seed cannot make the first boot wait for it.",
			},
			"tls": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The front's TLS mode (`--tls`): `acme`, `dns-cloudflare`, `byo` or `internal`; left out, loopback mode.",
				Validators:          []validator.String{stringvalidator.OneOf(TLSModes...)},
			},
			"domains": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Route → domain (`--domains route=host,…`), in a TLS mode.",
			},
			"ports": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.Int64Type,
				MarkdownDescription: "Route → port (`--ports route=port,…`).",
			},
			"address": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The loopback address the routes bind (`--address`). First-install only.",
			},
			"allow_rollback": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "Lets a change of `installer` to `previous.installer` run the kept installer's `rollback`. Without it, that change is a plan-time error: a rollback restores the previous values, so a variable edit never triggers one.",
			},
			"deletion_protection": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "While true (the default), destroying the resource is refused. Set it false and apply before a destroy.",
			},
			"delete_data": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "A destroy runs `uninstall --data`, deleting the volumes, instead of `uninstall`, which keeps them.",
			},
			"registry":   computedString("The provider's `registry` at the install, which a later plan compares; null in an imported state until its first apply."),
			"short_name": computedString("The installer's short name (`describe` `short_name`): the install keeps its installer as `<id>/<short_name>-stow`."),
			"version":    computedString("The release installed, from `status --json`."),
			"revision": schema.Int64Attribute{Computed: true, MarkdownDescription: "The installed release's package revision.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"channel": computedString("The channel the install follows."),
			"status":  computedString("`installed`, `installing` (the first install has not finished), `failed: <why>` or `not installed`."),
			"previous": schema.ObjectAttribute{
				Computed: true, AttributeTypes: previousType,
				MarkdownDescription: "The release `rollback` returns to (`version`, `revision`, `installer`); null before an upgrade.",
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()},
			},
		},
		Blocks: map[string]schema.Block{
			"docker": schema.SingleNestedBlock{
				MarkdownDescription: "The Docker shape: the installer runs on the host (`docker run --rm <installer> > <name>.sh`, then `sh <name>.sh --no-input`).",
				Attributes:          docker,
			},
			"kubernetes": k8sBlock(),
			"vm": schema.SingleNestedBlock{
				MarkdownDescription: "The Kairos VM shape, reached over SSH: a VM its hypervisor's or cloud's resource made from `stowage_vm_seed`. `sudo -n <short_name>-stow install|upgrade|rollback --json` and `<short_name>-stow status|describe --json` run on it; destroying the resource forgets it, removing the VM is its own resource's job.",
				Attributes:          vm,
			},
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Read: true, Update: true, Delete: true}),
		},
	}
}

// installerValidator checks the installer reference's form: by digest, of an installer image.
type installerValidator struct{}

var _ validator.String = installerValidator{}

func (installerValidator) Description(context.Context) string {
	return "an installer image by digest: <prefix>/<name>-installer@sha256:<64 hex>"
}

func (v installerValidator) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (installerValidator) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, _, err := parseInstaller(req.ConfigValue.ValueString()); err != nil {
		resp.Diagnostics.AddAttributeError(req.Path, "installer is not pinned by digest", err.Error())
	}
}

var _ resource.Resource = (*installResource)(nil)
