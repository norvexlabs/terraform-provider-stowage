package provider

import (
	"context"
	"fmt"
	pathpkg "path"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// installDataSource reads an install's `status --json` without managing it: a Docker install by
// the installer it keeps, or a VM's by its OS bundle's.
type installDataSource struct{ data *Data }

type installDataModel struct {
	Name             types.String `tfsdk:"name"`
	ShortName        types.String `tfsdk:"short_name"`
	Docker           *dockerModel `tfsdk:"docker"`
	VM               *vmReadModel `tfsdk:"vm"`
	ID               types.String `tfsdk:"id"`
	Installed        types.Object `tfsdk:"installed"`
	Previous         types.Object `tfsdk:"previous"`
	Channel          types.String `tfsdk:"channel"`
	Status           types.String `tfsdk:"status"`
	Inputs           types.List   `tfsdk:"inputs"`
	Registry         types.String `tfsdk:"registry"`
	Address          types.String `tfsdk:"address"`
	RegistryCADigest types.String `tfsdk:"registry_ca_digest"`
}

// vmReadModel is the data source's vm block: the connection alone.
type vmReadModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	User           types.String `tfsdk:"user"`
	PrivateKeyFile types.String `tfsdk:"private_key_file"`
	KnownHostsLine types.String `tfsdk:"known_hosts_line"`
}

var _ datasource.DataSourceWithConfigure = (*installDataSource)(nil)

func newInstallDataSource() datasource.DataSource { return &installDataSource{} }

func (d *installDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_install"
}

func (d *installDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*Data)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	d.data = data
}

func (d *installDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	str := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, MarkdownDescription: desc}
	}
	out := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	conn := map[string]schema.Attribute{
		"host": str("The host, reached by the system `ssh`; required in the `vm` block. Left out in the `docker` block, this machine."),
		"port": schema.Int64Attribute{Optional: true, MarkdownDescription: "SSH port (1–65535); left out, 22.",
			Validators: []validator.Int64{int64validator.Between(1, 65535)}},
		"user":             str("SSH user; required with `host`."),
		"private_key_file": str("The private key's path; left out, ssh's agent and default keys."),
		"known_hosts_line": str("The host's `known_hosts` line; required with `host`. That key alone is accepted."),
	}
	docker := map[string]schema.Attribute{"work_dir": str("The directory holding the install directory `<name>/`, an absolute, clean path; left out, the SSH user's home.")}
	for k, v := range conn {
		docker[k] = v
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an install's `status --json` without managing it: a Docker install (by the installer it keeps, `<work_dir>/<name>/<short_name>-stow status --json`) or a VM (`<short_name>-stow status --json`). Exactly one of the `docker` and `vm` blocks. Nothing on the target changes. A Kubernetes release is read by `stowage_install` (its read needs an installer image).",
		Attributes: map[string]schema.Attribute{
			"name":               schema.StringAttribute{Required: true, MarkdownDescription: "The project's name: the install directory `<name>/` on Docker; compared with the VM's."},
			"short_name":         schema.StringAttribute{Required: true, MarkdownDescription: "The installer's short name (`<short_name>-stow`)."},
			"id":                 out("`<work_dir>/<name>` on Docker (`.` the home), `vm:<host>/<short_name>-stow` on a VM."),
			"installed":          schema.ObjectAttribute{Computed: true, AttributeTypes: previousType, MarkdownDescription: "The release installed (`version`, `revision`, `installer`; `installer` null on a VM); null before the first install."},
			"previous":           schema.ObjectAttribute{Computed: true, AttributeTypes: previousType, MarkdownDescription: "The release `rollback` returns to; null when the install keeps none."},
			"channel":            out("The channel the install follows."),
			"status":             out("`installed`, `installing`, `failed: <why>` or `not installed`."),
			"inputs":             schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The input keys the install records, sorted (never a value); null when the installer does not tell."},
			"registry":           out("The image prefix the install pulls from."),
			"address":            out("The loopback address its routes bind."),
			"registry_ca_digest": out("`sha256:<hex>` of the registry CA the install keeps; null for none (on a VM, also when it cannot read it)."),
		},
		Blocks: map[string]schema.Block{
			"docker": schema.SingleNestedBlock{MarkdownDescription: "A Docker install.", Attributes: docker},
			"vm":     schema.SingleNestedBlock{MarkdownDescription: "A Kairos VM.", Attributes: conn},
		},
	}
}

