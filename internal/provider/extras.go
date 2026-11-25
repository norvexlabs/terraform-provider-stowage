package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// writeOnly are the write-only values a create or an update sends, read from the configuration
// at its start (installResource.wo): never in a model, so never in state.
type writeOnly struct {
	login    string // registry_login_wo
	kit      string // kit_wo
	kitCode  string // kit_code_wo
	rclone   string // backup_rclone_config_wo
	identity string // restore_identity_wo
}

// readWriteOnly reads them from cfg.
func readWriteOnly(ctx context.Context, cfg tfsdk.Config, diags *diag.Diagnostics) writeOnly {
	var login types.String
	diags.Append(cfg.GetAttribute(ctx, path.Root("registry_login_wo"), &login)...)
	kit, code := readKitWO(ctx, cfg, diags)
	var rclone, identity types.String
	diags.Append(cfg.GetAttribute(ctx, path.Root("backup_rclone_config_wo"), &rclone)...)
	diags.Append(cfg.GetAttribute(ctx, path.Root("restore_identity_wo"), &identity)...)
	return writeOnly{login: login.ValueString(), kit: kit, kitCode: code, rclone: rclone.ValueString(), identity: identity.ValueString()}
}

// loginChanged says whether an update sends registry_login_wo: its version changed (an imported
// state records none, so a configured version is a change).
func loginChanged(plan, state *installModel) bool {
	return !plan.RegistryLoginWOVersion.Equal(state.RegistryLoginWOVersion) && !plan.RegistryLoginWOVersion.IsNull()
}

// login is the login an update sends: wo's when the version changed, "" otherwise; an error in
// diags when the version changed and no login is given.
func (r *installResource) login(plan, state *installModel, diags *diag.Diagnostics) string {
	if !loginChanged(plan, state) {
		return ""
	}
	if r.wo.login == "" {
		diags.AddAttributeError(path.Root("registry_login_wo"), "registry_login_wo is not given",
			"registry_login_wo_version changed, which sends the login again: give registry_login_wo in the same apply.")
	}
	return r.wo.login
}

// acmeChange is what a re-run gives of acme_ca and acme_ca_root, plan against was: the value, or
// none (installer.AcmeNone) for one taken out; "" for one unchanged.
func acmeChange(plan, was *installModel) (ca, root string) {
	pick := func(p, w types.String) string {
		switch {
		case p.IsUnknown() || p.Equal(w):
			return ""
		case p.IsNull() || p.ValueString() == "":
			if w.IsNull() || w.ValueString() == "" {
				return ""
			}
			return installer.AcmeNone
		}
		return p.ValueString()
	}
	return pick(plan.AcmeCA, was.AcmeCA), pick(plan.AcmeCARoot, was.AcmeCARoot)
}

// kit is the kit kit_wo gives the first install, with its code: "" for none (neither kit_wo nor
// kit_wo_version given); an error in diags for a kit without its code. A kit expected and absent
// sets r.kitLost, which refuseLostKit refuses where the kit would be given: a create finding an
// install there gives none, so a retry of one needs no kit.
func (r *installResource) kit(m *installModel, diags *diag.Diagnostics) (string, string) {
	if r.wo.kit == "" && !known(m.KitWOVersion) {
		if r.wo.kitCode != "" {
			diags.AddAttributeError(path.Root("kit_code_wo"), "kit_code_wo without kit_wo", "The code opens a kit: give kit_wo, or remove kit_code_wo.")
		}
		return "", ""
	}
	if r.wo.kit == "" {
		r.kitLost = true
		return "", ""
	}
	if needKit(r.wo.kit, r.wo.kitCode, diags); diags.HasError() {
		return "", ""
	}
	return r.wo.kit, r.wo.kitCode
}

// stillInstalling refuses a create retried while the first install a create before it began is
// marked as running (st: installing, not failed — reported only once its .env is written; a
// re-run of a finished install never marks it): that install may be taking
// what this create gives. then is the way on: apply again once it ends, or what to do when no run
// is under way (a run cut by a timeout leaves the mark).
func stillInstalling(st *installer.Status, who, then string, diags *diag.Diagnostics) bool {
	if st == nil || !st.Installing || st.InstallFailed != "" {
		return false
	}
	diags.AddError("The first install is still running",
		"A first install runs on (status --json installing: "+who+"): once it has finished, "+then)
	return true
}

// refuseLostKit adds the refusal of a kit expected and absent (r.kitLost) to diags, and says
// whether it did.
func (r *installResource) refuseLostKit(diags *diag.Diagnostics) bool {
	if r.kitLost {
		diags.AddAttributeError(path.Root("kit_wo"), "The kit is not here", kitLost("kit_wo"))
	}
	return r.kitLost
}

