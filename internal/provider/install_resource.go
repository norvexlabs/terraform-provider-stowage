package provider

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// Default timeouts, when the timeouts block leaves one out.
const (
	createTimeout = 30 * time.Minute
	readTimeout   = 5 * time.Minute
	deleteTimeout = 15 * time.Minute
)

type installResource struct {
	data *Data
}

var (
	_ resource.ResourceWithConfigure        = (*installResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*installResource)(nil)
	_ resource.ResourceWithValidateConfig   = (*installResource)(nil)
	_ resource.ResourceWithConfigValidators = (*installResource)(nil)
)

func newInstallResource() resource.Resource { return &installResource{} }

func parseInstaller(ref string) (string, string, error) { return installer.ParseInstaller(ref) }

func (r *installResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_install"
}

func (r *installResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = installSchema(ctx)
}

func (r *installResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *installResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.ExactlyOneOf(path.MatchRoot("docker"), path.MatchRoot("kubernetes"), path.MatchRoot("vm")),
	}
}

func known(v attr.Value) bool { return !v.IsNull() && !v.IsUnknown() }

func stringMap(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if !known(m) {
		return nil
	}
	out := map[string]string{}
	diags.Append(m.ElementsAs(ctx, &out, false)...)
	return out
}

func (r *installResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var m installModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	switch {
	case m.VM != nil && !m.Installer.IsNull():
		resp.Diagnostics.AddAttributeError(path.Root("installer"), "installer is not the VM's release",
			"A VM's release is vm.version (its disk carried the first): remove installer.")
	case m.VM == nil && m.Installer.IsNull() && (m.Docker != nil || m.Kubernetes != nil):
		resp.Diagnostics.AddAttributeError(path.Root("installer"), "installer is required",
			"Give the installer image by digest, <prefix>/<name>-installer@sha256:<64 hex>, as `ship` prints it.")
	}
	if m.Docker != nil {
		validateSSH(path.Root("docker"), m.Docker.Host, m.Docker.User, m.Docker.KnownHostsLine, m.Docker.PrivateKeyFile, m.Docker.Port, true, &resp.Diagnostics)
		if known(m.Docker.WorkDir) && strings.TrimSpace(m.Docker.WorkDir.ValueString()) == "" {
			resp.Diagnostics.AddAttributeError(path.Root("docker").AtName("work_dir"), "work_dir is empty", "Give a directory, or leave work_dir out for the SSH user's home.")
		}
	}
	if m.VM != nil {
		validateSSH(path.Root("vm"), m.VM.Host, m.VM.User, m.VM.KnownHostsLine, m.VM.PrivateKeyFile, m.VM.Port, false, &resp.Diagnostics)
		if m.VM.Version.IsNull() {
			resp.Diagnostics.AddAttributeError(path.Root("vm").AtName("version"), "vm.version is required", "Give the release the VM runs.")
		}
	}
	if m.Kubernetes != nil && m.Kubernetes.Namespace.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("kubernetes").AtName("namespace"), "kubernetes.namespace is required", "Give the namespace the release lives in.")
	}

	inputs := stringMap(ctx, m.Inputs, &resp.Diagnostics)
	for _, k := range slices.Sorted(maps.Keys(inputs)) {
		if !installer.ValidInputKey(k) {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(k), "not an input key",
				fmt.Sprintf("%q: an input key is a letter or _, then letters, digits or _ (and not tls).", k))
		}
	}
	secrets := stringMap(ctx, m.SecretInputsWO, &resp.Diagnostics)
	for _, k := range slices.Sorted(maps.Keys(secrets)) {
		if !installer.ValidInputKey(k) {
			resp.Diagnostics.AddAttributeError(path.Root("secret_inputs_wo"), "not an input key",
				fmt.Sprintf("%q: an input key is a letter or _, then letters, digits or _ (and not tls).", k))
		}
		if _, ok := inputs[k]; ok {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(k), "input given twice",
				fmt.Sprintf("%s is in inputs and in secret_inputs_wo: keep it in one.", k))
		}
	}
	if known(m.SecretInputsWO) && m.SecretInputsWOVersion.IsNull() {
		resp.Diagnostics.AddAttributeWarning(path.Root("secret_inputs_wo_version"), "secret_inputs_wo_version is not set",
			"A write-only value is never compared: without a version to bump, a later change of secret_inputs_wo is never applied.")
	}
	for _, k := range slices.Sorted(maps.Keys(stringMap(ctx, m.Domains, &resp.Diagnostics))) {
		if !installer.ValidRoute(k) {
			resp.Diagnostics.AddAttributeError(path.Root("domains").AtMapKey(k), "not a route name", fmt.Sprintf("%q is not a route name.", k))
		}
	}
	if known(m.Ports) {
		ports := map[string]int64{}
		resp.Diagnostics.Append(m.Ports.ElementsAs(ctx, &ports, false)...)
		for _, k := range slices.Sorted(maps.Keys(ports)) {
			if !installer.ValidRoute(k) {
				resp.Diagnostics.AddAttributeError(path.Root("ports").AtMapKey(k), "not a route name", fmt.Sprintf("%q is not a route name.", k))
			}
			if p := ports[k]; p < 1 || p > 65535 {
				resp.Diagnostics.AddAttributeError(path.Root("ports").AtMapKey(k), "port out of range", fmt.Sprintf("%d is not a port (1-65535).", p))
			}
		}
	}
}

