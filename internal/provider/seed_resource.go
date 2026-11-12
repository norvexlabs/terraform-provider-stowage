package provider

import (
	"context"
	"fmt"
	"os"
	pathpkg "path"
	"slices"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// seedTimeout is a seed's create, when the timeouts block leaves it out: the installer image is
// fetched and the registry read.
const seedTimeout = 10 * time.Minute

var diskType = map[string]attr.Type{"format": types.StringType, "image": types.StringType, "os_gib": types.Int64Type, "disk_gib": types.Int64Type, "cloud_image": types.StringType}

var diskFileType = map[string]attr.Type{"path": types.StringType, "format": types.StringType, "sha256": types.StringType, "disk_gib": types.Int64Type}

type seedModel struct {
	ID             types.String   `tfsdk:"id"`
	Installer      types.String   `tfsdk:"installer"`
	Platform       types.String   `tfsdk:"platform"`
	Hostname       types.String   `tfsdk:"hostname"`
	User           types.String   `tfsdk:"user"`
	SSHPublicKey   types.String   `tfsdk:"ssh_public_key"`
	Channel        types.String   `tfsdk:"channel"`
	Network        types.String   `tfsdk:"network"`
	Address        types.String   `tfsdk:"address"`
	Gateway        types.String   `tfsdk:"gateway"`
	DNS            types.List     `tfsdk:"dns"`
	IPv6           types.Bool     `tfsdk:"ipv6"`
	Address6       types.String   `tfsdk:"address6"`
	Gateway6       types.String   `tfsdk:"gateway6"`
	TuneProfile    types.String   `tfsdk:"tune_profile"`
	SSHAllow       types.List     `tfsdk:"ssh_allow"`
	Timezone       types.String   `tfsdk:"timezone"`
	AddHosts       types.Map      `tfsdk:"add_hosts"`
	Interfaces     types.Map      `tfsdk:"interfaces"`
	Device         types.String   `tfsdk:"device"`
	TLS            types.String   `tfsdk:"tls"`
	Domains        types.Map      `tfsdk:"domains"`
	HTTPPort       types.Int64    `tfsdk:"http_port"`
	HTTPSPort      types.Int64    `tfsdk:"https_port"`
	Inputs         types.Map      `tfsdk:"inputs"`
	AwaitSecrets   types.Bool     `tfsdk:"await_secrets"`
	DiskDir        types.String   `tfsdk:"disk_dir"`
	Name           types.String   `tfsdk:"name"`
	ShortName      types.String   `tfsdk:"short_name"`
	Version        types.String   `tfsdk:"version"`
	Revision       types.Int64    `tfsdk:"revision"`
	UserData       types.String   `tfsdk:"user_data"`
	KnownHostsLine types.String   `tfsdk:"known_hosts_line"`
	Fingerprint    types.String   `tfsdk:"fingerprint"`
	HostKeyPrivate types.String   `tfsdk:"host_key_private"`
	Disk           types.Map      `tfsdk:"disk"`
	Waiting        types.List     `tfsdk:"waiting"`
	MetaData       types.String   `tfsdk:"meta_data"`
	AppGiB         types.Int64    `tfsdk:"app_gib"`
	DiskFile       types.Object   `tfsdk:"disk_file"`
	AwaitTLS       types.Bool     `tfsdk:"await_tls"`
	Timeouts       timeouts.Value `tfsdk:"timeouts"`
}

type seedResource struct{ data *Data }

var (
	_ resource.ResourceWithConfigure      = (*seedResource)(nil)
	_ resource.ResourceWithValidateConfig = (*seedResource)(nil)
)

func newSeedResource() resource.Resource { return &seedResource{} }

func (r *seedResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vm_seed"
}

func (r *seedResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	d, ok := req.ProviderData.(*Data)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.data = d
}

func (r *seedResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceS := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	str := func(desc string, v ...validator.String) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, MarkdownDescription: desc, PlanModifiers: replaceS, Validators: v}
	}
	out := func(desc string, sensitive bool) schema.StringAttribute {
		return schema.StringAttribute{Computed: true, Sensitive: sensitive, MarkdownDescription: desc,
			PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}}
	}
	list := func(desc string) schema.ListAttribute {
		return schema.ListAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: desc,
			PlanModifiers: []planmodifier.List{listplanmodifier.RequiresReplace()}}
	}
	smap := func(desc string) schema.MapAttribute {
		return schema.MapAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: desc,
			PlanModifiers: []planmodifier.Map{mapplanmodifier.RequiresReplace()}}
	}
	port := func(desc string) schema.Int64Attribute {
		return schema.Int64Attribute{Optional: true, MarkdownDescription: desc,
			PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
			Validators:    []validator.Int64{int64validator.Between(1, 65535)}}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A new Kairos VM's first-boot material, `vm seed --json` of the release an installer carries, run on the machine running OpenTofu (`docker run --rm <installer> > <name>.sh`, then `sh <name>.sh vm seed --json …`; a linux/amd64 machine with Docker). " +
			"Generated once: its one-time SSH host key would change at every plan otherwise, so every change of an argument replaces it. " +
			"Feed `user_data` and `disk` to the resource that creates the VM (a hypervisor's or a cloud's own provider), and `known_hosts_line` to `stowage_install`'s `vm` block. " +
			"`user_data` and `host_key_private` hold the one-time host key's private key: they are in state, which should be encrypted.",
		Attributes: map[string]schema.Attribute{
			"id": out("The one-time host key's fingerprint.", false),
			"installer": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The installer image by digest, `<prefix>/<name>-installer@sha256:<64 hex>`, as `ship` prints it: its release is the VM's first.",
				Validators:          []validator.String{installerValidator{}},
				PlanModifiers:       replaceS,
			},
			"platform": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Where the VM runs (`--platform`): `truenas`, `proxmox`, `libvirt`, `vmware`, `hyperv`, `azure`, `gcp`, `aws`, `scaleway` or `metal`. It decides the disk format and how the cloud-config reaches the VM; the installer refuses one the registry holds no disk for, naming those it offers.",
				PlanModifiers:       replaceS,
			},
			"hostname":       str("The VM's host name (`--hostname`); left out, the project's name."),
			"user":           str("The VM's administrator, who logs in over SSH (`--user`); left out, the installer's default."),
			"ssh_public_key": str("The administrator's SSH public key (`--ssh-key`, given as a file), e.g. `file(\"~/.ssh/id_ed25519.pub\")`; left out, the installer's default key of the machine running OpenTofu."),
			"channel":        str("The channel the VM's first install follows (`--channel`); left out, the release's."),
			"network":        str("`dhcp` (the default) or `static` (`--network`).", stringvalidator.OneOf("dhcp", "static")),
			"address":        str("With `network = \"static\"`: the address with its prefix, e.g. `192.0.2.10/24` (`--address`)."),
			"gateway":        str("With `network = \"static\"`: the gateway (`--gateway`)."),
			"dns":            list("With `network = \"static\"`: the DNS servers (`--dns`)."),
			"ipv6": schema.BoolAttribute{Optional: true, MarkdownDescription: "Turn IPv6 on (`--ipv6`).",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()}},
			"address6":     str("With `ipv6`: a static IPv6 address with its prefix (`--address6`)."),
			"gateway6":     str("With `address6`: the IPv6 gateway (`--gateway6`)."),
			"tune_profile": str("The VM's tuning profile (`--tune-profile`): `default` or `network`.", stringvalidator.OneOf("default", "network")),
			"ssh_allow":    list("The sources SSH accepts, CIDR (`--ssh-allow`); left out, every source (a cloud's default: the public address of the machine running OpenTofu)."),
			"timezone":     str("The VM's IANA time zone (`--timezone`); left out, UTC."),
			"add_hosts":    smap("Host records, name → IP (`--add-host NAME=IP`)."),
			"interfaces":   smap("NICs the release names, role → spec (`--interface ROLE=SPEC`)."),
			"device":       str("`metal`: the disk the OS installs on (`--device`), wiped."),
			"tls":          str("The front's TLS mode (`--tls`): `acme`, `dns-cloudflare`, `internal` or `byo`. With `byo` the VM's first install waits for the certificate, its key and CA, given over SSH by `stowage_install`'s `given_wo` (its `tls` section); the seed carries none (`await_tls`).", stringvalidator.OneOf(TLSModes...)),
			"domains":      smap("Route → domain (`--domains route=host,…`)."),
			"http_port":    port("The VM's HTTP port in a TLS mode (`--http-port`)."),
			"https_port":   port("The VM's HTTPS port in a TLS mode (`--https-port`)."),
			"inputs":       smap("Non-secret inputs (`--set KEY=VALUE`), in the cloud-config and in state. A secret input is refused by the installer: give it to `stowage_install`'s `secret_inputs_wo`, which goes over SSH once the VM answers."),
			"await_secrets": schema.BoolAttribute{Optional: true, PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplace()},
				MarkdownDescription: "The VM's first install waits for a decrypted `secrets export` (`--await-secrets`): a machine made again, given by `stowage_install`'s `secrets_wo` over SSH once the VM answers. Refused with `tls = \"byo\"` by the installer (the first install waits for one of them)."},
			"disk_dir":   str("An existing directory on the machine running OpenTofu the `platform`'s disk is written into, decompressed (`--disk-dir`): `disk_file` names it, for a hypervisor resource that imports a local file. The file is the caller's: destroying the seed leaves it, and a file of that name already holding the disk is kept."),
			"name":       out("The project's name (`describe`'s).", false),
			"short_name": out("The installer's short name: the VM runs `<short_name>-stow`. Give it to `stowage_install`'s `vm.short_name`.", false),
			"version":    out("The release the seed's disk boots first.", false),
			"revision": schema.Int64Attribute{Computed: true, MarkdownDescription: "That release's package revision.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"user_data":        out("The cloud-config for `platform`: a NoCloud seed's `user-data`, or a cloud's user data. It holds no secret input, but the one-time host key's private key.", true),
			"known_hosts_line": out("The one-time host key as a `known_hosts` line, under the name `vm-seed-host-key`: give it to `stowage_install`'s `vm.known_hosts_line`.", false),
			"fingerprint":      out("The one-time host key's SHA256 fingerprint, as ssh prints it.", false),
			"host_key_private": out("The one-time host key's OpenSSH private key, which the VM serves until its install and the reboot after it.", true),
			"disk": schema.MapAttribute{Computed: true, ElementType: types.ObjectType{AttrTypes: diskType},
				MarkdownDescription: "Per platform the registry holds a disk for, the disk it boots: `format`, `image` (by digest), `os_gib` (what Kairos takes of the VM's disk), `disk_gib` (the VM's disk size to create: `os_gib` plus `app_gib`) and, on a cloud the release publishes to, `cloud_image`, the name `ship --disks` registered its image under there (null otherwise; the ship registered it only when it had that cloud's settings: check it exists).",
				PlanModifiers:       []planmodifier.Map{mapplanmodifier.UseStateForUnknown()}},
			"meta_data": out("The NoCloud `meta-data` beside `user_data` (`instance-id`, `local-hostname`), for a hypervisor that builds its own seed ISO.", false),
			"app_gib": schema.Int64Attribute{Computed: true, MarkdownDescription: "The app's space the release recommends, which each disk's `disk_gib` adds to `os_gib`.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()}},
			"disk_file": schema.ObjectAttribute{Computed: true, AttributeTypes: diskFileType,
				MarkdownDescription: "With `disk_dir`: the `platform`'s disk as a file — `path` (absolute), `format`, `sha256` of its bytes and `disk_gib`; null otherwise.",
				PlanModifiers:       []planmodifier.Object{objectplanmodifier.UseStateForUnknown()}},
			"await_tls": schema.BoolAttribute{Computed: true, MarkdownDescription: "The VM's first install waits for a `byo` certificate given over SSH (`tls = \"byo\"`).",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()}},
			"waiting": schema.ListAttribute{Computed: true, ElementType: types.StringType,
				MarkdownDescription: "The required secret inputs nothing gives, sorted: the VM's first install waits for them, which `stowage_install`'s `secret_inputs_wo` gives over SSH.",
				PlanModifiers:       []planmodifier.List{listplanmodifier.UseStateForUnknown()}},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true}),
		},
	}
}