// readBack puts into m what the install records of the settings a configuration gives that no
// field of state reads otherwise (status --json --values): a login the install no longer holds
// takes registry_login_wo_version out, so the next plan sends it again; the ACME directory and
// its root, in the modes that take them. Nothing without the values.
func (r *installResource) readBack(m *installModel, v *installer.Values) {
	if v == nil {
		return
	}
	m.Inputs = inputsChanged(m.Inputs, r.recorded, v.Inputs)
	r.nowRecorded = v.Inputs
	if v.PullLogin != nil && !*v.PullLogin && v.PullLoginError == "" && known(m.RegistryLoginWOVersion) {
		m.RegistryLoginWOVersion = types.Int64Null()
	}
	if v.TLS != "acme" && v.TLS != "dns-cloudflare" {
		return
	}
	switch {
	case v.AcmeCA == "" && !m.AcmeCA.IsNull():
		m.AcmeCA = types.StringNull()
	case v.AcmeCA != "":
		m.AcmeCA = types.StringValue(v.AcmeCA)
	}
	switch {
	case v.AcmeCARootError != "":
	case v.AcmeCARoot == "":
		m.AcmeCARoot = types.StringNull()
	case v.AcmeCARoot != caDigest(m.AcmeCARoot.ValueString()):
		// Another root than state's: its digest, which the next plan replaces with the
		// configuration's root.
		m.AcmeCARoot = types.StringValue(v.AcmeCARoot)
	}
}

// inputsChanged is inputs with the value the install records now for each key it gives whose
// record changed since the refresh before (was: a kit taken, a re-run by hand) and differs from
// inputs': the next plan then gives the configuration's again, never leaving the install apart
// from it unseen. A value the installer records otherwise than given (an empty value as its
// default) records the same at every refresh, so it is never a change; nor is anything at the
// first refresh after an apply (was empty: the baseline). A file input is recorded by its digest.
func inputsChanged(inputs types.Map, was, now map[string]string) types.Map {
	if !known(inputs) || now == nil || was == nil {
		return inputs
	}
	out := maps.Clone(inputs.Elements())
	changed := false
	for _, k := range slices.Sorted(maps.Keys(inputs.Elements())) {
		s, ok := inputs.Elements()[k].(types.String)
		rec, has := now[k]
		before, had := was[k]
		if !ok || !known(s) || !has || !had || rec == before || rec == s.ValueString() || rec == caDigest(s.ValueString()) {
			continue
		}
		out[k], changed = types.StringValue(rec), true
	}
	if !changed {
		return inputs
	}
	return types.MapValueMust(types.StringType, out)
}

// recordedKey is the private state's key of the input values the last refresh read.
const recordedKey = "recorded_inputs"

// getRecorded is the input values the last refresh read; nil for none (the first refresh after
// an apply).
func getRecorded(ctx context.Context, p interface {
	GetKey(context.Context, string) ([]byte, diag.Diagnostics)
}, diags *diag.Diagnostics) map[string]string {
	if p == nil {
		return nil
	}
	b, d := p.GetKey(ctx, recordedKey)
	diags.Append(d...)
	var out map[string]string
	if len(b) == 0 || json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}

// setRecorded keeps v as the input values this refresh read.
func setRecorded(ctx context.Context, p interface {
	SetKey(context.Context, string, []byte) diag.Diagnostics
}, v map[string]string, diags *diag.Diagnostics) {
	b, err := json.Marshal(v)
	if err != nil || p == nil {
		return
	}
	diags.Append(p.SetKey(ctx, recordedKey, b)...)
}

// noteRecorded keeps v's inputs as what the install records now (nowRecorded), when it tells.
func (r *installResource) noteRecorded(v *installer.Values) {
	if v != nil && v.Inputs != nil {
		r.nowRecorded = v.Inputs
	}
}

// keepRecorded keeps, after a create or an update that read the install at its end (settle), what
// it recorded then as the baseline of the next refresh: a change after it (a kit taken later in
// the apply, a run by hand) shows there. Without that read, the private state is left as it is.
func (r *installResource) keepRecorded(ctx context.Context, p interface {
	SetKey(context.Context, string, []byte) diag.Diagnostics
}, diags *diag.Diagnostics) {
	if r.nowRecorded != nil && !diags.HasError() {
		setRecorded(ctx, p, r.nowRecorded, diags)
	}
}