func validateSSH(p path.Path, host, user, line, key types.String, port types.Int64, localAllowed bool, diags *diag.Diagnostics) {
	if host.IsUnknown() {
		return
	}
	if host.IsNull() {
		if !localAllowed {
			diags.AddAttributeError(p.AtName("host"), "host is required", "Give the host to reach over SSH.")
			return
		}
		for name, v := range map[string]attr.Value{"user": user, "known_hosts_line": line, "private_key_file": key, "port": port} {
			if !v.IsNull() {
				diags.AddAttributeError(p.AtName(name), name+" without host",
					"Without host the install is on the machine running OpenTofu, which needs no SSH setting: give host, or remove "+name+".")
			}
		}
		return
	}
	if user.IsNull() {
		diags.AddAttributeError(p.AtName("user"), "user is required with host", "Give the SSH user.")
	}
	if line.IsNull() {
		diags.AddAttributeError(p.AtName("known_hosts_line"), "known_hosts_line is required with host",
			"Give the host's known_hosts line (ssh-keyscan, checked against the host's own fingerprint, or the line stowage_vm_seed gives): no key learnt on first contact is accepted.")
	} else if known(line) {
		if _, err := target.ParseKnownHostsLine(line.ValueString()); err != nil {
			diags.AddAttributeError(p.AtName("known_hosts_line"), "known_hosts_line does not parse", err.Error())
		}
	}
	if known(host) && known(user) {
		if err := target.ValidateHostUser(host.ValueString(), user.ValueString()); err != nil {
			diags.AddAttributeError(p.AtName("host"), "host or user ssh would misread", err.Error())
		}
	}
}

func workDir(d *dockerModel) string {
	if d == nil || d.WorkDir.IsNull() || d.WorkDir.ValueString() == "" {
		return "."
	}
	return d.WorkDir.ValueString()
}

func targetKind(m *installModel) string {
	switch {
	case m.Docker != nil:
		return "docker"
	case m.Kubernetes != nil:
		return "kubernetes"
	case m.VM != nil:
		return "vm"
	}
	return ""
}

// firstInstallOnly is a setting a re-run cannot change: changing it would need a new install,
// and replacing an install deletes its data, so a change is a plan-time error, never a
// replacement.
func firstInstallOnly(diags *diag.Diagnostics, p path.Path, name, was, now string) {
	if was == now {
		return
	}
	diags.AddAttributeError(p, name+" is first-install only",
		fmt.Sprintf("%s was %q at the install and is now %q. A re-run cannot change it, and a new install would delete this one's data: set it back to %q, or move the install by hand and then `tofu state rm` this resource.", name, was, now, was))
}

func (r *installResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || req.State.Raw.IsNull() {
		return // a destroy, or a create
	}
	var plan, state installModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if a, b := targetKind(&state), targetKind(&plan); a != b {
		firstInstallOnly(&resp.Diagnostics, path.Root(b), "the target", a, b)
		return
	}
	if plan.Docker != nil && known(plan.Docker.WorkDir) {
		firstInstallOnly(&resp.Diagnostics, path.Root("docker").AtName("work_dir"), "docker.work_dir", workDir(state.Docker), workDir(plan.Docker))
	}
	if known(plan.Address) || plan.Address.IsNull() {
		firstInstallOnly(&resp.Diagnostics, path.Root("address"), "address", state.Address.ValueString(), plan.Address.ValueString())
	}
	if plan.VM != nil && state.VM != nil && !plan.VM.Channel.IsUnknown() {
		firstInstallOnly(&resp.Diagnostics, path.Root("vm").AtName("channel"), "vm.channel", state.VM.Channel.ValueString(), plan.VM.Channel.ValueString())
	}
	if known(plan.Installer) && known(state.Installer) {
		was, _, _ := parseInstaller(state.Installer.ValueString())
		now, _, _ := parseInstaller(plan.Installer.ValueString())
		firstInstallOnly(&resp.Diagnostics, path.Root("installer"), "the installer's project", was, now)
	}
	if r.data != nil && r.data.RegistryKnown && known(state.Registry) {
		firstInstallOnly(&resp.Diagnostics, path.Root("registry"), "the provider's registry", state.Registry.ValueString(), r.data.Registry)
	}
	if !plan.Installer.Equal(state.Installer) {
		for _, a := range []string{"version", "revision", "channel", "status", "previous"} {
			var unknown attr.Value
			switch a {
			case "revision":
				unknown = types.Int64Unknown()
			case "previous":
				unknown = types.ObjectUnknown(previousType)
			default:
				unknown = types.StringUnknown()
			}
			resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(a), unknown)...)
		}
	}
}

