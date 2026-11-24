package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/fetch"
	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"go.yaml.in/yaml/v3"
)

// dnsLabel is a Kubernetes namespace's form, and a helm release's (at most 53 characters).
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// validateK8s checks the Kubernetes type's own attributes (the model's kubernetes block). The
// type has none of the front's (tls, domains, ports, address, acme_ca), nor secrets_wo: a release
// keeps its secrets in the cluster.
func validateK8s(m *installModel, diags *diag.Diagnostics) {
	b, p := m.Kubernetes, path.Root("kubernetes")
	if b.Namespace.IsNull() {
		diags.AddAttributeError(p.AtName("namespace"), "namespace is required", "Give the namespace the release lives in.")
	}
	for name, v := range map[string]types.String{"namespace": b.Namespace, "release": b.Release} {
		if known(v) && (!dnsLabel.MatchString(v.ValueString()) || name == "release" && len(v.ValueString()) > 53) {
			diags.AddAttributeError(p.AtName(name), name+" is not a DNS label",
				fmt.Sprintf("%q: lower-case letters, digits and -, starting and ending with a letter or digit, at most 63 characters (53 for a release).", v.ValueString()))
		}
	}
	if known(b.Values) {
		if err := chartValues(b.Values.ValueString()); err != nil {
			diags.AddAttributeError(p.AtName("values"), "values is not a chart values file", err.Error())
		}
	}
}

// chartValues refuses what helm does not take as a values file: YAML that does not parse, more
// than one document, or a document that is not a mapping. Empty is refused too: leaving values out
// gives none.
func chartValues(v string) error {
	var doc, more any
	dec := yaml.NewDecoder(strings.NewReader(v))
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("it does not parse as YAML: %v", err)
	}
	// An empty document after it is read as none, as helm does.
	if err := dec.Decode(&more); err != nil && !errors.Is(err, io.EOF) || err == nil && more != nil {
		return errors.New("it holds more than one YAML document: give one mapping")
	}
	switch doc.(type) {
	case map[string]any, map[any]any: // a key that is no string (1: a) decodes to the latter
	default:
		return errors.New("its document is not a mapping of chart values (`yamlencode({...})`); leave values out to give none")
	}
	return nil
}

// valuesChanged says whether plan gives the release chart values state's run did not: a change
// the release is applied again for. Values left out give none: the release keeps its own.
func valuesChanged(plan, state *installModel) bool {
	if plan.Kubernetes == nil || plan.Kubernetes.Values.IsNull() {
		return false
	}
	return state.Kubernetes == nil || !plan.Kubernetes.Values.Equal(state.Kubernetes.Values)
}

// validateK8sConn checks the provider's kubernetes block: host with its credentials, or a
// kubeconfig file and context. A value not known yet (a cluster made in the same apply) is
// checked when the apply configures the provider again.
func validateK8sConn(b *k8sConnModel, diags *diag.Diagnostics) {
	p := path.Root("kubernetes")
	set := func(v interface{ IsNull() bool }) bool { return !v.IsNull() }
	withHost := map[string]bool{
		"token": set(b.Token), "cluster_ca_certificate": set(b.ClusterCACertificate), "client_certificate": set(b.ClientCertificate),
		"client_key": set(b.ClientKey), "insecure": b.Insecure.ValueBool(), "tls_server_name": set(b.TLSServerName), "proxy_url": set(b.ProxyURL), "exec": b.Exec != nil,
	}
	if b.Host.IsNull() {
		for _, name := range sortedTrue(withHost) {
			diags.AddAttributeError(p.AtName(name), name+" without host",
				fmt.Sprintf("%s goes with host, the API server's URL: give host, or remove %s (config_path names a kubeconfig file instead).", name, name))
		}
		return
	}
	for _, f := range []struct {
		name string
		v    types.String
	}{{"config_path", b.ConfigPath}, {"config_context", b.ConfigContext}} {
		if !f.v.IsNull() {
			diags.AddAttributeError(p.AtName(f.name), f.name+" with host",
				fmt.Sprintf("host and %s are two ways to reach the cluster: give the API server's values (host, its credentials), or a kubeconfig (config_path, config_context).", f.name))
		}
	}
	if b.Host.IsUnknown() {
		return
	}
	if h := b.Host.ValueString(); !strings.HasPrefix(h, "https://") && !strings.HasPrefix(h, "http://") {
		diags.AddAttributeError(p.AtName("host"), "host is not a URL", fmt.Sprintf("%q: give the API server's URL, https://<host>[:<port>].", h))
	}
	if set(b.ClientCertificate) != set(b.ClientKey) {
		diags.AddAttributeError(p.AtName("client_key"), "client_certificate and client_key go together", "Give the client certificate and its key, or neither.")
	}
	if !set(b.Token) && !set(b.ClientCertificate) && b.Exec == nil {
		diags.AddAttributeError(p.AtName("host"), "no credential for host",
			"Give token, client_certificate and client_key, or an exec block: the API server is reached with one of them.")
	}
	if b.Insecure.ValueBool() && set(b.ClusterCACertificate) {
		diags.AddAttributeError(p.AtName("insecure"), "insecure with cluster_ca_certificate", "kubectl refuses a CA with the check skipped: give one or the other.")
	}
	if e := b.Exec; e != nil {
		for name, v := range map[string]types.String{"api_version": e.APIVersion, "command": e.Command} {
			if v.IsNull() {
				diags.AddAttributeError(p.AtName("exec").AtName(name), "exec."+name+" is required", "Give the credential plugin's "+strings.ReplaceAll(name, "_", " ")+".")
			}
		}
	}
}