func (r *seedResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m seedModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if d := m.DiskDir.ValueString(); known(m.DiskDir) && (!strings.HasPrefix(d, "/") || pathpkg.Clean(d) != d) {
		resp.Diagnostics.AddAttributeError(path.Root("disk_dir"), "disk_dir is not an absolute, clean path",
			fmt.Sprintf("%q: give an existing directory as an absolute path (the installer runs in a private directory removed afterwards, which a relative one would name).", d))
	}
	if known(m.TLS) && m.TLS.ValueString() == "byo" && m.AwaitSecrets.ValueBool() {
		resp.Diagnostics.AddAttributeError(path.Root("await_secrets"), "await_secrets with tls byo",
			"The first install waits for one of them: a secrets export (await_secrets) or a byo certificate (tls = \"byo\"). Give the certificate after the restore, by a re-run's given_wo.")
	}
	// By key: a value may not be known yet (a variable at validate, another resource's).
	for _, k := range mapKeys(m.Inputs) {
		if !installer.ValidInputKey(k) {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(k), "not an input key",
				fmt.Sprintf("%q: an input key is a letter or _, then letters, digits or _ (and not tls).", k))
		}
	}
}

func (r *seedResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m seedModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if r.data == nil {
		resp.Diagnostics.AddError("The provider is not configured", "stowage_vm_seed needs the provider's configuration.")
		return
	}
	timeout, diags := m.Timeouts.Create(ctx, seedTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ref := m.Installer.ValueString()
	name, host, err := parseInstaller(ref)
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("installer"), "installer is not pinned by digest", err.Error())
		return
	}
	args := r.seedArgs(ctx, &m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	// The installer runs here, in a private directory of its own.
	work, err := os.MkdirTemp("", "stowage-seed-")
	if err != nil {
		resp.Diagnostics.AddError("A directory for the installer", err.Error())
		return
	}
	defer os.RemoveAll(work)
	run, err := r.data.NewRunner(Connection{})
	if err != nil {
		resp.Diagnostics.AddError("Running the installer here", err.Error())
		return
	}
	d := installer.Docker{Runner: run, WorkDir: work, Name: name}
	var login *installer.Login
	if r.data.Username != "" {
		login = &installer.Login{Host: host, Username: r.data.Username, Password: r.data.Password}
	}
	if err := d.Fetch(ctx, ref, login); err != nil {
		addInstallerError(&resp.Diagnostics, "Fetching the installer", err)
		return
	}
	desc, err := d.Describe(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the installer", err)
		return
	}
	if desc.Name != name {
		resp.Diagnostics.AddAttributeError(path.Root("installer"), "installer of another project",
			fmt.Sprintf("%s describes the project %q, not %q its image is named after.", ref, desc.Name, name))
		return
	}
	if len(desc.Shapes) > 0 && !slices.Contains(desc.Shapes, "kairos") {
		resp.Diagnostics.AddError("No Kairos shape", fmt.Sprintf("%s %s offers %v, not kairos: it has no VM to seed.", desc.Name, desc.Release.Version, desc.Shapes))
		return
	}
	seed, err := d.Seed(ctx, args, r.seedLogin(host))
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Seeding a VM of "+desc.Name, err)
		return
	}
	m.ID = types.StringValue(seed.Fingerprint)
	m.Name = types.StringValue(desc.Name)
	m.ShortName = types.StringValue(desc.ShortName)
	m.Version = types.StringValue(desc.Release.Version)
	m.Revision = types.Int64Value(desc.Release.Revision)
	m.UserData = types.StringValue(seed.UserData)
	m.KnownHostsLine = types.StringValue(seed.KnownHostsLine)
	m.Fingerprint = types.StringValue(seed.Fingerprint)
	m.HostKeyPrivate = types.StringValue(seed.HostKeyPrivate)
	disks := map[string]attr.Value{}
	for p, dk := range seed.Disk {
		cloud := types.StringNull()
		if dk.CloudImage != "" {
			cloud = types.StringValue(dk.CloudImage)
		}
		disks[p] = types.ObjectValueMust(diskType, map[string]attr.Value{"format": types.StringValue(dk.Format), "image": types.StringValue(dk.Image),
			"os_gib": types.Int64Value(dk.OSGiB), "disk_gib": types.Int64Value(dk.DiskGiB), "cloud_image": cloud})
	}
	m.Disk = types.MapValueMust(types.ObjectType{AttrTypes: diskType}, disks)
	m.MetaData = types.StringValue(seed.MetaData)
	m.AppGiB = types.Int64Value(seed.AppGiB)
	m.AwaitTLS = types.BoolValue(seed.AwaitTLS)
	m.DiskFile = types.ObjectNull(diskFileType)
	if f := seed.DiskFile; f != nil {
		m.DiskFile = types.ObjectValueMust(diskFileType, map[string]attr.Value{"path": types.StringValue(f.Path), "format": types.StringValue(f.Format),
			"sha256": types.StringValue(f.SHA256), "disk_gib": types.Int64Value(f.DiskGiB)})
	}
	waiting, d2 := types.ListValueFrom(ctx, types.StringType, seed.Waiting)
	resp.Diagnostics.Append(d2...)
	m.Waiting = waiting
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// seedLogin is the provider's pull account for the seed's own reads of the registry (its disks),
// when that registry is the installer's: the account is the installer registry's (refHost), as
// the fetch's. A provider registry on another host (a mirror) is read with the machine's own
// Docker login, as no login is given then (the pull account is not that registry's: a mirror
// refuses another's credentials); nil without an account.
func (r *seedResource) seedLogin(refHost string) *installer.Login {
	if r.data.Username == "" {
		return nil
	}
	if r.data.Registry != "" && !strings.EqualFold(registryHostOf(r.data.Registry), refHost) {
		return nil
	}
	return &installer.Login{Host: refHost, Username: r.data.Username, Password: r.data.Password}
}

