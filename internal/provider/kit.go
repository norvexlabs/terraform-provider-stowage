package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	pathpkg "path"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
)

// kitStash keeps the kits stowage_kit issued in this provider process, by the resource's id:
// the ephemeral stowage_kit hands one to what takes it in the same apply, and nothing else ever
// holds it (never state, a plan, a file of the root or a log). A later process finds none.
type kitStash struct {
	mu   sync.Mutex
	kits map[string]installer.IssuedKit
}

// put keeps k under a fresh random id, which it returns.
func (s *kitStash) put(k installer.IssuedKit) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.kits == nil {
		s.kits = map[string]installer.IssuedKit{}
	}
	s.kits[id] = k
	return id, nil
}

// get is the kit kept under id; false when this process holds none.
func (s *kitStash) get(id string) (installer.IssuedKit, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.kits[id]
	return k, ok
}

// forget drops the kit kept under id.
func (s *kitStash) forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.kits, id)
}

// The install a kit resource runs on: one of three blocks, each holding what the install
// resource of that shape exports.

type kitDockerModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	User           types.String `tfsdk:"user"`
	PrivateKeyFile types.String `tfsdk:"private_key_file"`
	KnownHostsLine types.String `tfsdk:"known_hosts_line"`
	InstallID      types.String `tfsdk:"install_id"`
	ShortName      types.String `tfsdk:"short_name"`
}

type kitK8sModel struct {
	Installer types.String `tfsdk:"installer"`
	Namespace types.String `tfsdk:"namespace"`
	Release   types.String `tfsdk:"release"`
}

type kitVMModel struct {
	Host           types.String `tfsdk:"host"`
	Port           types.Int64  `tfsdk:"port"`
	User           types.String `tfsdk:"user"`
	PrivateKeyFile types.String `tfsdk:"private_key_file"`
	KnownHostsLine types.String `tfsdk:"known_hosts_line"`
	ShortName      types.String `tfsdk:"short_name"`
}

// kitTarget is the install a kit resource names: one block of three.
type kitTarget struct {
	Docker     *kitDockerModel
	Kubernetes *kitK8sModel
	VM         *kitVMModel
}

// kitTargetBlocks are the three blocks naming the install, which role ("issues the kit", "takes
// the kit") describes. What names the install (its id, short name, namespace and release)
// replaces the resource, as another install is another kit; how it is reached (the SSH host,
// port, user, key and known host line, the Kubernetes installer) changes in place, running
// nothing, as the install resources take a new host for the same install.
func kitTargetBlocks(role string) map[string]schema.Block {
	replace := []planmodifier.String{stringplanmodifier.RequiresReplace()}
	conn := func(desc string) schema.StringAttribute {
		return schema.StringAttribute{Optional: true, MarkdownDescription: desc}
	}
	// Attributes in a block are optional in the schema (the framework requires one of an absent
	// block too); validateKitTarget requires them, in the block given.
	str := func(required bool, desc string) schema.StringAttribute {
		if required {
			desc += " Required."
		}
		return schema.StringAttribute{Optional: true, MarkdownDescription: desc, PlanModifiers: replace}
	}
	ssh := func(hostDesc string, hostRequired bool) map[string]schema.Attribute {
		a := map[string]schema.Attribute{}
		for name, attr := range sshAttributes(hostDesc) {
			a[name] = attr
		}
		if hostRequired {
			h := a["host"].(schema.StringAttribute)
			h.MarkdownDescription += " Required."
			a["host"] = h
		}
		return a
	}
	docker := ssh("The Docker host, reached over SSH as the install's own `host`; left out, the machine running OpenTofu.", false)
	docker["install_id"] = str(true, "The install's `id`, `<work_dir>/<name>` (`stowage_docker_install`'s `id`): its kept installer `<id>/<short_name>-stow` runs. One that is not an absolute, clean path is refused at validation (`install_id is not an install's id`); a directory holding no install is an apply error (`No install there`).")
	docker["short_name"] = str(true, "The installer's short name (`stowage_docker_install`'s `short_name`).")
	vm := ssh("The VM's address (`stowage_vm_install`'s `host`).", true)
	vm["user"] = conn("The SSH user, in the VM's admin group (`sudo` without a password). Required.")
	vm["known_hosts_line"] = conn("The VM's own `known_hosts` line: `stowage_vm_install`'s `host_key_line`. That key alone is accepted. Required.")
	vm["short_name"] = str(true, "The installer's short name: the VM runs `sudo -n <short_name>-stow`.")
	return map[string]schema.Block{
		"docker": schema.SingleNestedBlock{
			MarkdownDescription: "The Docker install that " + role + ": its kept installer runs on its host, as the install resource's runs do.",
			Attributes:          docker,
		},
		"kubernetes": schema.SingleNestedBlock{
			MarkdownDescription: "The Kubernetes release that " + role + ", on the cluster the provider's `kubernetes` block reaches (another cluster: a provider alias). The release's installer runs on the machine running OpenTofu (`<name>.sh k8s kit …`, `k8s upgrade --kit`).",
			Attributes: map[string]schema.Attribute{
				"installer": conn("The installer the release runs (`stowage_kubernetes_install`'s `installer`): fetched and checked as the install resource's are; a release running another is refused. Required."),
				"namespace": str(true, "The release's namespace."),
				"release":   str(false, "The helm release's name; left out, the project's name, as the install resource's."),
			},
		},
		"vm": schema.SingleNestedBlock{
			MarkdownDescription: "The VM install that " + role + ", reached over SSH: `sudo -n <short_name>-stow` runs on it.",
			Attributes:          vm,
		},
	}
}