func sortedTrue(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		if v {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// registryHostOf is the host of a registry prefix (`<host>/<path>`): the one a login names.
func registryHostOf(prefix string) string {
	host, _, _ := strings.Cut(prefix, "/")
	return host
}

// pullLogin is the login image_pull_login writes the cluster's pull Secret from: the mirror's own
// (registry_username), else the provider's pull account for the host of the registry m's release
// pulls from — the one state records (the provider's at the install), else the provider's.
func (r *installResource) pullLogin(m *installModel) *installer.Login {
	if l := r.data.mirrorLogin(); l != nil {
		return l // the mirror's own login, for the Secret the cluster pulls from it with
	}
	reg := r.registry()
	if known(m.Registry) && m.Registry.ValueString() != "" {
		reg = m.Registry.ValueString()
	}
	return &installer.Login{Host: registryHostOf(reg), Username: r.data.Username, Password: r.data.Password}
}

// planPullLogin refuses image_pull_login without what it needs: the provider's pull account and
// a mirror (the provider's registry, or the one state records).
func (r *installResource) planPullLogin(plan, state *installModel, diags *diag.Diagnostics) {
	if plan.Kubernetes == nil || !plan.Kubernetes.ImagePullLogin.ValueBool() || r.data == nil {
		return
	}
	p := path.Root("kubernetes").AtName("image_pull_login")
	if r.data.Username == "" && r.data.RegistryUsername == "" && !r.data.UsernameUnknown {
		diags.AddAttributeError(p, "image_pull_login needs the pull account", "Give the provider's username and password: the cluster's pull Secret is written from them.")
	}
	held := state != nil && known(state.Registry) && state.Registry.ValueString() != ""
	if r.data.RegistryKnown && r.data.Registry == "" && !held {
		diags.AddAttributeError(p, "image_pull_login needs a mirror", "Give the provider's registry, the mirror the cluster pulls from: the release's own registry takes no pull Secret from the provider.")
	}
}

// k8sDriver is the driver of m's release for the project name, with no runner or directory yet.
func k8sDriver(m *installModel, name string) installer.K8s {
	return installer.K8s{Name: name, Namespace: m.Kubernetes.Namespace.ValueString(), Release: m.Kubernetes.Release.ValueString()}
}

// k8sModifyPlan refuses a change of the release's namespace or name (another release is another
// install), and a plan that changes a drifted state's release (one recording an installer the
// provider does not run) — but for an installer the configuration's registry trusts or that is not
// known yet (the apply checks it), a plan of the provider's own settings alone, and while the
// provider's cluster is not known (its read was skipped).
func (r *installResource) k8sModifyPlan(ctx context.Context, plan, state *installModel, resp *resource.ModifyPlanResponse) {
	diags := &resp.Diagnostics
	if plan.Kubernetes == nil || state.Kubernetes == nil {
		return
	}
	// A release recording an installer the provider does not run: no plan runs anything over it,
	// but one the configuration's registry trusts (the update then takes its usual way), one of the
	// provider's own settings alone (applied without the cluster), and one whose cluster is not known
	// yet (it is being made again: its read was skipped).
	if ref, ok := driftRef(state); ok && len(remoteChange(plan, state)) > 0 && (r.data == nil || r.data.Kubernetes == nil || connKnown(r.data.Kubernetes)) {
		k := k8sDriver(state, installName(state))
		if !known(plan.Installer) {
			// The registry is checked at apply; another project's, or no installer, is refused now.
			if n, _, err := installer.ParseInstaller(ref); err != nil || n != installName(state) {
				untrustedDiag(diags, true, "", k, ref, "not this project's installer by digest", "the configured installer")
			}
		} else if err := r.trusted(ref, installName(state), plan.Installer); err != nil {
			untrustedDiag(diags, true, "", k, ref, err, plan.Installer.ValueString())
		}
	}
	if plan.Kubernetes.Values.IsNull() && known(state.Kubernetes.Values) {
		diags.AddAttributeWarning(path.Root("kubernetes").AtName("values"), "values left out: the release keeps its values",
			"Nothing runs for it: helm keeps the values the earlier runs gave (`--reset-then-reuse-values`). To drop a key, give it null in values and apply, then leave values out.")
	}
	if !plan.Kubernetes.Namespace.IsUnknown() {
		firstInstallOnly(diags, path.Root("kubernetes").AtName("namespace"), "namespace", state.Kubernetes.Namespace.ValueString(), plan.Kubernetes.Namespace.ValueString())
	}
	if plan.Kubernetes.Release.IsUnknown() || !known(plan.Installer) {
		return
	}
	name, _, _ := parseInstaller(plan.Installer.ValueString())
	was := baseName(state.ID.ValueString()) // <namespace>/<release>
	if now := k8sDriver(plan, name).ReleaseName(); was != "" && was != now {
		firstInstallOnly(diags, path.Root("kubernetes").AtName("release"), "the helm release (release, else the project's name)", was, now)
	}
}

// k8sRun is a Kubernetes run's private directories on this machine and the driver set for it.
// dir, in Data.TempDir (never the cache), holds what may hold a credential: the kubeconfig written,
// and each installer's working directory (dir/<sub>: its bootstrap script `<name>.sh`, whatever the
// installer writes in it). progs, in the cache's runs/ (Data.runDir), holds only each installer's
// program (progs/<sub>/<short>-stow), linked to the one the cache keeps. The run's files on
// standard input (secret inputs, logins, values) go to a directory the run's script makes with
// mktemp -d and removes. close removes both.
type k8sRun struct {
	r     *installResource
	dir   string
	progs string
	k     installer.K8s
	// configured are the installers the configuration names (Want.Configured).
	configured []string
	// keys are the keys each installer fetched carries, by reference.
	keys map[string]map[string][]byte
	// installed, while set, is the trust of the release installed, which a fetch must meet: the
	// new release's, on an upgrade.
	installed *fetch.Trust
}

// k8sStart prepares a run on m's cluster for the project name: the platform and the programs
// checked, the private directory made, the kubeconfig written. Nil, with the error in diags,
// otherwise; the caller closes it.
func (r *installResource) k8sStart(ctx context.Context, m *installModel, name string, diags *diag.Diagnostics) *k8sRun {
	if r.data == nil {
		diags.AddError("The provider is not configured", "the provider block was not read")
		return nil
	}
	runner, err := r.data.NewRunner(Connection{})
	if err != nil {
		diags.AddError("Cannot run the installer here", err.Error())
		return nil
	}
	dir, err := os.MkdirTemp(r.data.TempDir, "stowage-k8s-") // 0700
	if err != nil {
		diags.AddError("A private directory for the run", err.Error())
		return nil
	}
	progs, err := r.data.runDir("stowage-k8s-")
	if err != nil {
		_ = os.RemoveAll(dir)
		diags.AddError("A private directory for the run", err.Error())
		return nil
	}
	run := &k8sRun{r: r, dir: dir, progs: progs, keys: map[string]map[string][]byte{}}
	if known(m.Installer) {
		run.configured = append(run.configured, m.Installer.ValueString())
	}
	run.k = k8sDriver(m, name)
	run.k.Runner = busyRunner{Runner: installer.Filtered(runner), dir: progs}
	run.k.Mirror = r.data.mirrorLogin()
	if m.Kubernetes.ImagePullLogin.ValueBool() {
		run.k.PullLogin = r.pullLogin(m)
	}
	run.k.Values = m.Kubernetes.Values.ValueString()
	b := r.data.Kubernetes
	if b == nil {
		b = &k8sConnModel{}
	}
	if !connKnown(b) {
		diags.AddError("The provider's kubernetes block is not known yet",
			"A value of the provider's kubernetes block comes from a resource not applied yet: the install runs once the cluster's values are known (in the apply that makes them).")
		run.close()
		return nil
	}
	run.k.Context = b.ConfigContext.ValueString()
	switch {
	case known(b.Host):
		cfg := kubeconfig(ctx, b, diags)
		if diags.HasError() {
			run.close()
			return nil
		}
		run.k.Kubeconfig = filepath.Join(dir, "kubeconfig")
		if err := os.WriteFile(run.k.Kubeconfig, cfg, 0o600); err != nil {
			diags.AddError("Writing the kubeconfig", err.Error())
			run.close()
			return nil
		}
	case known(b.ConfigPath):
		p, err := expandHome(b.ConfigPath.ValueString())
		if err != nil {
			diags.AddError("The provider's kubernetes.config_path", err.Error())
			run.close()
			return nil
		}
		run.k.Kubeconfig = p
	case os.Getenv("KUBECONFIG") != "":
		// kubectl's default, made absolute: the installer runs in the run's directory.
		var files []string
		for _, f := range filepath.SplitList(os.Getenv("KUBECONFIG")) {
			if f == "" {
				continue
			}
			abs, err := filepath.Abs(f)
			if err != nil {
				diags.AddError("KUBECONFIG", err.Error())
				run.close()
				return nil
			}
			files = append(files, abs)
		}
		run.k.Kubeconfig = strings.Join(files, string(filepath.ListSeparator))
	}
	missing, err := run.k.Missing(ctx)
	switch {
	case err != nil:
		addInstallerError(diags, "Looking for the programs a Kubernetes run needs", err)
	case len(missing) > 0:
		diags.AddError("Missing on this machine: "+strings.Join(missing, ", "),
			"The Kubernetes target runs the installer on the machine running OpenTofu, which drives the cluster with helm (3.14 or later, which the installer checks) and kubectl, each found on PATH. Install "+strings.Join(missing, ", ")+".")
	}
	if diags.HasError() {
		run.close()
		return nil
	}
	return run
}

// close removes the run's directories: the kubeconfig, the working directories and the installers
// fetched.
func (run *k8sRun) close() {
	_ = os.RemoveAll(run.dir)
	_ = os.RemoveAll(run.progs)
}

// in is the driver working in the run's subdirectory sub (made once).
func (run *k8sRun) in(sub string, diags *diag.Diagnostics) (installer.K8s, bool) {
	k := run.k
	k.Dir = filepath.Join(run.dir, sub)
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		diags.AddError("A private directory for the run", err.Error())
		return k, false
	}
	return k, true
}

// fetch writes ref's bootstrap script in sub and reads its describe: of the run's project, and
// offering the Kubernetes shape. Nil, with the error in diags, otherwise.
func (run *k8sRun) fetch(ctx context.Context, sub, ref string, diags *diag.Diagnostics) (installer.K8s, *installer.Describe) {
	k, ok := run.in(sub, diags)
	if !ok {
		return k, nil
	}
	if err := run.script(ctx, k, ref); err != nil {
		addInstallerError(diags, "Fetching the installer", err)
		return k, nil
	}
	desc, err := k.Describe(ctx)
	if err != nil {
		addInstallerError(diags, "Reading the installer", err)
		return k, nil
	}
	if desc.Name != k.Name {
		diags.AddAttributeError(path.Root("installer"), "installer of another project",
			fmt.Sprintf("%s describes the project %q, not %q its image is named after.", ref, desc.Name, k.Name))
		return k, nil
	}
	if !slices.Contains(desc.Shapes, "kubernetes") {
		diags.AddError("No Kubernetes shape", fmt.Sprintf("%s %s offers %s, not kubernetes.", desc.Name, desc.Release.Version, strings.Join(desc.Shapes, ", ")))
		return k, nil
	}
	return k.Without(desc), desc
}

// expandHome is p with a leading ~/ the home directory, made absolute: the installer runs in the
// run's directory.
func expandHome(p string) (string, error) {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, rest)
	}
	return filepath.Abs(p)
}

