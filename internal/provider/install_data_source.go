package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// installDataSource reads an install's `status --json --values` without managing it, one type per
// shape: a Docker install by the installer it keeps, a VM's by its OS bundle's, a Kubernetes
// release by the installer image given, run on the machine running OpenTofu.
type installDataSource struct {
	kind string
	data *Data
}

// installDataModel is every type's model: the attributes a type does not have stay null, and are
// neither read nor written (dataAttributes).
type installDataModel struct {
	Name           types.String
	ShortName      types.String
	Host           types.String
	Port           types.Int64
	User           types.String
	PrivateKeyFile types.String
	KnownHostsLine types.String
	WorkDir        types.String
	Installer      types.String
	Namespace      types.String
	Release        types.String

	ID               types.String
	Installed        types.Object
	Previous         types.Object
	Channel          types.String
	Status           types.String
	Inputs           types.List
	Registry         types.String
	Address          types.String
	RegistryCADigest types.String
	PullLogin        types.Bool
}

// dataAttributes are a type's own arguments.
var dataAttributes = map[string][]string{
	"docker":     {"name", "short_name", "host", "port", "user", "private_key_file", "known_hosts_line", "work_dir"},
	"vm":         {"name", "short_name", "host", "port", "user", "private_key_file", "known_hosts_line"},
	"kubernetes": {"installer", "namespace", "release"},
}

var _ datasource.DataSourceWithConfigure = (*installDataSource)(nil)

func newInstallDataSource(kind string) func() datasource.DataSource {
	return func() datasource.DataSource { return &installDataSource{kind: kind} }
}

func (d *installDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = shapes[d.kind].typeName(req.ProviderTypeName)
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
	req := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Required: true, MarkdownDescription: desc}
	}
	opt := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, MarkdownDescription: desc}
	}
	out := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, MarkdownDescription: desc}
	}
	attrs := map[string]schema.Attribute{
		"installed":          schema.ObjectAttribute{Computed: true, AttributeTypes: previousType, MarkdownDescription: "The release installed (`version`, `revision`, `installer`; `installer` null on a VM); null before the first install."},
		"previous":           schema.ObjectAttribute{Computed: true, AttributeTypes: previousType, MarkdownDescription: "The release `rollback` returns to; null when the install keeps none."},
		"channel":            out("The channel the install follows."),
		"status":             out("`installed`, `installing`, `failed: <why>` or `not installed`."),
		"inputs":             schema.ListAttribute{Computed: true, ElementType: types.StringType, MarkdownDescription: "The input keys the install records, sorted (never a value); null when the installer does not tell."},
		"registry":           out("The image prefix the install pulls from."),
		"registry_ca_digest": out("`sha256:<hex>` of the registry CA the install keeps; null for none (on a VM, also when it cannot read it)."),
		"pull_login":         schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether the install holds its own registry login (`status --json --values` `pull_login`; on Kubernetes the release's pull Secret); null from an installer that does not tell (before Stowage 0.40.0)."},
	}
	var desc string
	switch d.kind {
	case "kubernetes":
		desc = "Reads a Kubernetes release's `k8s status --json --values` without managing it, on the cluster the provider's `kubernetes` block reaches: the installer image's entry for the machine running OpenTofu (read from the registry, checked against the release's keys) runs there, with `helm` and `kubectl`. Nothing on the cluster changes."
		attrs["installer"] = schema.StringAttribute{Required: true, Validators: []validator.String{installerValidator{}},
			MarkdownDescription: "An installer image of the release's project by digest (`<prefix>/<name>-installer@sha256:<64 hex>`): its `k8s status` reads the release."}
		attrs["namespace"] = req("The release's namespace.")
		attrs["release"] = opt("The helm release's name; left out, the project's name.")
		attrs["id"] = out("`<namespace>/<release>`.")
	default:
		attrs["port"] = schema.Int64Attribute{Optional: true, MarkdownDescription: "SSH port (1–65535); left out, 22.", Validators: portValidators}
		attrs["user"] = opt("SSH user; required with `host`.")
		attrs["private_key_file"] = opt("The private key's path; left out, ssh's agent and default keys.")
		attrs["known_hosts_line"] = opt("The host's `known_hosts` line; required with `host`. That key alone is accepted.")
		attrs["name"] = req("The project's name: the install directory `<name>/` on Docker; compared with the VM's.")
		attrs["short_name"] = req("The installer's short name (`<short_name>-stow`).")
		attrs["address"] = out("The loopback address its routes bind.")
		if d.kind == "docker" {
			desc = "Reads a Docker install's `status --json --values` without managing it, by the installer it keeps (`<work_dir>/<name>/<short_name>-stow status --json --values`), on a host reached over SSH or on the machine running OpenTofu. Nothing on the target changes."
			attrs["host"] = opt("The Docker host, reached by the system `ssh`; left out, the machine running OpenTofu.")
			attrs["work_dir"] = opt("The directory holding the install directory `<name>/`, an absolute, clean path; left out, the SSH user's home.")
			attrs["id"] = out("`<work_dir>/<name>` (`.` the home).")
		} else {
			desc = "Reads a Kairos VM's `<short_name>-stow status --json --values` over SSH without managing it. Nothing on the VM changes."
			attrs["host"] = req("The VM's address, reached by the system `ssh`.")
			attrs["id"] = out("`<host>/<short_name>-stow`.")
		}
	}
	resp.Schema = schema.Schema{MarkdownDescription: desc, Attributes: attrs}
}