// validateKitTarget refuses anything but exactly one block, and checks its SSH settings.
func validateKitTarget(t kitTarget, diags *diag.Diagnostics) {
	n := 0
	for _, set := range []bool{t.Docker != nil, t.Kubernetes != nil, t.VM != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		diags.AddError("One install, in one block", "Give exactly one of the docker, kubernetes and vm blocks: the install the kit is issued by, taken by or checked on.")
		return
	}
	need := func(p path.Path, v types.String) {
		if v.IsNull() {
			diags.AddAttributeError(p, "Missing "+p.String(), "Give "+p.String()+": the install's own, as its install resource exports it.")
		}
	}
	switch {
	case t.Docker != nil:
		d := t.Docker
		need(path.Root("docker").AtName("install_id"), d.InstallID)
		need(path.Root("docker").AtName("short_name"), d.ShortName)
		validateSSH(path.Root("docker"), d.Host, d.User, d.KnownHostsLine, d.PrivateKeyFile, d.Port, true, diags)
		if id := d.InstallID.ValueString(); known(d.InstallID) && (!strings.HasPrefix(id, "/") || pathpkg.Clean(id) != id || pathpkg.Dir(id) == id) {
			diags.AddAttributeError(path.Root("docker").AtName("install_id"), "install_id is not an install's id",
				fmt.Sprintf("%q: give the install resource's id, `<work_dir>/<name>`, an absolute, clean path (stowage_docker_install's id).", id))
		}
		checkShortName(path.Root("docker").AtName("short_name"), d.ShortName, diags)
	case t.VM != nil:
		v := t.VM
		need(path.Root("vm").AtName("host"), v.Host)
		need(path.Root("vm").AtName("user"), v.User)
		need(path.Root("vm").AtName("known_hosts_line"), v.KnownHostsLine)
		need(path.Root("vm").AtName("short_name"), v.ShortName)
		validateSSH(path.Root("vm"), v.Host, v.User, v.KnownHostsLine, v.PrivateKeyFile, v.Port, false, diags)
		checkShortName(path.Root("vm").AtName("short_name"), v.ShortName, diags)
	case t.Kubernetes != nil:
		need(path.Root("kubernetes").AtName("installer"), t.Kubernetes.Installer)
		need(path.Root("kubernetes").AtName("namespace"), t.Kubernetes.Namespace)
		if ref := t.Kubernetes.Installer.ValueString(); known(t.Kubernetes.Installer) {
			if _, _, err := parseInstaller(ref); err != nil {
				diags.AddAttributeError(path.Root("kubernetes").AtName("installer"), "installer is not an installer image by digest", err.Error())
			}
		}
	}
}

// checkShortName refuses a short name no installer has.
func checkShortName(p path.Path, v types.String, diags *diag.Diagnostics) {
	if known(v) && !installer.ValidShortName(v.ValueString()) {
		diags.AddAttributeError(p, "short_name is not an installer's short name", fmt.Sprintf("%q: give the install resource's short_name.", v.ValueString()))
	}
}