// connKnown says whether every value of the provider's kubernetes block is known.
func connKnown(b *k8sConnModel) bool {
	for _, v := range []attr.Value{b.Host, b.Token, b.ClusterCACertificate, b.ClientCertificate, b.ClientKey, b.Insecure, b.TLSServerName, b.ProxyURL, b.ConfigPath, b.ConfigContext} {
		if v.IsUnknown() {
			return false
		}
	}
	e := b.Exec
	return e == nil || !e.APIVersion.IsUnknown() && !e.Command.IsUnknown() && wholyKnown(e.Args) && wholyKnown(e.Env)
}

// kitNotGiven names the kit among the settings a take-over gives not, when one is configured.
func kitNotGiven(configured bool) string {
	if !configured {
		return ""
	}
	return "; kit_wo with kit_code_wo, as a release is there (stowage_kit_take gives a kit to it)"
}

// k8sID is the resource's ID on Kubernetes.
func k8sID(k installer.K8s) string { return k.Namespace + "/" + k.ReleaseName() }

// helmBusy is the refusal of a release helm is working on (why).
func helmBusy(diags *diag.Diagnostics, why string, k installer.K8s) {
	diags.AddError("helm is working on the release",
		fmt.Sprintf("%s (%s): apply again once that is over (`k8s status`). A helm killed in its operation leaves it so for good: then `helm rollback %s --namespace %s` (or `helm uninstall`) ends it.", why, k8sID(k), k.ReleaseName(), k.Namespace))
}

// refuseRunEnvInputs refuses an input named as a variable a run on this machine keeps from
// OpenTofu's environment (installer.RunEnv) while that environment sets it: the installer would
// take the environment's value over the one given.
// The run sets runSets itself (KUBECONFIG on Kubernetes): an input of those names is refused
// whatever the environment holds. givenWO's input keys are checked too.
func refuseRunEnvInputs(diags *diag.Diagnostics, runSets []string, givenWO string, given ...map[string]string) {
	keys := installer.GivenKeys(givenWO)
	for _, m := range given {
		keys = append(keys, slices.Collect(maps.Keys(m))...)
	}
	slices.Sort(keys)
	for _, k := range slices.Compact(keys) {
		_, set := os.LookupEnv(k)
		switch {
		case slices.Contains(runSets, k):
			diags.AddError(k+" is set by the run",
				fmt.Sprintf("%s is an input, and a variable the installer's run on this machine sets itself: the installer would take the run's value over the one given. An input of this name cannot be given by this target: leave it to its default.", k))
		case set && installer.RunEnv(k):
			diags.AddError(k+" comes from the environment",
				fmt.Sprintf("%s is an input, and a variable the installer's run on this machine needs from OpenTofu's environment: the installer would take that value over the one given. An input of this name cannot be given by a run on this machine: leave it to its default (the environment's value then applies).", k))
		}
	}
}

// k8sStatus is m's status with helm's: `pending: <why>` while helm works on the release (but a
// first install, `installing`), `failed: <why>` when its latest revision is neither that nor
// deployed.
func k8sStatus(m *installModel, st *installer.Status) {
	if st.InstallFailed != "" && strings.HasPrefix(m.Status.ValueString(), driftPrefix) {
		// An install_failed text never reads as the drift, which only markDrift writes.
		m.Status = types.StringValue("failed: install failed: " + st.InstallFailed)
	}
	if st.Installed == nil {
		return // nothing runs (uninstalled, its history kept): applyStatus's not installed
	}
	if busy, failed := helmState(st); busy != "" {
		m.Status = types.StringValue("pending: " + busy)
	} else if failed != "" {
		m.Status = types.StringValue("failed: " + failed)
	}
}

