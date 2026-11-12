package provider

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	pathpkg "path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

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
	_ resource.ResourceWithImportState      = (*installResource)(nil)
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
		checkWorkDir(m.Docker.WorkDir, &resp.Diagnostics)
	}
	if m.VM != nil {
		validateVM(&m, &resp.Diagnostics)
	}
	if m.Kubernetes != nil {
		validateK8s(&m, &resp.Diagnostics)
	}

	// The configuration's maps are read by key: a value may not be known yet (a write-only one
	// from an ephemeral variable, one computed from another resource); a null one is refused.
	for _, c := range []struct {
		name string
		m    types.Map
	}{{"inputs", m.Inputs}, {"secret_inputs_wo", m.SecretInputsWO}, {"domains", m.Domains}, {"ports", m.Ports}} {
		for _, k := range mapKeys(c.m) {
			if c.m.Elements()[k].IsNull() {
				p := path.Root(c.name).AtMapKey(k)
				if c.name == "secret_inputs_wo" {
					p = path.Root(c.name) // a write-only map's keys are not shown
				}
				resp.Diagnostics.AddAttributeError(p, c.name+"."+k+" is null", fmt.Sprintf("Give %s.%s a value, or take it out.", c.name, k))
			}
		}
	}
	inputs := mapKeys(m.Inputs)
	for _, k := range inputs {
		if !installer.ValidInputKey(k) {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(k), "not an input key",
				fmt.Sprintf("%q: an input key is a letter or _, then letters, digits or _ (and not tls).", k))
		}
	}
	for _, k := range mapKeys(m.SecretInputsWO) {
		if !installer.ValidInputKey(k) {
			resp.Diagnostics.AddAttributeError(path.Root("secret_inputs_wo"), "not an input key",
				fmt.Sprintf("%q: an input key is a letter or _, then letters, digits or _ (and not tls).", k))
		}
		if slices.Contains(inputs, k) {
			resp.Diagnostics.AddAttributeError(path.Root("inputs").AtMapKey(k), "input given twice",
				fmt.Sprintf("%s is in inputs and in secret_inputs_wo: keep it in one.", k))
		}
	}
	if known(m.SecretInputsWO) && m.SecretInputsWOVersion.IsNull() {
		resp.Diagnostics.AddAttributeWarning(path.Root("secret_inputs_wo_version"), "secret_inputs_wo_version is not set",
			"A write-only value is never compared: without a version to bump, a later change of secret_inputs_wo is never applied.")
	}
	for _, k := range mapKeys(m.Domains) {
		if !installer.ValidRoute(k) {
			resp.Diagnostics.AddAttributeError(path.Root("domains").AtMapKey(k), "not a route name", fmt.Sprintf("%q is not a route name.", k))
		}
	}
	for _, k := range mapKeys(m.Ports) {
		if !installer.ValidRoute(k) {
			resp.Diagnostics.AddAttributeError(path.Root("ports").AtMapKey(k), "not a route name", fmt.Sprintf("%q is not a route name.", k))
		}
		if p, ok := m.Ports.Elements()[k].(types.Int64); ok && known(p) && (p.ValueInt64() < 1 || p.ValueInt64() > 65535) {
			resp.Diagnostics.AddAttributeError(path.Root("ports").AtMapKey(k), "port out of range", fmt.Sprintf("%d is not a port (1-65535).", p.ValueInt64()))
		}
	}
}