// kitOps are the runs a kit resource makes on its install. A root orders them: after the install
// resource (its attributes in the block) and after one another on one install (depends_on). On
// Docker and the VM the installer's own lock refuses a run overlapping another (exit 3, "already
// running … wait for it to finish"); on Kubernetes helm refuses an upgrade over one pending, and
// a kit's Job runs beside anything.
type kitOps struct {
	// describe is the release the install runs.
	describe func(context.Context) (*installer.Describe, error)
	create   func(context.Context, string, installer.KitRequest) (*installer.IssuedKit, error)
	check    func(context.Context, string, string) (installer.KitCheck, error)
	take     func(context.Context, string, string) (*installer.Result, error)
	// status is the install's status: nil when there is none, which absent says.
	status func(context.Context) (*installer.Status, error)
	absent func(*diag.Diagnostics)
	close  func()
}

// kitOn connects to the install t names, through the install resources' own connection code
// (docker, vmWithin, k8sStart): nil, with the error in diags, otherwise. The caller closes it.
func (r *installResource) kitOn(ctx context.Context, t kitTarget, diags *diag.Diagnostics) *kitOps {
	switch {
	case t.Docker != nil:
		b := t.Docker
		id := b.InstallID.ValueString()
		m := &installModel{ID: b.InstallID, Docker: &dockerModel{Host: b.Host, Port: b.Port, User: b.User, PrivateKeyFile: b.PrivateKeyFile,
			KnownHostsLine: b.KnownHostsLine, WorkDir: types.StringValue(pathpkg.Dir(id))}}
		d, err := r.docker(m, b.ShortName.ValueString())
		if err != nil {
			diags.AddError("Cannot reach the Docker install", err.Error())
			return nil
		}
		return &kitOps{
			describe: d.KeptDescribe,
			create:   d.KitCreate,
			check:    d.KitCheck,
			take:     d.TakeKit,
			status:   d.Status,
			absent: func(diags *diag.Diagnostics) {
				diags.AddAttributeError(path.Root("docker").AtName("install_id"), "No install there",
					"The directory install_id names holds no install: give the install resource's id (stowage_docker_install's id).")
			},
			close: func() {},
		}
	case t.VM != nil:
		b := t.VM
		m := &installModel{VM: &vmModel{Host: b.Host, Port: b.Port, User: b.User, PrivateKeyFile: b.PrivateKeyFile,
			KnownHostsLine: b.KnownHostsLine, ShortName: b.ShortName}}
		v, err := r.vm(m)
		if err != nil {
			diags.AddError("Cannot reach the VM", err.Error())
			return nil
		}
		return &kitOps{
			describe: v.Describe,
			create:   v.KitCreate,
			check:    v.KitCheck,
			take:     v.TakeKit,
			status: func(ctx context.Context) (*installer.Status, error) {
				st, err := v.Status(ctx)
				if st == nil {
					return nil, err
				}
				return &st.Status, err
			},
			absent: func(diags *diag.Diagnostics) {
				diags.AddAttributeError(path.Root("vm").AtName("host"), "No install there",
					"The VM answers no status: give the host of the VM stowage_vm_install installed.")
			},
			close: func() {},
		}
	case t.Kubernetes != nil:
		return r.kitOnK8s(ctx, t.Kubernetes, diags)
	}
	diags.AddError("No install", "Give one of the docker, kubernetes and vm blocks.")
	return nil
}