// helmState tells helm's status of the release's latest revision: busy while helm works on it
// (pending-…, uninstalling; a first install's pending-install is status's installing, neither),
// failed when it is neither that nor deployed (failed, superseded, uninstalled, unknown), each the
// diagnostic's words; both "" when deployed, or when status reports no helm (an older installer).
func helmState(st *installer.Status) (busy, failed string) {
	h := st.Helm
	if h == nil || h.Status == "deployed" || h.Status == "pending-install" && st.Installing {
		return "", ""
	}
	why := fmt.Sprintf("helm's latest revision (%d) of the release is %s", h.Revision, h.Status)
	if strings.HasPrefix(h.Status, "pending-") || h.Status == "uninstalling" {
		return why, ""
	}
	return "", why
}

// helmBusyOf is helmState's busy alone.
func helmBusyOf(st *installer.Status) string {
	busy, _ := helmState(st)
	return busy
}

// helmBroken is why the release is not deployed (busy or failed), "" when it is.
func helmBroken(st *installer.Status) string {
	busy, failed := helmState(st)
	return busy + failed
}

func (r *installResource) k8sCreate(ctx context.Context, m *installModel, in installer.Install, resp *resource.CreateResponse) {
	diags := &resp.Diagnostics
	name := installName(m)
	run := r.k8sStart(ctx, m, name, diags)
	if run == nil {
		return
	}
	defer run.close()
	run.k.RegistryLogin = r.wo.login
	ref := m.Installer.ValueString()
	k, desc := run.fetch(ctx, "run", ref, diags)
	if desc == nil {
		return
	}
	refuseSecretInputs(diags, in.Inputs, desc)
	refuseRunEnvInputs(diags, []string{"KUBECONFIG"}, in.Given, in.Inputs, in.SecretInputs)
	if diags.HasError() {
		return
	}
	existing, err := k.Status(ctx)
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		return
	}
	var busy, failed string
	var existingUntrusted *untrustedRun
	if existing != nil {
		busy, failed = helmState(existing)
		if existing.Name == name {
			_, existingUntrusted = r.k8sTrust(existing, name, m.Installer)
		}
	}
	switch {
	case existing == nil:
		if r.refuseLostKit(diags) {
			return
		}
		if _, err := k.Install(ctx, in); err != nil {
			addInstallerError(diags, "Installing "+desc.Name, err)
			return
		}
	case existing.Name != name:
		diags.AddError("Another project's release", fmt.Sprintf("The release %s holds %q, not %q: give another release or namespace.", k8sID(k), existing.Name, name))
		return
	case existingUntrusted != nil:
		untrustedDiag(diags, true, "", k, existingUntrusted.ref, existingUntrusted.why, ref)
		return
	case existing.Installed != nil && existing.Installed.Installer != "" && existing.Installed.Installer != ref:
		diags.AddError("The release runs another installer",
			fmt.Sprintf("The release %s runs %s %s from %s: set installer to it, apply (the release is taken over), then change installer to upgrade it.", k8sID(k), name, existing.Installed.Version, existing.Installed.Installer))
		return
	case existing.Installed != nil && existing.Installed.Installer == "" && !existing.Installed.Same(desc.Release):
		diags.AddError("The release runs another release",
			fmt.Sprintf("The release %s runs %s %s (revision %d), and records no installer; %s carries %s (revision %d): set installer to %s %s's, apply (the release is taken over), then change installer to upgrade it.",
				k8sID(k), name, existing.Installed.Version, existing.Installed.Revision, ref, desc.Release.Version, desc.Release.Revision, name, existing.Installed.Version))
		return
	case existing.Installing || busy != "":
		why := busy
		if why == "" {
			why = "helm is installing the release"
		}
		helmBusy(diags, why, k)
		return
	case existing.Installed == nil:
		// Uninstalled with its history kept: the installer's install decides.
		if r.refuseLostKit(diags) {
			return
		}
		if _, err := k.Install(ctx, in); err != nil {
			addInstallerError(diags, "Installing "+desc.Name, err)
			return
		}
	default:
		diags.AddWarning("A release is already there",
			fmt.Sprintf("The release %s already runs %s: the create takes it over, giving it given_wo, then inputs and secret_inputs_wo, again (k8s upgrade --given), which replace the values it records; "+
				"with nothing to give, a release whose latest helm revision failed, or with values, is applied again (k8s upgrade). "+
				"The settings only a first install takes (the provider's registry, registry_ca%s) are not given: the release keeps what it recorded.", k8sID(k), name, kitNotGiven(in.Kit != "" || r.kitLost)))
		if in.Given != "" {
			if _, err := k.Give(ctx, in.Given, r.registryCA()); err != nil {
				addInstallerError(diags, "Giving "+desc.Name+" given_wo", err)
				return
			}
		}
		c := installer.Change{Inputs: in.Inputs, SecretInputs: in.SecretInputs}
		switch {
		case !c.Empty():
			if _, err := k.Reconfigure(ctx, c, r.registryCA()); err != nil {
				addInstallerError(diags, "Reconfiguring "+desc.Name, err)
				return
			}
		case (failed != "" || k.Values != "") && in.Given == "":
			// A Give or a Reconfigure gave the values already.
			if _, err := k.Reapply(ctx, r.registryCA()); err != nil {
				addInstallerError(diags, "Applying "+desc.Name+" again", err)
				return
			}
		}
	}
	st, err := r.k8sSettle(ctx, k)
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		return
	}
	if why := helmBroken(st); why != "" {
		diags.AddError("Installing "+desc.Name+" left the release not deployed", why+": apply again once it is fixed (`k8s status`).")
		return
	}
	m.ID = types.StringValue(k8sID(k))
	m.Registry = types.StringValue(r.registry())
	m.RegistryCADigest = r.providerCA()
	m.ShortName = types.StringValue(desc.ShortName)
	st, u := r.k8sTrust(st, name, m.Installer)
	applyStatus(m, st, false)
	k8sStatus(m, st)
	if u != nil {
		// The install ran: state is saved (so nothing is left untracked), the drift in its status,
		// with a warning (an error would taint the resource, whose replacement uninstalls it).
		markDrift(m, u)
		untrustedDiag(diags, false, "After the install, ", k, u.ref, u.why, m.Installer.ValueString())
	}
	diags.Append(resp.State.Set(ctx, m)...)
}

// k8sSettle reads the release until helm no longer installs it (pending-install): every Poll,
// within ctx's deadline.
func (r *installResource) k8sSettle(ctx context.Context, k installer.K8s) (*installer.Status, error) {
	for {
		st, err := k.StatusOf(ctx, true)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("the release %s is still installing when the timeout ran out (the read: %w): apply again once status --json no longer reports installing", k8sID(k), err)
		}
		if err != nil {
			return nil, err
		}
		if st == nil {
			return nil, fmt.Errorf("the install succeeded but the namespace holds no release %s", k8sID(k))
		}
		if !st.Installing {
			r.noteRecorded(st.Values)
			return st, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("the release %s is still installing when the timeout ran out: apply again once status --json no longer reports installing", k8sID(k))
		case <-time.After(r.data.Poll):
		}
	}
}