func (r *installResource) docker(m *installModel) (installer.Docker, error) {
	if r.data == nil {
		return installer.Docker{}, errors.New("the provider is not configured")
	}
	name, _, err := parseInstaller(m.Installer.ValueString())
	if err != nil {
		return installer.Docker{}, err
	}
	d := m.Docker
	run, err := r.data.NewRunner(Connection{
		Host:           d.Host.ValueString(),
		Port:           int(d.Port.ValueInt64()),
		User:           d.User.ValueString(),
		KeyFile:        d.PrivateKeyFile.ValueString(),
		KnownHostsLine: d.KnownHostsLine.ValueString(),
	})
	if err != nil {
		return installer.Docker{}, err
	}
	return installer.Docker{Runner: run, WorkDir: workDir(d), Name: name}, nil
}

func notYet(diags *diag.Diagnostics, what string) {
	diags.AddError(what+" is not implemented yet",
		what+" is not implemented in this release of terraform-provider-stowage (see its TODO.md). The Docker target's create, read and delete are.")
}

func addInstallerError(diags *diag.Diagnostics, summary string, err error) {
	var refused *installer.RefusedError
	var format *installer.FormatError
	switch {
	case errors.As(err, &refused):
		diags.AddError(summary+": refused", refused.Error())
	case errors.As(err, &format):
		diags.AddError(summary+": installer format", format.Error())
	default:
		diags.AddError(summary, err.Error())
	}
}