// kitOnK8s fetches the installer b names on this machine and checks the release runs it: a kit's
// runs use the installer of the release it runs, as `k8s upgrade` by another would upgrade it.
func (r *installResource) kitOnK8s(ctx context.Context, b *kitK8sModel, diags *diag.Diagnostics) *kitOps {
	ref := b.Installer.ValueString()
	name, _, err := parseInstaller(ref)
	if err != nil {
		diags.AddAttributeError(path.Root("kubernetes").AtName("installer"), "installer is not an installer image by digest", err.Error())
		return nil
	}
	m := &installModel{Installer: b.Installer, Kubernetes: &k8sModel{Namespace: b.Namespace, Release: b.Release, ImagePullLogin: types.BoolValue(false), Values: types.StringNull()}}
	run := r.k8sStart(ctx, m, name, diags)
	if run == nil {
		return nil
	}
	k, desc := run.fetch(ctx, "run", ref, diags)
	if desc == nil {
		run.close()
		return nil
	}
	st, err := k.StatusOf(ctx, false)
	if err != nil {
		addInstallerError(diags, "Reading the release", err)
		run.close()
		return nil
	}
	if st == nil {
		diags.AddAttributeError(path.Root("kubernetes").AtName("namespace"), "No release there",
			fmt.Sprintf("The namespace holds no release %s: give the install resource's namespace and release.", k8sID(k)))
		run.close()
		return nil
	}
	if why := runsOther(st, ref, desc); why != "" {
		diags.AddAttributeError(path.Root("kubernetes").AtName("installer"), "The release runs another installer",
			fmt.Sprintf("%s: a kit's runs use the installer of the release it runs. Give that installer (stowage_kubernetes_install's installer, once applied).", why))
		run.close()
		return nil
	}
	ca := r.registryCA()
	return &kitOps{
		describe: func(context.Context) (*installer.Describe, error) { return desc, nil },
		create: func(ctx context.Context, h string, req installer.KitRequest) (*installer.IssuedKit, error) {
			return k.KitCreate(ctx, h, req, ca)
		},
		check: func(ctx context.Context, h, i string) (installer.KitCheck, error) { return k.KitCheck(ctx, h, i, ca) },
		take: func(ctx context.Context, kit, code string) (*installer.Result, error) {
			return k.TakeKit(ctx, kit, code, ca)
		},
		// Read again just before the take: the release may have moved since (an upgrade of this
		// apply).
		status: func(ctx context.Context) (*installer.Status, error) {
			st, err := k.StatusOf(ctx, false)
			if err != nil {
				return nil, err
			}
			if st == nil {
				return nil, fmt.Errorf("the namespace holds no release %s any more", k8sID(k))
			}
			if why := runsOther(st, ref, desc); why != "" {
				return nil, fmt.Errorf("%s: a kit's runs use the installer of the release it runs; give that installer (stowage_kubernetes_install's installer, once applied)", why)
			}
			return st, nil
		},
		absent: func(diags *diag.Diagnostics) {
			diags.AddAttributeError(path.Root("kubernetes").AtName("namespace"), "No release there",
				fmt.Sprintf("The namespace holds no release %s: give the install resource's namespace and release.", k8sID(k)))
		},
		close: run.close,
	}
}

// runsOther is why the release st reports does not run ref (whose describe is desc); "" when it
// does (the installer it records, or, recording none, the release's version and revision), and
// when nothing is installed (the take's installedThere refuses that; the installer the rest). st
// is not nil: its callers refuse a release that is not there.
func runsOther(st *installer.Status, ref string, desc *installer.Describe) string {
	switch {
	case st.Installed == nil:
		return "" // nothing installed: the take's installedThere says so, the installer refuses the rest
	case st.Installed.Installer != "" && st.Installed.Installer != ref:
		return fmt.Sprintf("the release runs %s %s from %s, not %s", st.Name, st.Installed.Version, st.Installed.Installer, ref)
	case st.Installed.Installer == "" && !st.Installed.Same(desc.Release):
		return fmt.Sprintf("the release runs %s %s revision %d, not %s's %s revision %d", st.Name, st.Installed.Version, st.Installed.Revision, ref, desc.Release.Version, desc.Release.Revision)
	}
	return ""
}

// describeThere is the release the install runs: nil, with the error in diags, when it cannot be
// read, or when a Docker install_id's directory holds no install (KeptDescribe's nil).
func describeThere(ctx context.Context, ops *kitOps, diags *diag.Diagnostics) *installer.Describe {
	desc, err := ops.describe(ctx)
	switch {
	case err != nil:
		addInstallerError(diags, "Reading the install's release", err)
		return nil
	case desc == nil:
		ops.absent(diags)
	}
	return desc
}

// installedThere says whether the install ops reaches holds a release installed: a take is an
// install's re-run, never its first install (a VM answers from its seed's installer before it, a
// Docker directory once its first install began), which its resource's kit_wo is.
func installedThere(ctx context.Context, ops *kitOps, diags *diag.Diagnostics) bool {
	st, err := ops.status(ctx)
	switch {
	case err != nil:
		addInstallerError(diags, "Reading the install", err)
		return false
	case st == nil:
		ops.absent(diags)
		return false
	case st.Installed == nil:
		diags.AddError("Nothing installed there",
			"The install's first install has not finished (status --json installed is null): a kit taken now would run its first install with defaults. Order the take after the install resource (its attributes in the block), or give a first install the kit by the install resource's kit_wo.")
		return false
	}
	return true
}