func (r *installResource) k8sRead(ctx context.Context, m *installModel, state *tfsdk.State, diags *diag.Diagnostics) {
	if imported(m) || !known(m.Installer) {
		return // an imported state: the configuration gives the cluster and the installer, at the first apply
	}
	if r.data != nil && r.data.Kubernetes != nil && !connKnown(r.data.Kubernetes) {
		return // the cluster is being made again in this plan: state stands until the apply reaches it
	}
	name := installName(m)
	run := r.k8sStart(ctx, m, name, diags)
	if run == nil {
		return
	}
	defer run.close()
	k, ok := run.in("run", diags)
	if !ok {
		return
	}
	if err := run.script(ctx, k, m.Installer.ValueString()); err != nil {
		addInstallerError(diags, "Fetching the installer", err)
		return
	}
	st, err := k.StatusOf(ctx, true)
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		return
	}
	if st == nil {
		state.RemoveResource(ctx)
		return
	}
	r.readBack(m, st.Values)
	if st.Name != name {
		diags.AddError("Another project's release", fmt.Sprintf("The release %s holds %q, not %q.", k8sID(run.k), st.Name, name))
		return
	}
	st, u := r.k8sTrust(st, name, m.Installer)
	applyStatus(m, st, true)
	k8sStatus(m, st)
	if u != nil {
		// Never put in state, which the next run fetches and runs: state keeps its installer, and
		// status says the drift, which makes a plan an error (k8sModifyPlan).
		untrustedDiag(diags, false, "State keeps its installer, and a plan changing the release is an error until this is fixed: ", run.k, u.ref, u.why, m.Installer.ValueString())
		markDrift(m, u)
	}
	diags.Append(dropUnrecorded(ctx, m, st)...)
	diags.Append(state.Set(ctx, m)...)
}

// script writes, in k's directory, `<name>.sh` running ref's installer for this machine
// (InstallerSource: its index entry for this platform, checked against the release's keys, kept
// in the provider's cache by digest).
func (run *k8sRun) script(ctx context.Context, k installer.K8s, ref string) error {
	n, _, err := installer.ParseInstaller(ref)
	if err != nil {
		return err
	}
	if n != k.Name {
		return fmt.Errorf("installer %s is of the project %q, not %q", ref, n, k.Name)
	}
	// The program apart from k.Dir, in the cache's runs/: one directory per working directory, as
	// two installers of a run (an upgrade's) have the same name.
	progs := filepath.Join(run.progs, filepath.Base(k.Dir))
	if err := os.MkdirAll(progs, 0o700); err != nil {
		return err
	}
	p, err := run.r.data.source().Program(ctx, run.r.data, Want{Ref: ref, Configured: run.configured, Installed: run.installed}, progs)
	if err != nil {
		return err
	}
	run.keys[ref] = p.Keys
	return wrapper(k.Dir, k.Name, p.Path)
}

// installedTrust is the trust of the release installed, whose installer ref carries keys.
func installedTrust(ref string, keys map[string][]byte) *fetch.Trust {
	return &fetch.Trust{Name: "the keys of the release installed (" + ref + ")", Keys: keys}
}

// cacheKeep is how many installers the cache keeps, the last used: each is an installer binary
// (tens of MB).
const cacheKeep = 8

// touch refreshes dir's use time, which prune orders by.
func touch(dir string) {
	now := time.Now()
	_ = os.Chtimes(dir, now, now)
}

// prune removes the cache's entries (one directory per index digest) but keep and the
// cacheKeep-1 others last used.
func prune(dir, keep string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type used struct {
		name string
		at   time.Time
	}
	var all []used
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && e.IsDir() && len(e.Name()) == 64 && e.Name() != keep {
			all = append(all, used{e.Name(), fi.ModTime()})
		}
	}
	slices.SortFunc(all, func(a, b used) int { return b.at.Compare(a.at) })
	for _, u := range all[min(len(all), cacheKeep-1):] {
		_ = os.RemoveAll(filepath.Join(dir, u.name))
	}
	// The part files fetches cut off left in the entries kept (each platform's directory).
	now := time.Now()
	for _, u := range append(all[:min(len(all), cacheKeep-1)], used{name: keep}) {
		plats, _ := os.ReadDir(filepath.Join(dir, u.name))
		for _, p := range plats {
			if p.IsDir() {
				removeStaleParts(filepath.Join(dir, u.name, p.Name()), now)
			}
		}
	}
}

// writeFile writes b to dir/name through a temporary file renamed into place, 0600.
func writeFile(dir, name string, b []byte) bool { return writeFileMode(dir, name, b, 0o600) }

// writeFileMode is writeFile with the file's mode.
func writeFileMode(dir, name string, b []byte, mode os.FileMode) bool {
	f, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return false
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(f.Name(), mode) != nil || os.Rename(f.Name(), filepath.Join(dir, name)) != nil {
		_ = os.Remove(f.Name())
		return false
	}
	return true
}

// trusted refuses an installer ref the cluster records unless it is name's by digest, from the
// registry one of the configured installers names or the provider's mirror: it is fetched
// with the provider's pull account and run on this machine.
func (r *installResource) trusted(ref, name string, configured ...types.String) error {
	n, prefix, err := installerPrefix(ref)
	if err != nil {
		return err
	}
	if n != name {
		return fmt.Errorf("of the project %q, not %q", n, name)
	}
	var allowed []string
	for _, k := range configured {
		if known(k) && k.ValueString() != "" {
			if _, p, err := installerPrefix(k.ValueString()); err == nil {
				allowed = append(allowed, p)
			}
		}
	}
	if r.data != nil && r.data.Registry != "" {
		allowed = append(allowed, normPrefix(strings.TrimSuffix(r.data.Registry, "/")))
	}
	switch {
	case len(allowed) == 0:
		return fmt.Errorf("from %s, and no installer is configured to compare its registry with", prefix)
	case !slices.Contains(allowed, prefix):
		return fmt.Errorf("from %s, not the registry the configured installer comes from (%s)", prefix, strings.Join(allowed, ", "))
	}
	return nil
}

// installerPrefix is ref's project and registry prefix (what comes before /<name>-installer@,
// normalised: normPrefix).
func installerPrefix(ref string) (name, prefix string, err error) {
	name, _, err = installer.ParseInstaller(ref)
	if err != nil {
		return "", "", err
	}
	prefix, _, _ = strings.Cut(ref, "/"+name+"-installer@")
	return name, normPrefix(prefix), nil
}

// normPrefix is a registry prefix as Docker reads it: a first element that is no host (no `.` or
// `:`, not localhost) is Docker Hub's docker.io; index.docker.io is
// docker.io too; the host is case-insensitive.
func normPrefix(p string) string {
	host, rest, _ := strings.Cut(p, "/")
	if !strings.ContainsAny(host, ".:") && host != "localhost" && host == strings.ToLower(host) {
		return "docker.io/" + p
	}
	host = strings.ToLower(host)
	if host == "index.docker.io" {
		host = "docker.io"
	}
	if rest == "" {
		return host
	}
	return host + "/" + rest
}