func (r *installResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m installModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	switch targetKind(&m) {
	case "kubernetes":
		notYet(&resp.Diagnostics, "The Kubernetes target")
		return
	case "vm":
		notYet(&resp.Diagnostics, "The VM target")
		return
	}
	timeout, diags := m.Timeouts.Create(ctx, createTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Write-only values are in the configuration alone.
	var secrets types.Map
	var given, exported types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secrets)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("given_wo"), &given)...)
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secrets_wo"), &exported)...)
	in := installer.Install{
		Inputs:       stringMap(ctx, m.Inputs, &resp.Diagnostics),
		SecretInputs: stringMap(ctx, secrets, &resp.Diagnostics),
		Given:        given.ValueString(),
		Secrets:      exported.ValueString(),
		TLS:          m.TLS.ValueString(),
		Domains:      stringMap(ctx, m.Domains, &resp.Diagnostics),
		Address:      m.Address.ValueString(),
		Registry:     r.registry(),
		RegistryCA:   r.registryCA(),
	}
	if known(m.Ports) {
		ports := map[string]int64{}
		resp.Diagnostics.Append(m.Ports.ElementsAs(ctx, &ports, false)...)
		in.Ports = map[string]string{}
		for k, p := range ports {
			in.Ports[k] = strconv.FormatInt(p, 10)
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	d, err := r.docker(&m)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	ref := m.Installer.ValueString()
	_, host, _ := parseInstaller(ref)
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
	if desc.Name != d.Name {
		resp.Diagnostics.AddAttributeError(path.Root("installer"), "installer of another project",
			fmt.Sprintf("%s describes the project %q, not %q its image is named after.", ref, desc.Name, d.Name))
		return
	}
	if len(desc.Shapes) > 0 && !slices.Contains(desc.Shapes, "docker") {
		resp.Diagnostics.AddError("No Docker shape", fmt.Sprintf("%s %s offers %s, not docker.", desc.Name, desc.Release.Version, strings.Join(desc.Shapes, ", ")))
		return
	}
	for _, input := range desc.Inputs {
		if _, ok := in.Inputs[input.Key]; ok && input.Secret {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(input.Key), "secret input in inputs",
				fmt.Sprintf("%s is a secret input: inputs land in state, so give it in secret_inputs_wo.", input.Key))
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if _, err := d.Install(ctx, in); err != nil {
		addInstallerError(&resp.Diagnostics, "Installing "+desc.Name, err)
		return
	}
	st, err := d.Status(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the install", err)
		return
	}
	if st == nil {
		resp.Diagnostics.AddError("Reading the install", "the install succeeded but its directory is not there: "+d.WorkDir+"/"+d.Name)
		return
	}
	m.ID = types.StringValue(d.WorkDir + "/" + d.Name)
	m.Registry = types.StringValue(r.registry())
	applyStatus(&m, st, false)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *installResource) registry() string {
	if r.data == nil {
		return ""
	}
	return r.data.Registry
}

func (r *installResource) registryCA() string {
	if r.data == nil {
		return ""
	}
	return r.data.RegistryCA
}

// applyStatus puts what status --json says into the model. On a read the installer that runs
// replaces the desired one, so the next plan shows the change back; a create keeps the one
// planned, as its result must match the plan.
func applyStatus(m *installModel, st *installer.Status, read bool) {
	switch {
	case st.InstallFailed != "":
		m.Status = types.StringValue("failed: " + st.InstallFailed)
	case st.Installing:
		m.Status = types.StringValue("installing")
	case st.Installed != nil:
		m.Status = types.StringValue("installed")
	default:
		m.Status = types.StringValue("not installed")
	}
	m.Channel = types.StringValue(st.Channel)
	if st.Installed != nil {
		m.Version = types.StringValue(st.Installed.Version)
		m.Revision = types.Int64Value(st.Installed.Revision)
		if read && st.Installed.Installer != "" {
			m.Installer = types.StringValue(st.Installed.Installer)
		}
	} else {
		m.Version = types.StringNull()
		m.Revision = types.Int64Null()
	}
	if st.Previous == nil {
		m.Previous = types.ObjectNull(previousType)
	} else {
		installerValue := types.StringNull()
		if st.Previous.Installer != "" {
			installerValue = types.StringValue(st.Previous.Installer)
		}
		m.Previous = types.ObjectValueMust(previousType, map[string]attr.Value{
			"version":   types.StringValue(st.Previous.Version),
			"revision":  types.Int64Value(st.Previous.Revision),
			"installer": installerValue,
		})
	}
}

func (r *installResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m installModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() || m.Docker == nil {
		return // only the Docker target is created, so only it is read
	}
	timeout, diags := m.Timeouts.Read(ctx, readTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d, err := r.docker(&m)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	st, err := d.Status(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the install", err)
		return
	}
	if st == nil {
		resp.State.RemoveResource(ctx)
		return
	}
	applyStatus(&m, st, true)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// remoteChange names what in plan the target would have to apply, against state.
func remoteChange(plan, state *installModel) []string {
	var changed []string
	for name, eq := range map[string]bool{
		"installer":                plan.Installer.Equal(state.Installer),
		"inputs":                   plan.Inputs.Equal(state.Inputs),
		"secret_inputs_wo_version": plan.SecretInputsWOVersion.Equal(state.SecretInputsWOVersion),
		"tls":                      plan.TLS.Equal(state.TLS),
		"domains":                  plan.Domains.Equal(state.Domains),
		"ports":                    plan.Ports.Equal(state.Ports),
		"vm.version":               plan.VM == nil || state.VM == nil || plan.VM.Version.Equal(state.VM.Version),
	} {
		if !eq {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed
}

func (r *installResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state installModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if changed := remoteChange(&plan, &state); len(changed) > 0 {
		notYet(&resp.Diagnostics, "Changing "+strings.Join(changed, ", ")+" of an install")
		return
	}
	// Only the provider's own settings changed (the connection, deletion_protection,
	// delete_data, allow_rollback, timeouts): the target has nothing to do.
	plan.ID, plan.Registry, plan.Version, plan.Revision = state.ID, state.Registry, state.Version, state.Revision
	plan.Channel, plan.Status, plan.Previous = state.Channel, state.Status, state.Previous
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *installResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m installModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.DeletionProtection.ValueBool() {
		resp.Diagnostics.AddError("deletion_protection is on",
			"This install is protected: set deletion_protection = false and apply, then destroy. delete_data = true also deletes its volumes.")
		return
	}
	switch targetKind(&m) {
	case "vm":
		return // forgotten: removing the VM is its hypervisor resource's job
	case "kubernetes":
		notYet(&resp.Diagnostics, "The Kubernetes target")
		return
	}
	timeout, diags := m.Timeouts.Delete(ctx, deleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d, err := r.docker(&m)
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	if _, err := d.Uninstall(ctx, m.DeleteData.ValueBool()); err != nil {
		addInstallerError(&resp.Diagnostics, "Uninstalling", err)
	}
}