// strField is m's string argument a.
func (m *installDataModel) strField(a string) *types.String {
	return map[string]*types.String{"name": &m.Name, "short_name": &m.ShortName, "host": &m.Host, "user": &m.User,
		"private_key_file": &m.PrivateKeyFile, "known_hosts_line": &m.KnownHostsLine, "work_dir": &m.WorkDir,
		"installer": &m.Installer, "namespace": &m.Namespace, "release": &m.Release}[a]
}

func (d *installDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var m installDataModel
	for _, a := range dataAttributes[d.kind] {
		if a == "port" {
			resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(a), &m.Port)...)
		} else {
			resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(a), m.strField(a))...)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if d.data == nil {
		resp.Diagnostics.AddError("The provider is not configured", "The data source needs the provider's configuration.")
		return
	}
	// A read's bound, as the resources'.
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var st *installer.Status
	switch d.kind {
	case "kubernetes":
		st = d.readK8s(ctx, &m, &resp.Diagnostics)
	case "docker":
		st = d.readDocker(ctx, &m, &resp.Diagnostics)
	default:
		st = d.readVM(ctx, &m, &resp.Diagnostics)
	}
	if st == nil {
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
	m.Registry, m.RegistryCADigest, m.Address = orNull(st.Registry), orNull(st.RegistryCA), orNull(st.Address)
	m.PullLogin = types.BoolNull()
	if st.Values != nil && st.Values.PullLogin != nil {
		m.PullLogin = types.BoolValue(*st.Values.PullLogin)
	}
	put := func(a string, v any) { resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(a), v)...) }
	for _, a := range dataAttributes[d.kind] {
		if a == "port" {
			put(a, m.Port)
		} else {
			put(a, *m.strField(a))
		}
	}
	if d.kind != "kubernetes" {
		put("address", m.Address)
	}
	for a, v := range map[string]any{"id": m.ID, "installed": m.Installed, "previous": m.Previous, "channel": m.Channel, "status": m.Status,
		"inputs": m.Inputs, "registry": m.Registry, "registry_ca_digest": m.RegistryCADigest, "pull_login": m.PullLogin} {
		put(a, v)
	}
}

// names are m's project name and short name, refused unless each is one path element.
func names(m *installDataModel, diags *diag.Diagnostics) (string, string, bool) {
	name, short := m.Name.ValueString(), m.ShortName.ValueString()
	if !installer.ValidName(name) || !installer.ValidShortName(short) {
		diags.AddAttributeError(path.Root("name"), "not a project's name and short name", fmt.Sprintf("%q, %q: one path element each, as describe reports them.", name, short))
		return "", "", false
	}
	return name, short, true
}