// k8sTrust is st with the installers it records that are not trusted (r.trusted, against
// configured) left out, and the installed one of those with why (nil when it is trusted): a run
// would fetch them with the provider's pull account and run them here, and a rollback would go to
// them.
func (r *installResource) k8sTrust(st *installer.Status, name string, configured ...types.String) (*installer.Status, *untrustedRun) {
	c := *st
	var u *untrustedRun
	if st.Installed != nil && st.Installed.Installer != "" {
		if err := r.trusted(st.Installed.Installer, name, configured...); err != nil {
			in := *st.Installed
			u = &untrustedRun{ref: in.Installer, why: err}
			in.Installer = ""
			c.Installed = &in
		}
	}
	if st.Previous != nil && st.Previous.Installer != "" && r.trusted(st.Previous.Installer, name, configured...) != nil {
		prev := *st.Previous
		prev.Installer = ""
		c.Previous = &prev
	}
	return &c, u
}

// untrustedRun is an installer the release runs that the provider does not trust (ref), and why.
type untrustedRun struct {
	ref string
	why error
}

// driftPrefix begins the status of a release that records an installer the provider does not
// run (r.trusted): a plan of such a state is an error until it is fixed by hand.
const driftPrefix = "failed: the release records an installer the provider does not run: "

// driftAlso follows the drift's quoted installer with the status it hides, in a drifted status.
const driftAlso = " (and: "

// markDrift sets m's status to say the release runs u (nil: nothing), keeping the status it had
// (helm's, but installed) after it.
func markDrift(m *installModel, u *untrustedRun) {
	if u == nil {
		return
	}
	status := driftPrefix + strconv.Quote(u.ref)
	if was := m.Status.ValueString(); was != "" && was != "installed" && !strings.HasPrefix(was, driftPrefix) {
		status += driftAlso + was + ")"
	}
	m.Status = types.StringValue(status)
}

// driftRef is the installer a drifted status names, and whether m, a Kubernetes target's, is
// drifted: its release runs an installer the provider does not run.
func driftRef(m *installModel) (string, bool) {
	rest, ok := strings.CutPrefix(m.Status.ValueString(), driftPrefix)
	if m.Kubernetes == nil || !ok {
		return "", false
	}
	// The ref is quoted, so whatever it holds reads back whole; one that does not unquote is a
	// drift all the same, of a ref no installer is.
	q, err := strconv.QuotedPrefix(rest)
	if err != nil {
		return rest, true
	}
	ref, _ := strconv.Unquote(q)
	return ref, true
}

// drifted says whether m is drifted (driftRef).
func drifted(m *installModel) bool {
	_, ok := driftRef(m)
	return ok
}

// untrustedDiag is the diagnostic of a release recording an installer the provider does not run:
// what (k's release, ref and why, when), and the fix; an error, else a warning.
func untrustedDiag(diags *diag.Diagnostics, isErr bool, when string, k installer.K8s, ref string, why any, configured string) {
	detail := fmt.Sprintf("%sthe release %s records the installer %s (%v): the provider does not fetch or run it, nor run anything over it (its pull account would go to that installer, and it would run on this machine). %s",
		when, k8sID(k), ref, why, untrustedFix(k, configured))
	if isErr {
		diags.AddError("The release records another installer", detail)
	} else {
		diags.AddWarning("The release records another installer", detail)
	}
}

// untrustedFix is what to do of a release recording an installer the provider does not run.
func untrustedFix(k installer.K8s, configured string) string {
	return fmt.Sprintf("If it is this release's own reference from another registry (a mirror's, an alias), set installer to it, or the provider's registry to its registry. Else install the configured release over it by hand (%s's `k8s upgrade --namespace %s --release %s`), or remove it (`k8s uninstall`), then refresh; or `tofu state rm` this resource to forget it.", configured, k.Namespace, k.ReleaseName())
}

// k8sPrep is what an update of a Kubernetes install reads and decides before it changes
// anything: the apply's, and the plan's dry run's.
type k8sPrep struct {
	name          string
	run           *k8sRun
	cur, nextK    installer.K8s
	st            *installer.Status
	running, next *installer.Describe
	runningRef    string
	u             *update
	step          releaseStep
	rerun, up     installer.Change
	// backK and back, on a rollback with values: the installer of the release it
	// reaches, which gives the values again after it (helm's rollback takes back that revision's).
	backK installer.K8s
	back  *installer.Describe
}

