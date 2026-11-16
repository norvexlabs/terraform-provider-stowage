package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
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
	ID                     types.String   `tfsdk:"id"`
	Installer              types.String   `tfsdk:"installer"`
	Inputs                 types.Map      `tfsdk:"inputs"`
	SecretInputsWO         types.Map      `tfsdk:"secret_inputs_wo"`
	SecretInputsWOVersion  types.Int64    `tfsdk:"secret_inputs_wo_version"`
	GivenWO                types.String   `tfsdk:"given_wo"`
	SecretsWO              types.String   `tfsdk:"secrets_wo"`
	TLS                    types.String   `tfsdk:"tls"`
	Domains                types.Map      `tfsdk:"domains"`
	Ports                  types.Map      `tfsdk:"ports"`
	Address                types.String   `tfsdk:"address"`
	AllowRollback          types.Bool     `tfsdk:"allow_rollback"`
	DeletionProtection     types.Bool     `tfsdk:"deletion_protection"`
	DeleteData             types.Bool     `tfsdk:"delete_data"`
	PlanDryRun             types.Bool     `tfsdk:"plan_dry_run"`
	Registry               types.String   `tfsdk:"registry"`
	RegistryCADigest       types.String   `tfsdk:"registry_ca_digest"`
	ShortName              types.String   `tfsdk:"short_name"`
	Version                types.String   `tfsdk:"version"`
	Revision               types.Int64    `tfsdk:"revision"`
	Channel                types.String   `tfsdk:"channel"`
	Status                 types.String   `tfsdk:"status"`
	Previous               types.Object   `tfsdk:"previous"`
	RegistryLoginWO        types.String   `tfsdk:"registry_login_wo"`
	RegistryLoginWOVersion types.Int64    `tfsdk:"registry_login_wo_version"`
	AcmeCA                 types.String   `tfsdk:"acme_ca"`
	AcmeCARoot             types.String   `tfsdk:"acme_ca_root"`
	KitFile                types.String   `tfsdk:"kit_file"`
	KitCodeWO              types.String   `tfsdk:"kit_code_wo"`
	Docker                 *dockerModel   `tfsdk:"docker"`
	Kubernetes             *k8sModel      `tfsdk:"kubernetes"`
	VM                     *vmModel       `tfsdk:"vm"`
	Timeouts               timeouts.Value `tfsdk:"timeouts"`
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
	Platform       types.String `tfsdk:"platform"`
	Interfaces     types.Map    `tfsdk:"interfaces"`
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
			Validators:          portValidators,
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
		MarkdownDescription: "The install model the three install types are views of (split.go): one install on one target, its docker, kubernetes or vm block naming it.",
		Attributes: map[string]schema.Attribute{
			"id": computedString("`<work_dir>/<name>` on Docker; `<namespace>/<release>` on Kubernetes."),
			"installer": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The installer image by digest, `<prefix>/<name>-installer@sha256:<64 hex>`, as `ship` prints it: an installer carries one release, so the digest is the release. A change upgrades the install (the new release's `upgrade`), or rolls it back to `previous.installer` with `allow_rollback`.",
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
				MarkdownDescription: "A decrypted `secrets export` of a machine made again (the installer's `--secrets`), write-only, used at create, on standard input. On a VM, one seeded with `stowage_vm_seed`'s `await_secrets`, whose first install waits for it (the installer refuses it once that install ran).",
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
				MarkdownDescription: "Lets a change of `installer` to `previous.installer` run the kept installer's `rollback`. Without it, that change is a plan-time error: a rollback takes the previous release back (its compose files, installer and images; every input keeps its value) and on a VM reboots, so a variable edit never triggers one.",
			},
			"deletion_protection": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(true),
				MarkdownDescription: "While true (the default), destroying the resource is refused. Set it false and apply before a destroy.",
			},
			"plan_dry_run": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "A plan changing the release runs the change's dry run on the target, so the installer's refusal is a plan error. On Docker, a plan changing `installer`: the new release's `upgrade --dry-run` (fetched into a private directory beside the install and removed after), or the kept installer's `rollback --dry-run`. On Kubernetes, a plan changing `installer`: the new installer's `k8s upgrade --dry-run`, or `k8s rollback --dry-run`, run on this machine as an apply's. On a VM, a plan changing `version`: `upgrade --version V --dry-run --json` or `rollback --dry-run --json` over SSH (the host daemon's checks before its job; no reboot; the VM's OS needs Stowage 0.38.0 or later). A plan that also re-runs the running release is not dry-run, with a warning. Opt-in: the plan reaches the target. On an imported state never applied it runs too, and says what the take-over changes (`Taking over the install: …`, the keys that differ, never a value); while a value it reads is not known yet, skipped with a warning (`No dry run of this plan yet`), as the plan OpenTofu makes again at apply runs it.",
			},
			"delete_data": schema.BoolAttribute{
				Optional: true, Computed: true, Default: booldefault.StaticBool(false),
				MarkdownDescription: "A destroy runs `uninstall --data`, deleting the volumes, instead of `uninstall`, which keeps them.",
			},
			"registry_login_wo": schema.StringAttribute{
				Optional: true, WriteOnly: true,
				MarkdownDescription: "The install's own registry login, write-only: a Docker config holding one `auths` entry for the host of the release's registry or of the provider's `registry` (`{\"auths\": {\"HOST\": {\"auth\": \"BASE64(USER:PASSWORD)\"}}}`), minted for this install, sent on standard input. Docker: the install's `--registry-login-file`, at the first install or a re-run. Kubernetes: `k8s install|upgrade --registry-login-file` (the release's pull Secret written from it; refused with `image_pull_login`). VM: `sudo -n <short>-stow registry-login` over SSH, before its first install is given anything, or alone. Sent at create, and again when `registry_login_wo_version` changes; leaving both out keeps the login the install holds.",
			},
			"registry_login_wo_version": schema.Int64Attribute{
				Optional:            true,
				MarkdownDescription: "Bump it to send `registry_login_wo` again. A refresh that reads the install holding no login (`status --json --values` `pull_login` false) takes it out of state, so the next plan sends the login again.",
			},
			"acme_ca": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Another ACME directory than Let's Encrypt's, its URL (`--acme-ca`), in the `acme` and `dns-cloudflare` modes; taken out, Let's Encrypt's again (`--acme-ca none`). A change re-runs the install. A refresh reads the one the install holds (`status --json --values` `acme_ca`).",
			},
			"acme_ca_root": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "PEM of the root `acme_ca`'s certificates chain to (`--acme-ca-root`, a file on standard input), e.g. `file(\"acme-root.pem\")`; taken out, none (`--acme-ca-root none`). A change re-runs the install. A refresh compares its `sha256` with the one the install holds (`status --json --values` `acme_ca_root`): another puts that digest in state, so the next plan gives the root again.",
			},
			"kit_file": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A handoff kit, the file's path on the machine running OpenTofu, given to the first install (`--kit`, read at create and sent on standard input), with `kit_code_wo`. First-install only: a later change is not applied (a plan warning says so). Refused with `given_wo`.",
			},
			"kit_code_wo": schema.StringAttribute{
				Optional: true, WriteOnly: true,
				MarkdownDescription: "The kit's code (`--kit-code-file`), write-only, sent on standard input with the kit at create; required with `kit_file`.",
			},
			"registry":           computedString("The provider's `registry` at the install, which a later plan compares; null in an imported state until its first apply."),
			"registry_ca_digest": computedString("`sha256:<hex>` of the registry CA the install keeps (the provider's `registry_ca` at its first install), as `status --json` reports it; null when it keeps none. A later plan compares it with the provider's `registry_ca`: first-install only on Docker and the VM. Null in an imported state until its first apply, which records it."),
			"short_name":         computedString("The installer's short name (`describe` `short_name`): the install keeps its installer as `<id>/<short_name>-stow`."),
			"version":            computedString("The release installed, from `status --json`."),
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