func (d *installDataSource) readDocker(ctx context.Context, m *installDataModel, diags *diag.Diagnostics) *installer.Status {
	name, short, ok := names(m, diags)
	if !ok {
		return nil
	}
	if checkWorkDir(m.WorkDir, diags); diags.HasError() {
		return nil
	}
	if validateSSH(path.Empty(), m.Host, m.User, m.KnownHostsLine, m.PrivateKeyFile, m.Port, true, diags); diags.HasError() {
		return nil
	}
	run, err := d.data.NewRunner(Connection{Host: m.Host.ValueString(), Port: int(m.Port.ValueInt64()), User: m.User.ValueString(),
		KeyFile: m.PrivateKeyFile.ValueString(), KnownHostsLine: m.KnownHostsLine.ValueString()})
	if err != nil {
		diags.AddError("Cannot reach the Docker target", err.Error())
		return nil
	}
	dk := installer.Docker{Runner: run, WorkDir: workDir(&dockerModel{WorkDir: m.WorkDir}), Name: name, Short: short}
	st, err := dk.StatusOf(ctx, true)
	if err != nil {
		addInstallerError(diags, "Reading the install", err)
		return nil
	}
	if st == nil {
		diags.AddError("No install", fmt.Sprintf("%s/%s holds no install.", dk.WorkDir, name))
		return nil
	}
	if st.Name != name {
		diags.AddError("Another project's install", fmt.Sprintf("The install holds %q, not %q.", st.Name, name))
		return nil
	}
	m.ID = types.StringValue(dk.WorkDir + "/" + name)
	return st
}

func (d *installDataSource) readVM(ctx context.Context, m *installDataModel, diags *diag.Diagnostics) *installer.Status {
	name, short, ok := names(m, diags)
	if !ok {
		return nil
	}
	if validateSSH(path.Empty(), m.Host, m.User, m.KnownHostsLine, m.PrivateKeyFile, m.Port, false, diags); diags.HasError() {
		return nil
	}
	run, err := d.data.NewRunner(Connection{Host: m.Host.ValueString(), Port: int(m.Port.ValueInt64()), User: m.User.ValueString(),
		KeyFile: m.PrivateKeyFile.ValueString(), KnownHostsLine: m.KnownHostsLine.ValueString()})
	if err != nil {
		diags.AddError("Cannot reach the VM", err.Error())
		return nil
	}
	vs, err := (installer.VM{Runner: run, Short: short}).StatusOf(ctx, true)
	if err != nil {
		addInstallerError(diags, "Reading the VM", err)
		return nil
	}
	if vs.Name != name {
		diags.AddError("Another project's install", fmt.Sprintf("The install holds %q, not %q.", vs.Name, name))
		return nil
	}
	m.ID = types.StringValue(m.Host.ValueString() + "/" + short + "-stow")
	return &vs.Status
}

func (d *installDataSource) readK8s(ctx context.Context, m *installDataModel, diags *diag.Diagnostics) *installer.Status {
	r := &installResource{data: d.data}
	im := &installModel{Installer: m.Installer, Kubernetes: &k8sModel{Namespace: m.Namespace, Release: m.Release}}
	if validateK8s(im, diags); diags.HasError() {
		return nil
	}
	name := installName(im)
	run := r.k8sStart(ctx, im, name, diags)
	if run == nil {
		return nil
	}
	defer run.close()
	k, ok := run.in("run", diags)
	if !ok {
		return nil
	}
	if err := run.script(ctx, k, m.Installer.ValueString()); err != nil {
		addInstallerError(diags, "Fetching the installer", err)
		return nil
	}
	st, err := k.StatusOf(ctx, true)
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		return nil
	}
	if st == nil {
		diags.AddError("No release", fmt.Sprintf("The namespace holds no release %s.", k8sID(k)))
		return nil
	}
	if st.Name != name {
		diags.AddError("Another project's release", fmt.Sprintf("The release %s holds %q, not %q.", k8sID(k), st.Name, name))
		return nil
	}
	m.ID = types.StringValue(k8sID(k))
	return st
}