// k8sPrepare reads the release and decides the update's step as the apply does, with every
// refusal it makes before it runs anything: nil on one (said in diags). The caller closes
// prep.run.
func (r *installResource) k8sPrepare(ctx context.Context, plan, state *installModel, secrets map[string]string, diags *diag.Diagnostics, dry bool) *k8sPrep {
	name := installName(plan)
	login := ""
	if !dry {
		if login = r.login(plan, state, diags); diags.HasError() {
			return nil
		}
	}
	run := r.k8sStart(ctx, plan, name, diags)
	if run == nil {
		return nil
	}
	run.k.RegistryLogin = login
	kept := false
	defer func() {
		if !kept {
			run.close()
		}
	}()
	statusRef := plan.Installer.ValueString()
	if known(state.Installer) && state.Installer.ValueString() != "" {
		statusRef = state.Installer.ValueString()
		run.configured = append(run.configured, statusRef)
	}
	cur, ok := run.in("run", diags)
	if !ok {
		return nil
	}
	if err := run.script(ctx, cur, statusRef); err != nil {
		addInstallerError(diags, "Fetching the installer", err)
		return nil
	}
	st, err := cur.StatusOf(ctx, imported(state))
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		return nil
	}
	switch {
	case st == nil:
		diags.AddError("No release to change", fmt.Sprintf("The namespace holds no release %s: an import names a release that exists; otherwise `tofu state rm` this resource and apply to install again.", k8sID(cur)))
		return nil
	case st.Name != name:
		diags.AddError("Another project's release", fmt.Sprintf("The release %s holds %q, not %q.", k8sID(cur), st.Name, name))
		return nil
	case st.Installing:
		helmBusy(diags, "helm is installing the release", cur)
		return nil
	case helmBusyOf(st) != "":
		helmBusy(diags, helmBusyOf(st), cur)
		return nil
	case st.Installed == nil:
		diags.AddError("Nothing runs", fmt.Sprintf("The release %s was uninstalled (helm keeps its history): `k8s rollback` brings it back, or `tofu state rm` this resource and apply to install again.", k8sID(cur)))
		return nil
	}
	// One the release records is the cluster's word: of this project, by digest, from the
	// configured installer's registry, or never fetched or run here.
	st, ut := r.k8sTrust(st, name, plan.Installer, state.Installer)
	if ut != nil {
		// Never fetched or run here, and nothing is run over it: refused before anything changes.
		untrustedDiag(diags, true, "", cur, ut.ref, ut.why, plan.Installer.ValueString())
		return nil
	}
	// The release records no installer (its values do not): state's, else the desired one, which
	// must then carry the release that runs.
	runningRef := st.Installed.Installer
	if runningRef == "" {
		runningRef = statusRef
	}
	running := (*installer.Describe)(nil)
	if runningRef != statusRef {
		cur, running = run.fetch(ctx, "run", runningRef, diags)
	} else {
		running, err = cur.Describe(ctx)
		if err != nil {
			addInstallerError(diags, "Reading the installer of the release that runs", err)
			return nil
		}
		cur = cur.Without(running)
	}
	if running == nil {
		return nil
	}
	if st.Installed.Installer == "" && !running.Release.Same(*st.Installed) {
		diags.AddAttributeError(path.Root("installer"), "the release names no installer",
			fmt.Sprintf("The release %s runs %s %s (revision %d) and records no installer; %s carries %s (revision %d): set installer to %s %s's, apply (the release is taken over), then change installer.",
				k8sID(cur), name, st.Installed.Version, st.Installed.Revision, runningRef, running.Release.Version, running.Release.Revision, name, st.Installed.Version))
		return nil
	}
	u := &update{r: r, plan: plan, state: state, secrets: secrets, diags: diags}
	if u.adopt(ctx, st); diags.HasError() {
		return nil
	}
	step, next, nextK := r.k8sReleaseStep(ctx, run, u, st, runningRef, running, dry)
	if diags.HasError() {
		return nil
	}
	rerun, up := u.split(ctx, running, next)
	refuseSecretInputs(diags, stringMap(ctx, plan.Inputs, diags), running, next)
	refuseRunEnvInputs(diags, []string{"KUBECONFIG"}, "", stringMap(ctx, plan.Inputs, diags), secrets)
	if diags.HasError() {
		return nil
	}
	switch {
	case step == rollback && imported(state):
		importedRollback(diags, path.Root("installer"), "installer", runningRef+", the release it runs", "previous.installer")
		return nil
	case step == rollback && !rerun.Empty():
		rollbackAlone(diags, "inputs or secret inputs")
		return nil
	case step == rollback && plan.Kubernetes.ImagePullLogin.ValueBool() && !state.Kubernetes.ImagePullLogin.ValueBool():
		rollbackAlone(diags, "image_pull_login") // helm's rollback writes no pull Secret
		return nil
	case step == rollback && valuesChanged(plan, state):
		rollbackAlone(diags, "values") // helm's rollback takes back the values it reaches
		return nil
	case step == rollback && loginChanged(plan, state):
		rollbackAlone(diags, "registry_login_wo_version") // helm's rollback writes no pull Secret
		return nil
	}
	p := &k8sPrep{name: name, run: run, cur: cur, nextK: nextK, st: st, running: running, next: next, runningRef: runningRef, u: u, step: step, rerun: rerun, up: up}
	if step == rollback && cur.Values != "" {
		// Fetched before anything changes: a rollback whose values cannot be given again is refused.
		if p.backK, p.back = run.fetch(ctx, "back", plan.Installer.ValueString(), diags); p.back == nil {
			return nil
		}
	}
	kept = true
	return p
}

// k8sUpdate applies a plan to a release in DESIGN § 13's order: the inputs first, by `k8s upgrade
// --given` of the running release's installer, state saved; then the release, by the new
// release's `k8s upgrade` or the running one's `k8s rollback`.
func (r *installResource) k8sUpdate(ctx context.Context, plan, state *installModel, secrets map[string]string, diags *diag.Diagnostics, save func(*installModel) bool) {
	p := r.k8sPrepare(ctx, plan, state, secrets, diags, false)
	if p == nil {
		return
	}
	defer p.run.close()
	name, runningRef, u, step := p.name, p.runningRef, p.u, p.step
	st, cur, running := p.st, p.cur, p.running // the release as read, and its installer
	next, nextK := p.next, p.nextK             // the new release, on an upgrade
	rerun, up := p.rerun, p.up                 // what the running release's re-run and the upgrade are given
	short := running.ShortName
	base := state // what the final read falls back to: the state saved last
	pullLogin := plan.Kubernetes.ImagePullLogin.ValueBool() && !state.Kubernetes.ImagePullLogin.ValueBool()
	if rerun.Empty() && step == stay && (pullLogin || valuesChanged(plan, state) || loginChanged(plan, state)) {
		// image_pull_login turned on or the chart values changed, no input: the release applied
		// again writes the Secret and gives helm the values.
		if _, err := cur.Reapply(ctx, r.registryCA()); err != nil {
			addInstallerError(diags, "Applying "+name+" again", err)
			return
		}
	}
	if !rerun.Empty() {
		if _, err := cur.Reconfigure(ctx, rerun, r.registryCA()); err != nil {
			addInstallerError(diags, "Reconfiguring "+name, err)
			return
		}
		if step != stay {
			partial := u.partial(ctx, rerun)
			if diags.HasError() {
				return
			}
			partial.Installer = types.StringValue(runningRef)
			k8sFill(partial, cur, short, r.registry(), r.providerCA(), state)
			// The release as the reconfiguration left it; the read before it, when this one fails.
			var after *untrustedRun
			now, err := cur.Status(ctx)
			if err == nil && now == nil {
				err = fmt.Errorf("the namespace holds no release %s any more", k8sID(cur))
			}
			if err == nil {
				st, after = r.k8sTrust(now, name, plan.Installer, state.Installer)
				if busy := helmBusyOf(st); busy != "" {
					err = fmt.Errorf("%s", busy)
				}
			}
			applyStatus(partial, st, false)
			k8sStatus(partial, st)
			if err != nil {
				// Not checked again: the upgrade is not run over it. State is the reconfiguration's.
				addInstallerError(diags, "Reading the release after its reconfiguration (the upgrade is not run)", err)
				save(partial)
				return
			}
			// Recording an installer not trusted now: saved so, and nothing more runs over it.
			stillUntrusted(diags, partial, cur, after, ", the reconfiguration (the upgrade to "+plan.Installer.ValueString()+" is not run)", plan.Installer.ValueString())
			if !save(partial) || diags.HasError() {
				return
			}
			base = partial
		}
	}
	after := cur
	switch step {
	case upgrade:
		if _, err := nextK.Upgrade(ctx, up, r.registryCA()); err != nil {
			addInstallerError(diags, "Upgrading "+name+" to "+next.Release.Version, err)
			return
		}
		short, after = next.ShortName, nextK
	case rollback:
		if _, err := cur.Rollback(ctx); err != nil {
			addInstallerError(diags, "Rolling "+name+" back", err)
			return
		}
		// helm's rollback takes back the values the release it reaches was given: values
		// given again by that release's installer, so the release runs what state records.
		if p.back != nil {
			short, after = p.back.ShortName, p.backK
			if _, err := p.backK.Reapply(ctx, r.registryCA()); err != nil {
				addInstallerError(diags, "Applying "+name+" again with values after its rollback", err)
				// Rolled back, the values not given again: state records none, so the next plan
				// gives them.
				final := *plan
				kb := *plan.Kubernetes
				kb.Values = types.StringNull()
				final.Kubernetes = &kb
				k8sFill(&final, cur, short, r.registry(), r.providerCA(), state)
				r.k8sFinal(ctx, after, &final, base, name, diags, plan.Installer, state.Installer)
				save(&final)
				return
			}
		}
	}
	final := *plan
	k8sFill(&final, cur, short, r.registry(), r.providerCA(), state)
	r.k8sFinal(ctx, after, &final, base, name, diags, plan.Installer, state.Installer)
	save(&final)
}