// validateExtras checks the settings every type takes beside its own: the kit's code with its kit,
// the kit without given_wo, the ACME root's form, the login with its version and the version with
// its login. The kit's code is
// the create's to require (kit): a configuration may leave it out once the install took the kit.
func validateExtras(m *installModel, diags *diag.Diagnostics) {
	if m.KitWO.IsNull() && m.KitWOVersion.IsNull() && known(m.KitCodeWO) {
		diags.AddAttributeError(path.Root("kit_code_wo"), "kit_code_wo without kit_wo", "The code opens a kit: give kit_wo, or remove kit_code_wo.")
	}
	if (!m.KitWO.IsNull() || !m.KitWOVersion.IsNull()) && !m.GivenWO.IsNull() {
		diags.AddAttributeError(path.Root("kit_wo"), "kit_wo with given_wo", "A kit gives what given_wo gives: give one of them.")
	}
	if r := m.AcmeCARoot.ValueString(); known(m.AcmeCARoot) && !strings.Contains(r, "-----BEGIN CERTIFICATE-----") {
		diags.AddAttributeError(path.Root("acme_ca_root"), "acme_ca_root is not PEM",
			"Give the root certificate's PEM (-----BEGIN CERTIFICATE-----), e.g. file(\"acme-root.pem\").")
	}
	if known(m.RegistryLoginWOVersion) && m.RegistryLoginWO.IsNull() {
		diags.AddAttributeError(path.Root("registry_login_wo_version"), "registry_login_wo_version without registry_login_wo",
			"The version says when registry_login_wo is sent again, and no login is given: give registry_login_wo with it, or remove registry_login_wo_version.")
	}
	if !m.RegistryLoginWO.IsNull() && m.RegistryLoginWOVersion.IsNull() {
		diags.AddAttributeWarning(path.Root("registry_login_wo_version"), "registry_login_wo_version is not set",
			"A write-only value is never compared: without a version to bump, the login is sent at create alone, and a later change is never sent.")
	}
	if m.Kubernetes != nil && m.Kubernetes.ImagePullLogin.ValueBool() && !m.RegistryLoginWO.IsNull() {
		diags.AddAttributeError(path.Root("registry_login_wo"), "registry_login_wo with image_pull_login",
			"Both write the release's pull Secret: registry_login_wo from the release's own login, image_pull_login from the provider's pull account. Give one.")
	}
	if m.VM != nil {
		for _, role := range mapKeys(m.VM.Interfaces) {
			i, ok := interfaceOf(m.VM.Interfaces, role)
			p := path.Root("vm").AtName("interfaces").AtMapKey(role)
			switch {
			case !ok:
			case i.Mode == "static" && i.Address == "":
				diags.AddAttributeError(p.AtName("address"), "address is required with static", "Give the NIC's IPv4 address with its prefix, e.g. 192.0.2.20/24.")
			case i.Mode != "static" && i.Address != "":
				diags.AddAttributeError(p.AtName("address"), "address without static", `An address is a static NIC's: set mode = "static", or remove address.`)
			}
		}
	}
}

// interfaceOf is the interfaces entry of role, when its values are known.
func interfaceOf(m types.Map, role string) (installer.Interface, bool) {
	o, ok := m.Elements()[role].(types.Object)
	if !ok || !known(o) {
		return installer.Interface{}, false
	}
	a := o.Attributes()
	s := func(n string) (string, bool) {
		v, ok := a[n].(types.String)
		return v.ValueString(), ok && !v.IsUnknown()
	}
	mac, ok1 := s("mac")
	mode, ok2 := s("mode")
	addr, ok3 := s("address")
	return installer.Interface{Role: role, MAC: mac, Mode: mode, Address: addr}, ok1 && ok2 && ok3
}

// attachments are the interfaces plan adds or changes against state's, sorted by role.
func attachments(plan, state types.Map) []installer.Interface {
	var out []installer.Interface
	was := map[string]attr.Value{}
	if known(state) {
		was = state.Elements()
	}
	if !known(plan) {
		return nil
	}
	for _, role := range slices.Sorted(maps.Keys(plan.Elements())) {
		if old, ok := was[role]; ok && old.Equal(plan.Elements()[role]) {
			continue
		}
		if i, ok := interfaceOf(plan, role); ok {
			out = append(out, i)
		}
	}
	return out
}

// consoles are where an operator watches a VM's boot, by platform.
var consoles = map[string]string{
	"truenas":  "its display on TrueNAS",
	"proxmox":  "its console in Proxmox VE",
	"libvirt":  "`virsh console` on the libvirt host",
	"vmware":   "its console in vSphere or Workstation",
	"hyperv":   "its console in Hyper-V Manager",
	"metal":    "the machine's own screen",
	"azure":    "its serial console in the Azure portal",
	"gcp":      "its serial port output in the Google Cloud console",
	"aws":      "its serial console or system log in the EC2 console",
	"scaleway": "the instance's Console tab in the Scaleway console",
}

// consoleHint is err with where to watch the VM's boot, when platform names a console.
func consoleHint(err error, platform types.String) error {
	if c, ok := consoles[platform.ValueString()]; ok && err != nil {
		return fmt.Errorf("%w. The VM's boot shows on %s", err, c)
	}
	return err
}