func (d *installDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m installDataModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if (m.Docker == nil) == (m.VM == nil) {
		resp.Diagnostics.AddError("One target", "Give exactly one of the docker and vm blocks.")
		return
	}
	name, short := m.Name.ValueString(), m.ShortName.ValueString()
	if !installer.ValidName(name) || !installer.ValidShortName(short) {
		resp.Diagnostics.AddAttributeError(path.Root("name"), "not a project's name and short name", fmt.Sprintf("%q, %q: one path element each, as describe reports them.", name, short))
		return
	}
	if d.data == nil {
		resp.Diagnostics.AddError("The provider is not configured", "stowage_install needs the provider's configuration.")
		return
	}
	if m.Docker != nil {
		if w := m.Docker.WorkDir.ValueString(); known(m.Docker.WorkDir) && (!strings.HasPrefix(w, "/") || pathpkg.Clean(w) != w) {
			resp.Diagnostics.AddAttributeError(path.Root("docker").AtName("work_dir"), "work_dir is not an absolute, clean path",
				fmt.Sprintf("%q: give an absolute path with no . or .. element, no // and no trailing /, or leave work_dir out for the SSH user's home.", w))
			return
		}
	}
	// A read's bound, as the resource's.
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var st *installer.Status
	var vmFields *installer.VMStatus
	if m.Docker != nil {
		b := m.Docker
		validateSSH(path.Root("docker"), b.Host, b.User, b.KnownHostsLine, b.PrivateKeyFile, b.Port, true, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		run, err := d.data.NewRunner(Connection{Host: b.Host.ValueString(), Port: int(b.Port.ValueInt64()), User: b.User.ValueString(),
			KeyFile: b.PrivateKeyFile.ValueString(), KnownHostsLine: b.KnownHostsLine.ValueString()})
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
			return
		}
		dk := installer.Docker{Runner: run, WorkDir: workDir(b), Name: name, Short: short}
		if st, err = dk.Status(ctx); err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the install", err)
			return
		}
		if st == nil {
			resp.Diagnostics.AddError("No install", fmt.Sprintf("%s/%s holds no install.", dk.WorkDir, name))
			return
		}
		m.ID = types.StringValue(dk.WorkDir + "/" + name)
	} else {
		b := m.VM
		validateSSH(path.Root("vm"), b.Host, b.User, b.KnownHostsLine, b.PrivateKeyFile, b.Port, false, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		run, err := d.data.NewRunner(Connection{Host: b.Host.ValueString(), Port: int(b.Port.ValueInt64()), User: b.User.ValueString(),
			KeyFile: b.PrivateKeyFile.ValueString(), KnownHostsLine: b.KnownHostsLine.ValueString()})
		if err != nil {
			resp.Diagnostics.AddError("Cannot reach the VM", err.Error())
			return
		}
		if vmFields, err = (installer.VM{Runner: run, Short: short}).Status(ctx); err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the VM", err)
			return
		}
		st = &vmFields.Status
		m.ID = types.StringValue("vm:" + b.Host.ValueString() + "/" + short + "-stow")
	}
	if st.Name != name {
		resp.Diagnostics.AddError("Another project's install", fmt.Sprintf("The install holds %q, not %q.", st.Name, name))
		return
	}
	var im installModel
	applyStatus(&im, st, false)
	m.Status, m.Channel, m.Previous = im.Status, im.Channel, im.Previous
	m.Installed = types.ObjectNull(previousType)
	if st.Installed != nil {
		inst := types.StringNull()
		if st.Installed.Installer != "" {
			inst = types.StringValue(st.Installed.Installer)
		}
		m.Installed = types.ObjectValueMust(previousType, map[string]attr.Value{
			"version": types.StringValue(st.Installed.Version), "revision": types.Int64Value(st.Installed.Revision), "installer": inst})
	}
	m.Inputs = types.ListNull(types.StringType)
	if st.Inputs != nil {
		l, diags := types.ListValueFrom(ctx, types.StringType, []string(*st.Inputs))
		resp.Diagnostics.Append(diags...)
		m.Inputs = l
	}
	orNull := func(s string) types.String {
		if s == "" {
			return types.StringNull()
		}
		return types.StringValue(s)
	}
	m.Registry, m.Address, m.RegistryCADigest = orNull(st.Registry), orNull(st.Address), orNull(st.RegistryCA)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