// k8sFinal fills m's values read from the release after a run (k's status): on a read that fails,
// base's, with the error; a release still running an installer not trusted (against configured)
// keeps the drift status, with an error.
func (r *installResource) k8sFinal(ctx context.Context, k installer.K8s, m, base *installModel, name string, diags *diag.Diagnostics, configured ...types.String) {
	st, err := k.StatusOf(ctx, true)
	if err == nil && st != nil {
		r.noteRecorded(st.Values)
	}
	if err == nil && st == nil {
		err = fmt.Errorf("the namespace holds no release %s any more", k8sID(k))
	}
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		keepComputed(m, base)
		if drifted(m) {
			// What this run left is not known: a stale drift would refuse the next plan for it.
			m.Status = types.StringValue("failed: not read after this apply (refresh to read it)")
		}
		return
	}
	st, u := r.k8sTrust(st, name, configured...)
	applyStatus(m, st, false)
	k8sStatus(m, st)
	stillUntrusted(diags, m, k, u, "", m.Installer.ValueString())
}

// stillUntrusted is the error of a run after which the release records an installer not trusted
// (u; nil: none), m's status kept as the drift.
func stillUntrusted(diags *diag.Diagnostics, m *installModel, k installer.K8s, u *untrustedRun, note, configured string) {
	if u == nil {
		return
	}
	markDrift(m, u)
	untrustedDiag(diags, true, "After this run"+note+" (something else installed it meanwhile, or the installer run records its own reference otherwise than it was configured), ", k, u.ref, u.why, configured)
}

// k8sFill sets what an update's state holds whatever the status: the ID, the short name, the
// registry (an imported state's is the provider's now), no write-only value.
func k8sFill(m *installModel, k installer.K8s, short, registry string, ca types.String, state *installModel) {
	m.ID = types.StringValue(k8sID(k))
	m.ShortName = types.StringValue(short)
	m.Registry, m.RegistryCADigest = state.Registry, state.RegistryCADigest
	if imported(state) || m.RegistryCADigest.IsUnknown() {
		m.RegistryCADigest = ca
	}
	if imported(state) {
		m.Registry = types.StringValue(registry)
	}
	m.SecretInputsWO, m.GivenWO, m.SecretsWO = types.MapNull(types.StringType), types.StringNull(), types.StringNull()
}

// k8sReleaseStep decides the release change, as the Docker target's releaseStep: none when the
// release runs the desired installer (or the plan keeps state's), rollback to previous with
// allow_rollback, else the desired installer's upgrade, fetched and described here.
func (r *installResource) k8sReleaseStep(ctx context.Context, run *k8sRun, u *update, st *installer.Status, runningRef string, running *installer.Describe, dry bool) (releaseStep, *installer.Describe, installer.K8s) {
	desired := u.plan.Installer.ValueString()
	switch {
	case runningRef == desired:
		return stay, nil, installer.K8s{}
	case u.plan.Installer.Equal(u.state.Installer) && !imported(u.state):
		return stay, nil, installer.K8s{}
	case st.Previous != nil && st.Previous.Installer == desired:
		if rollbackRefused(u.plan.AllowRollback) {
			u.diags.AddAttributeError(path.Root("allow_rollback"), "a rollback needs allow_rollback",
				fmt.Sprintf("installer is %s, the release the last upgrade replaced: reaching it is k8s rollback, which takes the previous release back. Set allow_rollback = true to roll back.", desired))
			return stay, nil, installer.K8s{}
		}
		return rollback, nil, installer.K8s{}
	}
	if dry && u.rerunCertain(ctx, running) {
		return stay, nil, installer.K8s{}
	}
	// The new release is checked against the keys the release installed carries (its installer,
	// fetched and read above), as the release's own upgrade --version checks it.
	run.installed = installedTrust(runningRef, run.keys[runningRef])
	k, next := run.fetch(ctx, "next", desired, u.diags)
	run.installed = nil
	if next == nil {
		return stay, nil, k
	}
	if next.Release.Same(*st.Installed) {
		if rec := st.Installed.Installer; rec != "" {
			u.diags.AddAttributeError(path.Root("installer"), "installer is another digest of the release installed",
				fmt.Sprintf("%s carries %s %s (revision %d), which the release runs from %s: set installer to %s.", desired, next.Name, next.Release.Version, next.Release.Revision, rec, rec))
		}
		return stay, nil, k
	}
	return upgrade, next, k
}

func (r *installResource) k8sDelete(ctx context.Context, m *installModel, diags *diag.Diagnostics) {
	if ref, ok := driftRef(m); ok {
		// Checked again: the provider's registry may trust it now.
		if err := r.trusted(ref, installName(m), m.Installer); err != nil {
			untrustedDiag(diags, true, "Not uninstalled: ", k8sDriver(m, installName(m)), ref, err, m.Installer.ValueString())
			return
		}
	}
	if !known(m.Installer) {
		diags.AddError("An imported release never applied",
			"The state names no installer to uninstall the release with: apply once (the configuration gives it), or `tofu state rm` this resource to forget it.")
		return
	}
	name := installName(m)
	run := r.k8sStart(ctx, m, name, diags)
	if run == nil {
		return
	}
	defer run.close()
	// The describe first: an installer without the machine interface is refused here, before
	// uninstall is given --json.
	k, desc := run.fetch(ctx, "run", m.Installer.ValueString(), diags)
	if desc == nil {
		return
	}
	_, err := k.Uninstall(ctx, m.DeleteData.ValueBool())
	var refused *installer.RefusedError
	if errors.As(err, &refused) && strings.HasPrefix(refused.Reason, fmt.Sprintf("no release %s in %s, and nothing ", k.ReleaseName(), k.Namespace)) &&
		!strings.Contains(refused.Reason, " but ") {
		return // gone already, and nothing left of it
	}
	if err != nil {
		addInstallerError(diags, "Uninstalling", err)
	}
}

// k8sImport reads stowage_kubernetes_install's import ID, `<namespace>/<release>`: the release;
// the configuration gives the cluster, the installer and the rest at the first apply.
func (r *installResource) k8sImport(ctx context.Context, id string, resp *resource.ImportStateResponse) {
	ns, release, ok := strings.Cut(id, "/")
	if !ok || !dnsLabel.MatchString(ns) || !dnsLabel.MatchString(release) || len(release) > 53 {
		resp.Diagnostics.AddError("Import ID", fmt.Sprintf("%q: want <namespace>/<release> (helm list --all-namespaces)", id))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("kubernetes"), &k8sModel{Namespace: types.StringValue(ns), Release: types.StringValue(release)})...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), ns+"/"+release)...)
	for a, v := range map[string]bool{"deletion_protection": true, "delete_data": false, "allow_rollback": false} {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(a), v)...)
	}
}