// mapKeys is a known map's keys, sorted, whether or not each value is known; nil for a null or
// unknown map.
func mapKeys(m types.Map) []string {
	if !known(m) {
		return nil
	}
	return slices.Sorted(maps.Keys(m.Elements()))
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

// checkWorkDir refuses a docker.work_dir that is not an absolute, clean path.
func checkWorkDir(v types.String, diags *diag.Diagnostics) {
	if w := v.ValueString(); known(v) && (!strings.HasPrefix(w, "/") || pathpkg.Clean(w) != w) {
		diags.AddAttributeError(path.Root("docker").AtName("work_dir"), "work_dir is not an absolute, clean path",
			fmt.Sprintf("%q: give an absolute path with no . or .. element, no // and no trailing /, or leave work_dir out for the SSH user's home (OpenTofu's working directory, locally).", w))
	}
}

// portValidators are an SSH port's, the resource's and the data source's.
var portValidators = []validator.Int64{int64validator.Between(1, 65535)}

// heldWorkDir is d's work directory as the install's directory resolved it: left out while
// state's was left out too, the directory state's ID names (the home the create resolved), so
// leaving it out and naming that directory are the same; else as given (an absolute, clean path,
// or "." for a home not resolved).
func heldWorkDir(state *installModel, d *dockerModel) string {
	if workDir(state.Docker) != "." {
		return workDir(d)
	}
	return homeWorkDir(state.ID, d)
}

// homeWorkDir is d's work directory, or, left out, the home an absolute id names (the one the
// create resolved).
func homeWorkDir(id types.String, d *dockerModel) string {
	w := workDir(d)
	if v := id.ValueString(); w == "." && known(id) && strings.HasPrefix(v, "/") {
		return pathpkg.Dir(v)
	}
	return w
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
	firstInstallOnlyFix(diags, p, name, was, now, fmt.Sprintf("A re-run cannot change it, and a new install would delete this one's data: set it back to %q, or move the install by hand and then `tofu state rm` this resource.", was))
}

// firstInstallOnlyFix is firstInstallOnly's refusal with its own fix.
func firstInstallOnlyFix(diags *diag.Diagnostics, p path.Path, name, was, now, fix string) {
	if was == now {
		return
	}
	diags.AddAttributeError(p, name+" is first-install only", fmt.Sprintf("%s was %q at the install and is now %q. %s", name, was, now, fix))
}

func (r *installResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return // a destroy
	}
	if req.State.Raw.IsNull() {
		var plan installModel
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		if !resp.Diagnostics.HasError() {
			r.planPullLogin(&plan, nil, &resp.Diagnostics)
		}
		return // a create
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
	// Left out is the home directory (workDir): taking work_dir out is a change too.
	if plan.Docker != nil && state.Docker != nil && !plan.Docker.WorkDir.IsUnknown() {
		firstInstallOnly(&resp.Diagnostics, path.Root("docker").AtName("work_dir"), "docker.work_dir", heldWorkDir(&state, state.Docker), heldWorkDir(&state, plan.Docker))
	}
	// An imported state does not know the first install's address: the installer refuses a
	// re-run given one, and none is given after the first install.
	if !imported(&state) && (known(plan.Address) || plan.Address.IsNull()) {
		firstInstallOnly(&resp.Diagnostics, path.Root("address"), "address", state.Address.ValueString(), plan.Address.ValueString())
	}
	// The VM's installer is its seed's, or the import ID's: no other is run.
	if plan.VM != nil && state.VM != nil && known(plan.VM.ShortName) {
		shortNameFixed(&resp.Diagnostics, state.VM.ShortName.ValueString(), plan.VM.ShortName.ValueString())
	}
	// An imported state does not know the VM's channel: the first apply records the configuration's.
	if plan.VM != nil && state.VM != nil && !plan.VM.Channel.IsUnknown() && !imported(&state) {
		firstInstallOnly(&resp.Diagnostics, path.Root("vm").AtName("channel"), "vm.channel", state.VM.Channel.ValueString(), plan.VM.Channel.ValueString())
	}
	if known(plan.Installer) && (known(state.Installer) || imported(&state)) {
		// An imported release's project is told at the first apply, from its status.
		if was := installName(&state); was != "" {
			now, _, _ := parseInstaller(plan.Installer.ValueString())
			firstInstallOnly(&resp.Diagnostics, path.Root("installer"), "the installer's project", was, now)
		}
	}
	r.k8sModifyPlan(ctx, &plan, &state, resp)
	r.planPullLogin(&plan, &state, &resp.Diagnostics)
	planMovedSecrets(ctx, req, &plan, &state, &resp.Diagnostics)
	r.planRollback(ctx, &plan, &state, &resp.Diagnostics)
	planVM(ctx, &plan, &state, resp)
	planRemovals(ctx, &plan, &state, &resp.Diagnostics)
	if r.data != nil && r.data.RegistryCAKnown && !imported(&state) && plan.Kubernetes == nil {
		firstInstallOnlyFix(&resp.Diagnostics, path.Root("registry_ca_digest"), "the provider's registry_ca", state.RegistryCADigest.ValueString(), caDigest(r.data.RegistryCA),
			"The install keeps the registry CA its first install was given (status --json registry_ca), which a re-run cannot change: give the provider that CA again (an empty one for none).")
	}
	// The provider's registry as written, against the one written at the install (state's): a
	// plan cannot read the release's own registry, so naming it over an install made without one
	// is a change too (leave it out). The apply compares the meaning with the install's
	// (heldMismatch).
	if r.data != nil && r.data.RegistryKnown && known(state.Registry) {
		firstInstallOnly(&resp.Diagnostics, path.Root("registry"), "the provider's registry", state.Registry.ValueString(), r.data.Registry)
	}
	// Last: a plan another check refuses never reaches the target.
	r.planDryRun(ctx, req, &plan, &state, &resp.Diagnostics)
	unknowns := []string{}
	switch {
	case !plan.Installer.Equal(state.Installer) || imported(&state):
		unknowns = []string{"version", "revision", "channel", "status", "previous", "short_name"}
		if imported(&state) {
			unknowns = append(unknowns, "registry") // the first apply records the provider's
		}
	case len(remoteChange(&plan, &state)) > 0:
		// The update reads the release after its run, and the target may run another than state
		// says (changed outside OpenTofu, a refresh skipped); a re-run may finish a first
		// install that failed.
		unknowns = []string{"version", "revision", "channel", "status", "previous"}
	}
	if imported(&state) {
		unknowns = append(unknowns, "registry") // the first apply records the provider's
	}
	if len(unknowns) > 0 {
		for _, a := range unknowns {
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

// imported says whether state came from an import and was never applied: the import records the
// target and the install's directory alone, and an apply records the rest (registry first).
func imported(m *installModel) bool { return m.Registry.IsNull() }

// installName is the project's name: the installer's, else (an imported state) the install
// directory's.
func installName(m *installModel) string {
	if known(m.Installer) {
		name, _, _ := parseInstaller(m.Installer.ValueString())
		return name
	}
	if m.Kubernetes != nil {
		return "" // <namespace>/<release> names no project
	}
	return baseName(m.ID.ValueString())
}

func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// previousInstaller is the installer of the release state's previous names; "" when none.
func previousInstaller(ctx context.Context, m *installModel) string {
	if !known(m.Previous) {
		return ""
	}
	var prev struct {
		Version   types.String `tfsdk:"version"`
		Revision  types.Int64  `tfsdk:"revision"`
		Installer types.String `tfsdk:"installer"`
	}
	if diags := m.Previous.As(ctx, &prev, basetypes.ObjectAsOptions{}); diags.HasError() {
		return ""
	}
	return prev.Installer.ValueString()
}

// planRollback refuses, at plan time, a change of the release back to the previous one without
// allow_rollback: a rollback takes the previous release back, so a variable edit never makes one.
func (r *installResource) planRollback(ctx context.Context, plan, state *installModel, diags *diag.Diagnostics) {
	if !known(plan.Installer) || plan.Installer.Equal(state.Installer) {
		return
	}
	if prev := previousInstaller(ctx, state); prev != "" && prev == plan.Installer.ValueString() {
		if plan.AllowRollback.ValueBool() {
			if changed := slices.DeleteFunc(remoteChange(plan, state), func(a string) bool { return a == "installer" }); len(changed) > 0 {
				rollbackAlone(diags, strings.Join(changed, ", "))
			}
			return
		}
		diags.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
			fmt.Sprintf("installer is now %s, the release the install's last upgrade replaced (previous.installer): reaching it is the kept installer's rollback, which takes the previous release back (every input keeps its value). Set allow_rollback = true to roll back, or set installer back to %s.", prev, state.Installer.ValueString()))
	}
}

// rollbackAlone is the refusal of a rollback with other changes of the target.
func rollbackAlone(diags *diag.Diagnostics, changed string) {
	diags.AddAttributeError(path.Root("allow_rollback"), "a rollback goes alone",
		fmt.Sprintf("installer goes back to previous.installer, a rollback, which restores the previous release: %s changed with it are not applied with it. Apply the rollback alone, then the change.", changed))
}

// planRemovals refuses what a re-run cannot be told: a TLS mode given up for loopback, a route's
// domain or port taken away. The installer has no flag that says it.
func planRemovals(ctx context.Context, plan, state *installModel, diags *diag.Diagnostics) {
	// A route the new release adds takes its domain from its domain input, which the upgrade is
	// given (inputs); --domains is a re-run's, whose release does not know the route.
	if known(plan.Installer) && !plan.Installer.Equal(state.Installer) && !imported(state) && known(plan.Domains) && !state.Domains.IsUnknown() {
		was := map[string]attr.Value{}
		if !state.Domains.IsNull() {
			was = state.Domains.Elements()
		}
		for _, k := range slices.Sorted(maps.Keys(plan.Domains.Elements())) {
			if _, ok := was[k]; !ok {
				diags.AddAttributeError(path.Root("domains").AtMapKey(k), "a domain added with a release change",
					fmt.Sprintf("domains.%s is added while installer changes: a route the new release adds takes its domain from its domain input, given in inputs with the upgrade; a route the running release has takes it in an apply of its own. Add domains.%s in another apply.", k, k))
			}
		}
	}
	if known(state.TLS) && plan.TLS.IsNull() {
		diags.AddAttributeError(path.Root("tls"), "tls cannot be removed",
			fmt.Sprintf("The install runs in the TLS mode %q: a re-run has no flag for loopback mode (a move between loopback and a TLS mode is a new install). Set tls back to %q, or to another TLS mode.", state.TLS.ValueString(), state.TLS.ValueString()))
	}
	for _, c := range []struct {
		name        string
		plan, state types.Map
	}{{"domains", plan.Domains, state.Domains}, {"ports", plan.Ports, state.Ports}} {
		if !known(c.state) || c.plan.IsUnknown() {
			continue
		}
		now := map[string]attr.Value{}
		if !c.plan.IsNull() {
			now = c.plan.Elements()
		}
		for _, k := range slices.Sorted(maps.Keys(c.state.Elements())) {
			if _, ok := now[k]; !ok {
				diags.AddAttributeError(path.Root(c.name), c.name+"."+k+" cannot be removed",
					fmt.Sprintf("The install records the route %s's %s: a re-run changes one, it never takes one away. Put %s.%s back.", k, strings.TrimSuffix(c.name, "s"), c.name, k))
			}
		}
	}
}

func (r *installResource) docker(m *installModel, short string) (installer.Docker, error) {
	if r.data == nil {
		return installer.Docker{}, errors.New("the provider is not configured")
	}
	name := installName(m)
	if name == "" {
		return installer.Docker{}, errors.New("the install's project is not known: give installer")
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
	// Left out, the home the create resolved: every ID built from it is state's.
	return installer.Docker{Runner: run, WorkDir: homeWorkDir(m.ID, d), Name: name, Short: short, Mirror: r.data.mirrorLogin()}, nil
}

func addInstallerError(diags *diag.Diagnostics, summary string, err error) {
	var refused *installer.RefusedError
	var format *installer.FormatError
	switch {
	case errors.As(err, &refused):
		// The fix is advice for the operator: shown, never run.
		fix := refused.Fix
		if fix == "" {
			fix = "The installer names no fix."
		}
		diags.AddError(refused.Reason, fmt.Sprintf("%s\n\n%s was refused by the installer (%s, exit 3) before anything changed on the target.", fix, summary, refused.Verb))
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
	if targetKind(&m) == "vm" {
		r.createVM(ctx, req, resp)
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
	in.Ports = portMap(ctx, m.Ports, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if m.Kubernetes != nil {
		r.k8sCreate(ctx, &m, in, resp)
		return
	}

	d, err := r.docker(&m, "")
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	desc := r.fetchDescribe(ctx, d, m.Installer.ValueString(), &resp.Diagnostics)
	if desc == nil {
		return
	}
	refuseSecretInputs(&resp.Diagnostics, in.Inputs, desc)
	if d.Local() {
		refuseRunEnvInputs(&resp.Diagnostics, nil, in.Given, in.Inputs, in.SecretInputs)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	d = d.Without(desc)
	d.Short = desc.ShortName
	id := d.WorkDir + "/" + d.Name
	if d.WorkDir == "." {
		// Left out: the home, resolved once before anything runs, which a later plan compares.
		home, err := d.Resolve(ctx)
		if err != nil {
			addInstallerError(&resp.Diagnostics, "Reading the work directory", err)
			return
		}
		id = pathpkg.Join(home, d.Name)
	}
	// An install already in the directory (a create that failed or timed out after the install
	// began, or one a destroy kept with its data) is run again, as the installer's re-run, which
	// refuses the settings only a first install takes: they are the ones it recorded.
	existing, err := d.FetchedStatus(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the install directory", err)
		return
	}
	if existing != nil {
		if existing.Name != d.Name {
			resp.Diagnostics.AddError("Another project's install",
				fmt.Sprintf("%s/%s holds an install of %q, not %q: give another docker.work_dir.", d.WorkDir, d.Name, existing.Name, d.Name))
			return
		}
		if bad := heldMismatch(existing, in.Address, in.Registry, desc.Registry, in.RegistryCA); len(bad) > 0 {
			resp.Diagnostics.AddError("The install there was made otherwise",
				fmt.Sprintf("%s/%s already holds an install of %s whose first install recorded other settings, which a re-run cannot change: %s. Give the settings the install holds (status --json), or another docker.work_dir.", d.WorkDir, d.Name, d.Name, strings.Join(bad, "; ")))
			return
		}
		resp.Diagnostics.AddWarning("An install is already there",
			fmt.Sprintf("%s/%s already holds an install of %s: the create runs the installer again on it (its re-run), which finishes a first install left unfinished; "+
				"an install of another release there is refused by the installer: set installer to the one it runs (status --json installed.installer), apply, then change installer to upgrade it. "+
				"The settings only a first install takes (describe's first_install_only: address, the provider's registry and registry_ca; and secrets_wo) are not given: the install keeps the ones it recorded, which the configuration's match.", d.WorkDir, d.Name, d.Name))
		withoutFirstInstallOnly(&in, desc.FirstInstallOnly)
	}
	if _, err := d.Install(ctx, in); err != nil {
		addInstallerError(&resp.Diagnostics, "Installing "+desc.Name, err)
		return
	}
	st, err := r.settle(ctx, d)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the install", err)
		return
	}
	if st.InstallFailed != "" {
		resp.Diagnostics.AddError("Installing "+desc.Name+" failed",
			fmt.Sprintf("status --json reports the first install failed: %s. Apply again: the install runs again on %s/%s and finishes it.", st.InstallFailed, d.WorkDir, d.Name))
		return
	}
	m.ID = types.StringValue(id)
	m.Registry = types.StringValue(r.registry())
	m.ShortName = types.StringValue(desc.ShortName)
	applyStatus(&m, st, false)
	applyHeld(&m, st, false)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// providerCA is the provider's registry CA's digest as state records it: null for none.
func (r *installResource) providerCA() types.String {
	if d := caDigest(r.registryCA()); d != "" {
		return types.StringValue(d)
	}
	return types.StringNull()
}

// planMovedSecrets refuses at plan an input taken out of inputs and given in secret_inputs_wo
// (a write-only value the configuration holds at plan) with another value, without a new
// secret_inputs_wo_version: its value would never be sent. The one check of it: OpenTofu plans
// again with the final configuration before an apply, write-only values known then.
func planMovedSecrets(ctx context.Context, req resource.ModifyPlanRequest, plan, state *installModel, diags *diag.Diagnostics) {
	if !plan.SecretInputsWOVersion.Equal(state.SecretInputsWOVersion) || !known(plan.Inputs) && !plan.Inputs.IsNull() {
		return
	}
	var secretsWO types.Map
	diags.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	if !known(secretsWO) {
		return
	}
	was, now := stringMap(ctx, state.Inputs, diags), stringMap(ctx, plan.Inputs, diags)
	for _, k := range mapKeys(secretsWO) {
		v := secretsWO.Elements()[k]
		sv, ok := v.(types.String)
		if !ok || !known(sv) {
			continue
		}
		old, had := was[k]
		if _, still := now[k]; had && !still && sv.ValueString() != old {
			diags.AddAttributeError(path.Root("secret_inputs_wo_version"), "an input moved to secret_inputs_wo",
				fmt.Sprintf("%s is taken out of inputs and given in secret_inputs_wo: change secret_inputs_wo_version in the same apply, so its value is sent (else the install keeps the one inputs gave).", k))
		}
	}
}

// caDigest is `sha256:<hex>` of a registry CA as the installer is given it (the file's bytes),
// as status --json reports the one an install keeps; "" for none.
func caDigest(pem string) string {
	if pem == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(pem))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// heldMismatch names each first-install-only setting the configuration means that the install st
// recorded otherwise: a re-run is never given them, so they would be silently not applied. Left
// out, each means its default: the address 127.0.0.1, the release's own registry (defaultRegistry,
// describe's), no registry CA.
func heldMismatch(st *installer.Status, address, registry, defaultRegistry, registryCA string) []string {
	var out []string
	if want := cmp.Or(address, defaultAddress); st.Address != "" && st.Address != want {
		out = append(out, fmt.Sprintf("address is %q, and the install holds %q", want, st.Address))
	}
	if want := cmp.Or(registry, defaultRegistry); want != "" && st.Registry != "" && st.Registry != want {
		out = append(out, fmt.Sprintf("the provider's registry is %q, and the install pulls from %q", want, st.Registry))
	}
	if d := caDigest(registryCA); d != st.RegistryCA {
		out = append(out, fmt.Sprintf("the provider's registry_ca is %s, and the install keeps %s", cmp.Or(d, "none"), cmp.Or(st.RegistryCA, "none")))
	}
	return out
}

// applyHeld puts into m what status says the install recorded at its first install, for a later
// plan to compare: the registry CA's digest (on a VM, whose status leaves it out when it cannot
// read it, only when given) and the kept installer's short name; on a read (read), the loopback
// address and the registry where state's differ from the install's, so the plan shows the
// change, a first-install-only error. An apply never changes those two: the plan holds them.
func applyHeld(m *installModel, st *installer.Status, read bool) {
	switch {
	case st.RegistryCA != "":
		m.RegistryCADigest = types.StringValue(st.RegistryCA)
	case m.VM == nil:
		m.RegistryCADigest = types.StringNull()
	}
	if st.ShortName != "" && m.VM == nil {
		m.ShortName = types.StringValue(st.ShortName)
	}
	if !read {
		return
	}
	switch {
	case st.Address == "":
	case known(m.Address) && m.Address.ValueString() != st.Address:
		m.Address = types.StringValue(st.Address)
	case m.Address.IsNull() && st.Address != defaultAddress:
		m.Address = types.StringValue(st.Address)
	}
	if known(m.Registry) && m.Registry.ValueString() != "" && st.Registry != "" && m.Registry.ValueString() != st.Registry {
		m.Registry = types.StringValue(st.Registry)
	}
}

// defaultAddress is the loopback address an install binds when --address gives none.
const defaultAddress = "127.0.0.1"

// withoutFirstInstallOnly leaves out of in the settings a re-run refuses: those describe names
// first-install only that the provider gives, and the secrets export (a re-run keeps the
// install's secrets).
func withoutFirstInstallOnly(in *installer.Install, names []string) {
	in.Secrets = ""
	for _, n := range names {
		switch n {
		case "address":
			in.Address = ""
		case "registry":
			in.Registry = ""
		case "registry_ca":
			in.RegistryCA = ""
		}
	}
}

// fetchDescribe writes ref's bootstrap script on the target and reads its describe: it must be
// of d's project and offer the Docker shape. Nil, with the error in diags, otherwise.
func (r *installResource) fetchDescribe(ctx context.Context, d installer.Docker, ref string, diags *diag.Diagnostics) *installer.Describe {
	_, host, _ := parseInstaller(ref)
	if err := d.Fetch(ctx, ref, r.data.fetchLogin(host)); err != nil {
		addInstallerError(diags, "Fetching the installer", err)
		return nil
	}
	desc, err := d.Describe(ctx)
	if err != nil {
		addInstallerError(diags, "Reading the installer", err)
		return nil
	}
	if desc.Name != d.Name {
		diags.AddAttributeError(path.Root("installer"), "installer of another project",
			fmt.Sprintf("%s describes the project %q, not %q its image is named after.", ref, desc.Name, d.Name))
		return nil
	}
	if len(desc.Shapes) > 0 && !slices.Contains(desc.Shapes, "docker") {
		diags.AddError("No Docker shape", fmt.Sprintf("%s %s offers %s, not docker.", desc.Name, desc.Release.Version, strings.Join(desc.Shapes, ", ")))
		return nil
	}
	return desc
}

// settle reads the install until its first install is over (done, or failed): every Poll, within
// ctx's deadline (the create timeout).
func (r *installResource) settle(ctx context.Context, d installer.Docker) (*installer.Status, error) {
	for {
		st, err := d.Status(ctx)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// The read was cut by the create timeout: the install is still installing.
			return nil, fmt.Errorf("%s/%s is still installing when the create timeout ran out (timeouts.create; the read: %w): apply again once status --json no longer reports installing", d.WorkDir, d.Name, err)
		}
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("the install succeeded but its directory is not there: %s/%s", d.WorkDir, d.Name)
		}
		if !st.Installing || st.InstallFailed != "" {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s/%s is still installing when the create timeout ran out (timeouts.create): apply again once status --json no longer reports installing", d.WorkDir, d.Name)
		case <-time.After(r.data.Poll):
		}
	}
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
	if resp.Diagnostics.HasError() {
		return
	}
	if m.VM != nil {
		r.readVM(ctx, &m, resp)
		return
	}
	if m.Kubernetes != nil {
		timeout, diags := m.Timeouts.Read(ctx, readTimeout)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		r.k8sRead(ctx, &m, &resp.State, &resp.Diagnostics)
		return
	}
	if m.Docker == nil {
		return
	}
	if known(m.Docker.Host) && m.Docker.KnownHostsLine.IsNull() {
		return // an imported state: the configuration gives the connection, at the first apply
	}
	timeout, diags := m.Timeouts.Read(ctx, readTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d, err := r.docker(&m, m.ShortName.ValueString())
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
	applyHeld(&m, st, true)
	resp.Diagnostics.Append(dropUnrecorded(ctx, &m, st)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// dropUnrecorded removes from m's inputs a key given a value that the install does not record,
// so the next plan shows it given again. status --json lists every input key the install records,
// whichever release declared it (Stowage >= 0.35.0), so a key it does not list is not recorded.
// Nothing is dropped while the recorded keys are unknown.
func dropUnrecorded(ctx context.Context, m *installModel, st *installer.Status) diag.Diagnostics {
	var diags diag.Diagnostics
	if st.Inputs == nil || !known(m.Inputs) {
		return nil
	}
	inputs := stringMap(ctx, m.Inputs, &diags)
	kept := maps.Clone(inputs)
	for k, v := range inputs {
		if v != "" && !slices.Contains(*st.Inputs, k) {
			delete(kept, k)
		}
	}
	if len(kept) != len(inputs) {
		var d diag.Diagnostics
		m.Inputs, d = types.MapValueFrom(ctx, types.StringType, kept)
		diags.Append(d...)
	}
	return diags
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
		// Turned on, the release is applied again to write the pull Secret; turned off, the
		// Secret stays (nothing removes it) and nothing runs.
		"kubernetes.image_pull_login": plan.Kubernetes == nil || state.Kubernetes == nil ||
			!plan.Kubernetes.ImagePullLogin.ValueBool() || state.Kubernetes.ImagePullLogin.ValueBool(),
	} {
		if !eq {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed
}

// updateTimeout is the update's, when the timeouts block leaves it out: it may upgrade.
const updateTimeout = 30 * time.Minute

func (r *installResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state installModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(remoteChange(&plan, &state)) == 0 && !imported(&state) {
		// Only the provider's own settings changed (the connection, deletion_protection,
		// delete_data, allow_rollback, timeouts): the target has nothing to do.
		plan.ID, plan.Registry, plan.Version, plan.Revision = state.ID, state.Registry, state.Version, state.Revision
		plan.Channel, plan.Status, plan.Previous, plan.ShortName = state.Channel, state.Status, state.Previous, state.ShortName
		plan.RegistryCADigest = state.RegistryCADigest
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}
	if targetKind(&plan) == "vm" {
		r.updateVM(ctx, req, resp, &plan, &state)
		return
	}
	timeout, diags := plan.Timeouts.Update(ctx, updateTimeout)
	resp.Diagnostics.Append(diags...)
	var secretsWO types.Map
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_inputs_wo"), &secretsWO)...)
	secrets := stringMap(ctx, secretsWO, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	save := func(m *installModel) bool {
		resp.Diagnostics.Append(resp.State.Set(ctx, m)...)
		return !resp.Diagnostics.HasError()
	}
	if plan.Kubernetes != nil {
		r.k8sUpdate(ctx, &plan, &state, secrets, &resp.Diagnostics, save)
		return
	}
	u := &update{r: r, plan: &plan, state: &state, secrets: secrets, diags: &resp.Diagnostics}
	u.run(ctx, save)
}

// update applies a plan to a Docker install in DESIGN § 13's order: the inputs first, by a re-run
// of the running release's installer, state saved; then the release, by the new release's
// installer (`upgrade`) or the kept one's `rollback`. A failure between leaves the reconfigured
// release in state, so the next plan shows the release change alone.
type update struct {
	r           *installResource
	plan, state *installModel
	secrets     map[string]string // secret_inputs_wo, from the configuration
	diags       *diag.Diagnostics
}

// releaseStep is the release change an update ends with.
type releaseStep int

const (
	stay     releaseStep = iota // the install runs the desired release
	upgrade                     // the new release's installer's upgrade
	rollback                    // the kept installer's rollback
)

func (u *update) run(ctx context.Context, save func(*installModel) bool) {
	plan, state := u.plan, u.state
	d, err := u.r.docker(plan, state.ShortName.ValueString())
	if err != nil {
		u.diags.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	st, err := d.Status(ctx)
	if err != nil {
		addInstallerError(u.diags, "Reading the install", err)
		return
	}
	if st == nil {
		u.diags.AddError("No install to change", fmt.Sprintf("%s/%s holds no install: an import names an install directory that exists; otherwise `tofu state rm` this resource and apply to install again.", d.WorkDir, d.Name))
		return
	}
	if st.ShortName != "" && st.ShortName != d.Short {
		// The kept installer is <short>-stow by the short name the install records (a rollback
		// across a rename keeps the older release's).
		if d, err = u.r.docker(plan, st.ShortName); err != nil {
			u.diags.AddError("Cannot reach the Docker target", err.Error())
			return
		}
	}
	running, err := d.KeptDescribe(ctx)
	if err != nil {
		addInstallerError(u.diags, "Reading the installer the install keeps", err)
		return
	}
	if !u.keptChecks(d, st, running) {
		return
	}
	step, next := u.releaseStep(ctx, d, st, nil)
	if u.diags.HasError() {
		return
	}
	rerun, up := u.split(ctx, running, next)
	refuseSecretInputs(u.diags, stringMap(ctx, plan.Inputs, u.diags), running, next)
	if d.Local() {
		refuseRunEnvInputs(u.diags, nil, "", stringMap(ctx, plan.Inputs, u.diags), u.secrets)
	}
	if u.diags.HasError() {
		return
	}
	d = d.Without(running, next)
	switch {
	case step == rollback && imported(state):
		runs := "the installer of the release it runs (status --json installed.installer)"
		if st.Installed != nil && st.Installed.Installer != "" {
			runs = st.Installed.Installer + ", the release it runs"
		}
		u.diags.AddAttributeError(path.Root("installer"), "a rollback of an imported install",
			fmt.Sprintf("An imported install is taken over first: apply with installer = %s, then set installer back to previous.installer.", runs))
		return
	case step == rollback && !rerun.Empty():
		rollbackAlone(u.diags, "inputs, secret inputs, tls, domains or ports")
		return
	}
	short := running.ShortName
	if !rerun.Empty() {
		if _, err := d.Rerun(ctx, rerun); err != nil {
			addInstallerError(u.diags, "Reconfiguring "+d.Name, err)
			return
		}
		if step != stay {
			partial := u.partial(ctx, rerun)
			if u.diags.HasError() {
				return
			}
			// The status read before the re-run: the release is the same, and the final read follows.
			fillUpdate(partial, d, short)
			applyStatus(partial, st, false)
			applyHeld(partial, st, false)
			if !save(partial) || u.diags.HasError() {
				return
			}
		}
	}
	switch step {
	case upgrade:
		if _, err := d.Upgrade(ctx, up); err != nil {
			addInstallerError(u.diags, "Upgrading "+d.Name+" to "+next.Release.Version, err)
			return
		}
		short = next.ShortName
		d.Short = short
	case rollback:
		if _, err := d.Rollback(ctx); err != nil {
			addInstallerError(u.diags, "Rolling "+d.Name+" back", err)
			return
		}
	}
	final := *plan
	final.ID = types.StringValue(d.WorkDir + "/" + d.Name)
	final.Registry = state.Registry
	final.RegistryCADigest = state.RegistryCADigest
	if imported(state) {
		final.Registry = types.StringValue(u.r.registry())
	}
	u.settled(ctx, d, &final, short)
	save(&final)
}

// refuseSecretInputs refuses an input given a value that one of the releases declares secret:
// inputs land in state, and a changed one on the target's command line.
func refuseSecretInputs(diags *diag.Diagnostics, given map[string]string, releases ...*installer.Describe) {
	for _, k := range slices.Sorted(maps.Keys(given)) {
		for _, r := range releases {
			if r != nil && given[k] != "" && r.Secret(k) {
				diags.AddAttributeError(path.Root("inputs").AtMapKey(k), "secret input in inputs",
					fmt.Sprintf("%s is a secret input of %s %s: inputs land in state and on the target's command line, so give it in secret_inputs_wo.", k, r.Name, r.Release.Version))
				break
			}
		}
	}
}

// keptChecks refuses what an update cannot run over: no install any more, a kept installer of
// another project, first-install settings the install recorded otherwise (an imported state's
// first apply, a registry the plan could not tell from the release's own). Each is an error in
// diags; false then.
func (u *update) keptChecks(d installer.Docker, st *installer.Status, running *installer.Describe) bool {
	if running == nil {
		u.diags.AddError("No install to change", fmt.Sprintf("%s/%s is gone since it was read: apply again.", d.WorkDir, d.Name))
		return false
	}
	if running.Name != d.Name {
		u.diags.AddError("Installer of another project", fmt.Sprintf("The installer kept in %s/%s describes the project %q, not %q.", d.WorkDir, d.Name, running.Name, d.Name))
		return false
	}
	if bad := heldMismatch(st, u.plan.Address.ValueString(), u.r.registry(), running.Registry, u.r.registryCA()); len(bad) > 0 {
		u.diags.AddError("The install was made otherwise",
			fmt.Sprintf("The install %s/%s recorded other first-install settings, which a re-run cannot change: %s. Give the settings the install holds (status --json).", d.WorkDir, d.Name, strings.Join(bad, "; ")))
		return false
	}
	return true
}

// releaseStep decides the release change: none when the install runs the desired release (its
// installer, or the release that installer describes), rollback when the desired installer is the
// previous release's and allow_rollback is set, else the new installer's upgrade, fetched and
// described here (next). The installer refuses an older release. The new installer, when it must
// be read, is fetched onto d, or, with fetchTo, onto the Docker fetchTo gives (false: none, the
// step is stay).
func (u *update) releaseStep(ctx context.Context, d installer.Docker, st *installer.Status, fetchTo func() (installer.Docker, bool)) (releaseStep, *installer.Describe) {
	desired := u.plan.Installer.ValueString()
	switch {
	case st.Installed != nil && st.Installed.Installer == desired:
		return stay, nil
	case u.plan.Installer.Equal(u.state.Installer) && !imported(u.state):
		return stay, nil
	case st.Previous != nil && st.Previous.Installer == desired:
		if !u.plan.AllowRollback.ValueBool() {
			u.diags.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
				fmt.Sprintf("installer is %s, the release the install's last upgrade replaced: reaching it is the kept installer's rollback, which takes the previous release back (every input keeps its value). Set allow_rollback = true to roll back.", desired))
			return stay, nil
		}
		return rollback, nil
	}
	if fetchTo != nil {
		var ok bool
		if d, ok = fetchTo(); !ok {
			return stay, nil
		}
	}
	next := u.r.fetchDescribe(ctx, d, desired, u.diags)
	if next == nil {
		return stay, nil
	}
	if st.Installed != nil && next.Release.Same(*st.Installed) {
		// The same release: no upgrade, which would move it to a later revision. When the install
		// records another digest of its installer, Read would put that one back at every refresh.
		if rec := st.Installed.Installer; rec != "" {
			u.diags.AddAttributeError(path.Root("installer"), "installer is another digest of the release installed",
				fmt.Sprintf("%s carries %s %s (revision %d), which the install runs from %s: set installer to %s.", desired, next.Name, next.Release.Version, next.Release.Revision, rec, rec))
		}
		return stay, nil
	}
	return upgrade, next
}

// split sorts the plan's changes between the re-run of the running release (running) and the
// upgrade to next (nil without one). The re-run takes what the running release declares, and
// every change without an upgrade; the upgrade takes the inputs the running release does not
// declare (those the new release adds: the installer refuses any other) and the ports of routes
// state does not record. A removed input is given empty to the re-run when the running release
// declares it and the upgrade does not drop it. Secret inputs go to the re-run when
// secret_inputs_wo_version changed; those the running release does not declare go with every
// upgrade.
// split decides only: what it reports in u.diags is the maps' own conversion, which a known map
// of strings never fails.
func (u *update) split(ctx context.Context, running, next *installer.Describe) (rerun, up installer.Change) {
	plan, state := u.plan, u.state
	rerun = installer.Change{Inputs: map[string]string{}, SecretInputs: map[string]string{}, Domains: map[string]string{}, Ports: map[string]string{}}
	up = installer.Change{Inputs: map[string]string{}, SecretInputs: map[string]string{}, Ports: map[string]string{}}
	upgrading := next != nil
	was := stringMap(ctx, state.Inputs, u.diags)
	now := stringMap(ctx, plan.Inputs, u.diags)
	for k, v := range now {
		if old, ok := was[k]; ok && old == v {
			continue
		}
		if upgrading && !running.Declares(k) {
			up.Inputs[k] = v
		} else {
			rerun.Inputs[k] = v
		}
	}
	bump := !plan.SecretInputsWOVersion.Equal(state.SecretInputsWOVersion)
	for _, k := range slices.Sorted(maps.Keys(was)) {
		// An input moved to secret_inputs_wo is not emptied: --set would win over its answer,
		// which goes only with a new secret_inputs_wo_version: the plan refuses one without
		// (planMovedSecrets), and OpenTofu plans again with the final configuration before any
		// apply, so no apply sees one.
		if _, secret := u.secrets[k]; secret {
			continue
		}
		if _, ok := now[k]; !ok && running.Declares(k) && (!upgrading || next.Declares(k)) {
			rerun.Inputs[k] = ""
		}
	}
	for k, v := range u.secrets {
		switch {
		case running.Declares(k) && bump:
			rerun.SecretInputs[k] = v
		case running.Declares(k):
		case upgrading:
			up.SecretInputs[k] = v
		case bump:
			rerun.SecretInputs[k] = v
		}
	}
	if known(plan.TLS) && !plan.TLS.Equal(state.TLS) {
		rerun.TLS = plan.TLS.ValueString()
	}
	oldDomains := stringMap(ctx, state.Domains, u.diags)
	for k, v := range stringMap(ctx, plan.Domains, u.diags) {
		if oldDomains[k] != v {
			rerun.Domains[k] = v
		}
	}
	oldPorts, newPorts := portMap(ctx, state.Ports, u.diags), portMap(ctx, plan.Ports, u.diags)
	for k, v := range newPorts {
		old, ok := oldPorts[k]
		switch {
		case ok && old == v:
		case upgrading && !running.HasRoute(k) && next.HasRoute(k):
			up.Ports[k] = v // a route the new release adds: the upgrade binds it
		default:
			rerun.Ports[k] = v
		}
	}
	return rerun, up
}

// partial is the plan as the re-run applied it: the release still state's, and the inputs and
// ports the upgrade still has to give left as state records them.
func (u *update) partial(ctx context.Context, rerun installer.Change) *installModel {
	m := *u.plan
	m.Installer = u.state.Installer
	m.ID = u.state.ID
	m.Registry = u.state.Registry
	m.RegistryCADigest = u.state.RegistryCADigest
	if imported(u.state) {
		m.Registry = types.StringValue(u.r.registry())
	}
	inputs := stringMap(ctx, u.state.Inputs, u.diags)
	if inputs == nil {
		inputs = map[string]string{}
	}
	given := stringMap(ctx, u.plan.Inputs, u.diags)
	for k, v := range rerun.Inputs {
		if _, ok := given[k]; ok {
			inputs[k] = v
		} else {
			delete(inputs, k)
		}
	}
	m.Inputs = mapOrNull(ctx, u.state.Inputs, u.plan.Inputs, inputs, u.diags)
	ports := portMap(ctx, u.state.Ports, u.diags)
	if ports == nil {
		ports = map[string]string{}
	}
	maps.Copy(ports, rerun.Ports)
	nums := map[string]int64{}
	for k, v := range ports {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			u.diags.AddAttributeError(path.Root("ports").AtMapKey(k), "port", err.Error())
		}
		nums[k] = n
	}
	if len(nums) == 0 && u.plan.Ports.IsNull() {
		m.Ports = types.MapNull(types.Int64Type)
	} else {
		var diags diag.Diagnostics
		m.Ports, diags = types.MapValueFrom(ctx, types.Int64Type, nums)
		u.diags.Append(diags...)
	}
	return &m
}

// mapOrNull is vals as a map, null when it is empty and neither state nor plan holds a map.
func mapOrNull(ctx context.Context, was, now types.Map, vals map[string]string, diags *diag.Diagnostics) types.Map {
	if len(vals) == 0 && was.IsNull() && now.IsNull() {
		return types.MapNull(types.StringType)
	}
	m, d := types.MapValueFrom(ctx, types.StringType, vals)
	diags.Append(d...)
	return m
}

// settled fills m's computed values from the install's status: on a status that cannot be read,
// state's values, and the error.
func (u *update) settled(ctx context.Context, d installer.Docker, m *installModel, short string) {
	fillUpdate(m, d, short)
	st, err := d.Status(ctx)
	if err == nil && st == nil {
		err = fmt.Errorf("%s/%s holds no install any more", d.WorkDir, d.Name)
	}
	if err != nil {
		addInstallerError(u.diags, "Reading the install", err)
		keepComputed(m, u.state)
		return
	}
	applyStatus(m, st, false)
	applyHeld(m, st, false)
}

// keepComputed sets m's values read from the target to state's, when the read after an update
// failed: a null status is not installed, a null channel empty.
func keepComputed(m, state *installModel) {
	m.Version, m.Revision, m.Channel, m.Status, m.Previous = state.Version, state.Revision, state.Channel, state.Status, state.Previous
	m.RegistryCADigest = state.RegistryCADigest
	if m.RegistryCADigest.IsUnknown() {
		m.RegistryCADigest = types.StringNull()
	}
	if m.Status.IsNull() {
		m.Status = types.StringValue("not installed")
	}
	if m.Previous.IsNull() || m.Previous.IsUnknown() {
		m.Previous = types.ObjectNull(previousType)
	}
	if m.Channel.IsNull() {
		m.Channel = types.StringValue("")
	}
}

// fillUpdate sets what an update's state holds whatever the status: the short name, the ID, no
// write-only value.
func fillUpdate(m *installModel, d installer.Docker, short string) {
	m.ShortName = types.StringValue(short)
	m.SecretInputsWO, m.GivenWO, m.SecretsWO = types.MapNull(types.StringType), types.StringNull(), types.StringNull()
	if m.ID.IsNull() {
		m.ID = types.StringValue(d.WorkDir + "/" + d.Name)
	}
}

func portMap(ctx context.Context, m types.Map, diags *diag.Diagnostics) map[string]string {
	if !known(m) {
		return nil
	}
	nums := map[string]int64{}
	diags.Append(m.ElementsAs(ctx, &nums, false)...)
	out := map[string]string{}
	for k, n := range nums {
		// Checked here too: a port unknown when the configuration was validated is known now.
		if n < 1 || n > 65535 {
			diags.AddAttributeError(path.Root("ports").AtMapKey(k), "port out of range", fmt.Sprintf("%d is not a port (1-65535).", n))
		}
		out[k] = strconv.FormatInt(n, 10)
	}
	return out
}

// ImportState reads an import ID naming the installer an install keeps, `[<host>:]<work_dir>/
// <name>/<short>-stow`: the target and the install's directory. The configuration gives the
// connection and the rest at the first apply, which reconfigures the install with it.
func (r *installResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if id, ok := strings.CutPrefix(req.ID, "vm:"); ok {
		r.importVM(ctx, id, resp)
		return
	}
	if strings.HasPrefix(req.ID, "kubernetes:") {
		r.k8sImport(ctx, req.ID, resp)
		return
	}
	host, work, name, short, err := parseImportID(req.ID)
	if err != nil {
		resp.Diagnostics.AddError("Import ID", err.Error())
		return
	}
	hostValue, workValue := types.StringNull(), types.StringNull()
	if host != "" {
		hostValue = types.StringValue(host)
	}
	if work != "." {
		workValue = types.StringValue(work)
	}
	null := types.StringNull()
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("docker"), &dockerModel{
		Host: hostValue, Port: types.Int64Null(), User: null, PrivateKeyFile: null, KnownHostsLine: null, WorkDir: workValue})...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), work+"/"+name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("short_name"), short)...)
	for a, v := range map[string]bool{"deletion_protection": true, "delete_data": false, "allow_rollback": false} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(a), v)...)
	}
}

// parseImportID reads `[<host>:]<work_dir>/<name>/<short>-stow`; a host is what comes before the
// first colon when no slash does, and a work_dir left out is ".".
func parseImportID(id string) (host, work, name, short string, err error) {
	rest := id
	if i := strings.Index(id, ":"); i >= 0 && !strings.Contains(id[:i], "/") {
		host, rest = id[:i], id[i+1:]
		if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n@") {
			return "", "", "", "", fmt.Errorf("%q: the host before the colon is not a host name", id)
		}
	}
	bad := fmt.Errorf("%q: want [<host>:]<work_dir>/<name>/<short>-stow, the installer the install keeps (`ls <work_dir>/<name>/*-stow`)", id)
	dir, file := splitLast(rest)
	short, ok := strings.CutSuffix(file, "-stow")
	if !ok || !installer.ValidShortName(short) || dir == "" {
		return "", "", "", "", bad
	}
	work, name = splitLast(dir)
	if !installer.ValidName(name) {
		return "", "", "", "", bad
	}
	switch {
	case work == "" && strings.HasPrefix(dir, "/"):
		work = "/"
	case work == "":
		work = "."
	case work == ".":
	case !strings.HasPrefix(work, "/") || pathpkg.Clean(work) != work:
		// docker.work_dir is an absolute, clean path, or left out (the home: <name>/… alone).
		return "", "", "", "", fmt.Errorf("%q: the work directory %q is neither absolute and clean nor the home: give its absolute path, or <name>/<short>-stow for an install in the SSH user's home", id, work)
	}
	return host, work, name, short, nil
}

// splitLast is p before and after its last slash; no slash is "" and p.
func splitLast(p string) (string, string) {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i], p[i+1:]
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
	if targetKind(&m) == "vm" {
		return // forgotten: removing the VM is its hypervisor resource's job
	}
	timeout, diags := m.Timeouts.Delete(ctx, deleteTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if m.Kubernetes != nil {
		r.k8sDelete(ctx, &m, &resp.Diagnostics)
		return
	}
	d, err := r.docker(&m, m.ShortName.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Cannot reach the Docker target", err.Error())
		return
	}
	// The kept installer's describe first: one without the machine interface is refused here,
	// before uninstall is given --json; no install directory is an install already gone.
	running, err := d.KeptDescribe(ctx)
	if err != nil {
		addInstallerError(&resp.Diagnostics, "Reading the installer the install keeps", err)
		return
	}
	if running == nil {
		return
	}
	if _, err := d.Without(running).Uninstall(ctx, m.DeleteData.ValueBool()); err != nil {
		addInstallerError(&resp.Diagnostics, "Uninstalling", err)
	}
}
