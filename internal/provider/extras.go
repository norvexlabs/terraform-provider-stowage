package provider

import (
	"context"
	"fmt"
	"maps"
	"os"
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
	login   string // registry_login_wo
	kitCode string // kit_code_wo
}

// readWriteOnly reads them from cfg.
func readWriteOnly(ctx context.Context, cfg tfsdk.Config, diags *diag.Diagnostics) writeOnly {
	var login, code types.String
	diags.Append(cfg.GetAttribute(ctx, path.Root("registry_login_wo"), &login)...)
	diags.Append(cfg.GetAttribute(ctx, path.Root("kit_code_wo"), &code)...)
	return writeOnly{login: login.ValueString(), kitCode: code.ValueString()}
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

// kit is the kit kit_file names, read on the machine running OpenTofu, with its code: "" for
// none; an error in diags when it cannot be read or has no code.
func (r *installResource) kit(m *installModel, diags *diag.Diagnostics) (string, string) {
	if !known(m.KitFile) || m.KitFile.ValueString() == "" {
		return "", ""
	}
	p, err := expandHome(m.KitFile.ValueString())
	if err == nil {
		var b []byte
		if b, err = os.ReadFile(p); err == nil {
			if len(b) == 0 {
				err = fmt.Errorf("%s is empty", p)
			} else {
				if r.wo.kitCode == "" {
					diags.AddAttributeError(path.Root("kit_code_wo"), "kit_code_wo is not given", "A kit opens with its code: give kit_code_wo with kit_file.")
				}
				return string(b), r.wo.kitCode
			}
		}
	}
	diags.AddAttributeError(path.Root("kit_file"), "kit_file cannot be read", err.Error())
	return "", ""
}

// readBack puts into m what the install records of the settings a configuration gives that no
// field of state reads otherwise (status --json --values): a login the install no longer holds
// takes registry_login_wo_version out, so the next plan sends it again; the ACME directory and
// its root, in the modes that take them. Nothing without the values.
func readBack(m *installModel, v *installer.Values) {
	if v == nil {
		return
	}
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

// validateExtras checks the settings every type takes beside its own: the kit's code with its kit,
// the kit without given_wo, the ACME root's form, the login with its version and the version with
// its login. The kit's code is
// the create's to require (kit): a configuration may leave it out once the install took the kit.
func validateExtras(m *installModel, diags *diag.Diagnostics) {
	if m.KitFile.IsNull() && known(m.KitCodeWO) {
		diags.AddAttributeError(path.Root("kit_code_wo"), "kit_code_wo without kit_file", "The code opens a kit: give kit_file, or remove kit_code_wo.")
	}
	if !m.KitFile.IsNull() && !m.GivenWO.IsNull() {
		diags.AddAttributeError(path.Root("kit_file"), "kit_file with given_wo", "A kit gives what given_wo gives: give one of them.")
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