// seedArgs is m as vm seed's flags, with the provider's registry and its CA.
func (r *seedResource) seedArgs(ctx context.Context, m *seedModel, diags *diag.Diagnostics) installer.SeedArgs {
	list := func(l types.List) []string {
		if !known(l) {
			return nil
		}
		var out []string
		diags.Append(l.ElementsAs(ctx, &out, false)...)
		return out
	}
	return installer.SeedArgs{
		Platform: m.Platform.ValueString(), Hostname: m.Hostname.ValueString(), User: m.User.ValueString(),
		Channel: m.Channel.ValueString(), Network: m.Network.ValueString(), Address: m.Address.ValueString(),
		Gateway: m.Gateway.ValueString(), Address6: m.Address6.ValueString(), Gateway6: m.Gateway6.ValueString(),
		TuneProfile: m.TuneProfile.ValueString(), Timezone: m.Timezone.ValueString(), Device: m.Device.ValueString(),
		TLS: m.TLS.ValueString(), IPv6: m.IPv6.ValueBool(), HTTPPort: m.HTTPPort.ValueInt64(), HTTPSPort: m.HTTPSPort.ValueInt64(),
		DNS: list(m.DNS), SSHAllow: list(m.SSHAllow),
		Hosts: stringMap(ctx, m.AddHosts, diags), Interfaces: stringMap(ctx, m.Interfaces, diags),
		Domains: stringMap(ctx, m.Domains, diags), Inputs: stringMap(ctx, m.Inputs, diags),
		SSHPublicKey: m.SSHPublicKey.ValueString(),
		Registry:     r.data.Registry, RegistryCA: r.data.RegistryCA,
		AwaitSecrets: m.AwaitSecrets.ValueBool(), DiskDir: m.DiskDir.ValueString(),
	}
}

// Read keeps state: a seed is generated once, and nothing remote holds it.
func (r *seedResource) Read(context.Context, resource.ReadRequest, *resource.ReadResponse) {}

// Update is reached only by a change of timeouts: every argument replaces the seed.
func (r *seedResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state seedModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Timeouts = plan.Timeouts
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete forgets the seed: it created nothing.
func (r *seedResource) Delete(context.Context, resource.DeleteRequest, *resource.DeleteResponse) {}
