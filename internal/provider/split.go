package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The three install resource types, one per shape, are views of one model: installSchema, whose
// docker, kubernetes and vm blocks name the target. Each type's schema is that model's attributes
// it takes, the block's flattened to the top; shapeResource converts each value, request and
// diagnostic path between the type's schema and the model's, and runs installResource on the
// model. Invariant: every attribute of a type maps to exactly one of the model's, and back.

// field is one attribute of a type: its name there, and its path in the model (an attribute, or
// one of the kind's block).
type field struct {
	outer string
	inner []string
	// desc replaces the model's description, when set.
	desc string
	// required makes the attribute required in the type's schema (the model checks it too).
	required bool
}

// shape is a resource type's view of the model.
type shape struct {
	kind   string // the model's block: docker, kubernetes, vm
	fields []field
	desc   string
	// once makes m, the schemas and value types every request converts with, at the first.
	once sync.Once
	m    model
}

// common are the model's attributes every type takes, by the same name.
var common = []string{"id", "inputs", "secret_inputs_wo", "secret_inputs_wo_version", "given_wo", "allow_rollback",
	"deletion_protection", "plan_dry_run", "registry", "registry_ca_digest", "status", "previous", "revision",
	"registry_login_wo", "registry_login_wo_version", "kit_wo", "kit_code_wo", "kit_wo_version", "secret_rotations", "timeouts"}

// backupFields are the model's attributes of a machine made again (backup.go): Docker's and the VM's.
var backupFields = []string{"backup", "backup_rclone_config_wo", "backup_rclone_config_wo_version", "restore", "restore_identity_wo"}

func top(names ...string) []field {
	out := make([]field, 0, len(names))
	for _, n := range names {
		out = append(out, field{outer: n, inner: []string{n}})
	}
	return out
}

func block(kind string, names ...string) []field {
	out := make([]field, 0, len(names))
	for _, n := range names {
		out = append(out, field{outer: n, inner: []string{kind, n}})
	}
	return out
}

var shapes = map[string]*shape{
	"docker": {
		kind: "docker",
		desc: "One install of a Stowage product on a Docker host, reached over SSH by the system `ssh` binary, or on the machine running OpenTofu (no `host`). " +
			"The installer runs on the host: `docker run --rm <installer> > <name>.sh`, then `sh <name>.sh --no-input --json`; the install keeps its installer as `<work_dir>/<name>/<short_name>-stow`, which every later run uses.",
		fields: append(append(top(append(append(common, backupFields...), "installer", "secrets_wo", "tls", "domains", "ports", "address", "delete_data",
			"acme_ca", "acme_ca_root", "short_name", "version", "channel")...),
			block("docker", "port", "user", "private_key_file", "known_hosts_line", "work_dir")...),
			field{outer: "host", inner: []string{"docker", "host"}, desc: "The Docker host, reached over SSH by the system `ssh` binary. Left out, the install is on the machine running OpenTofu (its own Docker)."}),
	},
	"kubernetes": {
		kind: "kubernetes",
		desc: "One install of a Stowage product as a helm release on the cluster the provider's `kubernetes` block reaches. " +
			"The installer runs on the machine running OpenTofu (its image's entry for that machine's platform, read from the registry, no Docker): `<name>.sh k8s install|upgrade|rollback|uninstall`, driving `helm` and `kubectl`.",
		fields: append(append(top(append(common, "installer", "delete_data", "short_name", "version", "channel")...),
			block("kubernetes", "release", "image_pull_login", "values")...),
			field{outer: "namespace", inner: []string{"kubernetes", "namespace"}, required: true, desc: "The release's namespace. First-install only."}),
	},
	"vm": {
		kind: "vm",
		desc: "The install of a Stowage product on a Kairos VM, reached over SSH: a VM its hypervisor's or cloud's resource made from `stowage_vm_seed`. " +
			"`sudo -n <short_name>-stow install|upgrade|rollback --json` and `<short_name>-stow status|describe --json` run on it; destroying the resource forgets the install, removing the VM is its own resource's job.",
		fields: append(append(top(append(append(common, backupFields...), "secrets_wo", "tls", "domains", "ports", "acme_ca", "acme_ca_root")...),
			block("vm", "port", "private_key_file", "known_hosts_line", "channel", "host_key_line", "seed_boot_id", "platform", "interfaces", "ssh_allow")...),
			field{outer: "host", inner: []string{"vm", "host"}, required: true, desc: "The VM's address, reached over SSH by the system `ssh` binary (the VM's own resource's address output)."},
			field{outer: "user", inner: []string{"vm", "user"}, required: true, desc: "The SSH user: the seed's `user`, in the VM's admin group (`sudo` without a password)."},
			field{outer: "short_name", inner: []string{"vm", "short_name"}, required: true, desc: "The installer's short name, `stowage_vm_seed`'s `short_name`: the VM runs `<short_name>-stow`."},
			field{outer: "version", inner: []string{"vm", "version"}, required: true, desc: "The release the VM runs (a create installs the one its OS carries: `stowage_vm_seed`'s `version`). A change upgrades the VM (`sudo -n <short_name>-stow upgrade --version V --yes --json`, then its reboot followed), or rolls it back to `previous.version` with `allow_rollback`. A refresh reads the release the VM runs."}),
	},
}

// typeName is the resource type of a shape: stowage_<kind>_install.
func (s *shape) typeName(provider string) string {
	if s.kind == "vm" {
		return provider + "_vm_install"
	}
	return provider + "_" + s.kind + "_install"
}

// schema is the type's schema: the model's attributes it takes, the block's at the top.
func (s *shape) schema(ctx context.Context) schema.Schema {
	inner := installSchema(ctx)
	out := schema.Schema{MarkdownDescription: s.desc, Attributes: map[string]schema.Attribute{}, Blocks: map[string]schema.Block{}}
	for _, f := range s.fields {
		if f.outer == "timeouts" {
			out.Blocks["timeouts"] = inner.Blocks["timeouts"]
			continue
		}
		a := innerAttribute(inner, f.inner)
		if a == nil {
			panic(fmt.Sprintf("%s: the model has no %s", s.kind, strings.Join(f.inner, ".")))
		}
		out.Attributes[f.outer] = adjust(a, f.desc, f.required)
	}
	if id, ok := out.Attributes["id"].(schema.StringAttribute); ok {
		id.MarkdownDescription = map[string]string{
			"docker":     "`<work_dir>/<name>`, the install's directory (`work_dir` left out: the SSH user's home, resolved at create).",
			"kubernetes": "`<namespace>/<release>`.",
			"vm":         "`<host>/<short_name>-stow`.",
		}[s.kind]
		out.Attributes["id"] = id
	}
	return out
}

// innerAttribute is the model's attribute at p: a top-level one, or one of a block's.
func innerAttribute(inner schema.Schema, p []string) schema.Attribute {
	if len(p) == 1 {
		return inner.Attributes[p[0]]
	}
	b, ok := inner.Blocks[p[0]].(schema.SingleNestedBlock)
	if !ok {
		return nil
	}
	return b.Attributes[p[1]]
}

// adjust is a with its description replaced (when desc is set), and required.
func adjust(a schema.Attribute, desc string, required bool) schema.Attribute {
	switch v := a.(type) {
	case schema.StringAttribute:
		if desc != "" {
			v.MarkdownDescription = desc
		}
		if required {
			v.Optional, v.Required = false, true
		}
		return v
	case schema.Int64Attribute:
		if desc != "" {
			v.MarkdownDescription = desc
		}
		return v
	case schema.BoolAttribute:
		if desc != "" {
			v.MarkdownDescription = desc
		}
		return v
	case schema.MapAttribute:
		if desc != "" {
			v.MarkdownDescription = desc
		}
		return v
	}
	return a
}

// find is the field mapping to the model's path p; nil without one.
func (s *shape) find(p ...string) *field {
	for i := range s.fields {
		f := &s.fields[i]
		if len(f.inner) == len(p) && f.inner[0] == p[0] && (len(p) == 1 || f.inner[1] == p[1]) {
			return f
		}
	}
	return nil
}

// toInner is v, a value of the type's schema (outerT), as a value of the model's (innerT): the
// kind's block always present, the other blocks null.
func (s *shape) toInner(v tftypes.Value, innerT tftypes.Object) (tftypes.Value, error) {
	if v.IsNull() {
		return tftypes.NewValue(innerT, nil), nil
	}
	if !v.IsKnown() {
		return tftypes.NewValue(innerT, tftypes.UnknownValue), nil
	}
	var outer map[string]tftypes.Value
	if err := v.As(&outer); err != nil {
		return tftypes.Value{}, err
	}
	vals := map[string]tftypes.Value{}
	for name, t := range innerT.AttributeTypes {
		switch {
		case name == s.kind:
			bt := t.(tftypes.Object)
			bv := map[string]tftypes.Value{}
			for an, at := range bt.AttributeTypes {
				if f := s.find(s.kind, an); f != nil {
					bv[an] = outer[f.outer]
				} else {
					bv[an] = tftypes.NewValue(at, nil)
				}
			}
			vals[name] = tftypes.NewValue(bt, bv)
		case shapes[name] != nil:
			vals[name] = tftypes.NewValue(t, nil)
		default:
			if f := s.find(name); f != nil {
				vals[name] = outer[f.outer]
			} else {
				vals[name] = tftypes.NewValue(t, nil)
			}
		}
	}
	return tftypes.NewValue(innerT, vals), nil
}

// toOuter is v, a value of the model's schema, as a value of the type's (outerT).
func (s *shape) toOuter(v tftypes.Value, outerT tftypes.Object) (tftypes.Value, error) {
	if v.IsNull() {
		return tftypes.NewValue(outerT, nil), nil
	}
	if !v.IsKnown() {
		return tftypes.NewValue(outerT, tftypes.UnknownValue), nil
	}
	var inner map[string]tftypes.Value
	if err := v.As(&inner); err != nil {
		return tftypes.Value{}, err
	}
	var blk map[string]tftypes.Value
	if b := inner[s.kind]; b.IsKnown() && !b.IsNull() {
		if err := b.As(&blk); err != nil {
			return tftypes.Value{}, err
		}
	}
	vals := map[string]tftypes.Value{}
	for _, f := range s.fields {
		t := outerT.AttributeTypes[f.outer]
		switch {
		case len(f.inner) == 1:
			vals[f.outer] = inner[f.inner[0]]
		case blk != nil:
			vals[f.outer] = blk[f.inner[1]]
		case !inner[s.kind].IsKnown():
			vals[f.outer] = tftypes.NewValue(t, tftypes.UnknownValue)
		default:
			vals[f.outer] = tftypes.NewValue(t, nil)
		}
	}
	return tftypes.NewValue(outerT, vals), nil
}

// outerPath is the type's path of p, a path of the model: empty when the type has no such
// attribute.
func (s *shape) outerPath(p path.Path) path.Path {
	steps := p.Steps()
	if len(steps) == 0 {
		return p
	}
	first, ok := steps[0].(path.PathStepAttributeName)
	if !ok {
		return path.Empty()
	}
	var f *field
	rest := steps[1:]
	if string(first) == s.kind {
		if len(steps) < 2 {
			return path.Empty()
		}
		second, ok := steps[1].(path.PathStepAttributeName)
		if !ok {
			return path.Empty()
		}
		f, rest = s.find(s.kind, string(second)), steps[2:]
	} else {
		f = s.find(string(first))
	}
	if f == nil {
		return path.Empty()
	}
	out := path.Root(f.outer)
	for _, st := range rest {
		switch v := st.(type) {
		case path.PathStepAttributeName:
			out = out.AtName(string(v))
		case path.PathStepElementKeyString:
			out = out.AtMapKey(string(v))
		case path.PathStepElementKeyInt:
			out = out.AtListIndex(int(v))
		case path.PathStepElementKeyValue:
			out = out.AtSetValue(v.Value)
		}
	}
	return out
}

// diags are the model's diagnostics with the type's paths.
func (s *shape) diags(in diag.Diagnostics) diag.Diagnostics {
	var out diag.Diagnostics
	for _, d := range in {
		wp, ok := d.(diag.DiagnosticWithPath)
		if !ok {
			out = append(out, d)
			continue
		}
		p := s.outerPath(wp.Path())
		switch {
		case len(p.Steps()) == 0 && d.Severity() == diag.SeverityError:
			out.AddError(d.Summary(), d.Detail())
		case len(p.Steps()) == 0:
			out.AddWarning(d.Summary(), d.Detail())
		case d.Severity() == diag.SeverityError:
			out.AddAttributeError(p, d.Summary(), d.Detail())
		default:
			out.AddAttributeWarning(p, d.Summary(), d.Detail())
		}
	}
	return out
}

// shapeResource is a resource type of one shape: installResource run on the model.
type shapeResource struct {
	shape *shape
	inner *installResource
}

var (
	_ resource.ResourceWithConfigure      = (*shapeResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*shapeResource)(nil)
	_ resource.ResourceWithValidateConfig = (*shapeResource)(nil)
	_ resource.ResourceWithImportState    = (*shapeResource)(nil)
)

func newShapeResource(kind string) func() resource.Resource {
	return func() resource.Resource { return &shapeResource{shape: shapes[kind], inner: &installResource{}} }
}

func (r *shapeResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = r.shape.typeName(req.ProviderTypeName)
}

func (r *shapeResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = r.shape.schema(ctx)
}

func (r *shapeResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.inner.Configure(ctx, req, resp)
}

// model holds the model's schema and value type, and converts.
type model struct {
	s      *shape
	schema schema.Schema
	innerT tftypes.Object
	outerT tftypes.Object
}

// model is the type's model, made once for the process (the framework makes a resource for each
// request, the shape is one): the schemas do not change.
func (r *shapeResource) model(ctx context.Context) model {
	s := r.shape
	s.once.Do(func() {
		in := installSchema(ctx)
		s.m = model{s: s, schema: in, innerT: in.Type().TerraformType(ctx).(tftypes.Object),
			outerT: s.schema(ctx).Type().TerraformType(ctx).(tftypes.Object)}
	})
	return s.m
}

// in is v as the model's value; an error in diags otherwise.
func (m model) in(v tftypes.Value, diags *diag.Diagnostics) tftypes.Value {
	out, err := m.s.toInner(v, m.innerT)
	if err != nil {
		diags.AddError("Converting to the install model", err.Error())
	}
	return out
}

// out is v, the model's value, as the type's; an error in diags otherwise.
func (m model) out(v tftypes.Value, diags *diag.Diagnostics) tftypes.Value {
	out, err := m.s.toOuter(v, m.outerT)
	if err != nil {
		diags.AddError("Converting from the install model", err.Error())
	}
	return out
}

func (m model) config(c tfsdk.Config, diags *diag.Diagnostics) tfsdk.Config {
	return tfsdk.Config{Raw: m.in(c.Raw, diags), Schema: m.schema}
}

func (m model) plan(p tfsdk.Plan, diags *diag.Diagnostics) tfsdk.Plan {
	return tfsdk.Plan{Raw: m.in(p.Raw, diags), Schema: m.schema}
}

func (m model) state(s tfsdk.State, diags *diag.Diagnostics) tfsdk.State {
	return tfsdk.State{Raw: m.in(s.Raw, diags), Schema: m.schema}
}

func (r *shapeResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	m := r.model(ctx)
	ireq := resource.ValidateConfigRequest{Config: m.config(req.Config, &resp.Diagnostics)}
	if resp.Diagnostics.HasError() {
		return
	}
	var iresp resource.ValidateConfigResponse
	r.inner.ValidateConfig(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
}

func (r *shapeResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	m := r.model(ctx)
	ireq := resource.ModifyPlanRequest{Config: m.config(req.Config, &resp.Diagnostics), Plan: m.plan(req.Plan, &resp.Diagnostics),
		State: m.state(req.State, &resp.Diagnostics), ProviderMeta: req.ProviderMeta, Private: req.Private, ClientCapabilities: req.ClientCapabilities}
	iresp := resource.ModifyPlanResponse{Plan: m.plan(resp.Plan, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	r.inner.ModifyPlan(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.Plan.Raw = m.out(iresp.Plan.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}

func (r *shapeResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	m := r.model(ctx)
	ireq := resource.CreateRequest{Config: m.config(req.Config, &resp.Diagnostics), Plan: m.plan(req.Plan, &resp.Diagnostics), ProviderMeta: req.ProviderMeta}
	iresp := resource.CreateResponse{State: m.state(resp.State, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	r.inner.Create(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.State.Raw = m.out(iresp.State.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}

func (r *shapeResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	m := r.model(ctx)
	ireq := resource.ReadRequest{State: m.state(req.State, &resp.Diagnostics), Private: req.Private, ProviderMeta: req.ProviderMeta, ClientCapabilities: req.ClientCapabilities}
	iresp := resource.ReadResponse{State: m.state(resp.State, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	r.inner.Read(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.State.Raw = m.out(iresp.State.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}

func (r *shapeResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	m := r.model(ctx)
	ireq := resource.UpdateRequest{Config: m.config(req.Config, &resp.Diagnostics), Plan: m.plan(req.Plan, &resp.Diagnostics),
		State: m.state(req.State, &resp.Diagnostics), ProviderMeta: req.ProviderMeta, Private: req.Private}
	iresp := resource.UpdateResponse{State: m.state(resp.State, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	r.inner.Update(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.State.Raw = m.out(iresp.State.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}

func (r *shapeResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	m := r.model(ctx)
	ireq := resource.DeleteRequest{State: m.state(req.State, &resp.Diagnostics), Private: req.Private, ProviderMeta: req.ProviderMeta}
	iresp := resource.DeleteResponse{State: m.state(resp.State, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	r.inner.Delete(ctx, ireq, &iresp)
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.State.Raw = m.out(iresp.State.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}

// ImportState reads the type's import ID: Docker's `[<host>:]<work_dir>/<name>/<short>-stow`,
// Kubernetes's `<namespace>/<release>`, the VM's `<host>/<short>-stow`.
func (r *shapeResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	m := r.model(ctx)
	iresp := resource.ImportStateResponse{State: m.state(resp.State, &resp.Diagnostics), Private: resp.Private}
	if resp.Diagnostics.HasError() {
		return
	}
	switch r.shape.kind {
	case "vm":
		r.inner.importVM(ctx, req.ID, &iresp)
	case "kubernetes":
		r.inner.k8sImport(ctx, req.ID, &iresp)
	default:
		r.inner.importDocker(ctx, req.ID, &iresp)
	}
	resp.Diagnostics.Append(r.shape.diags(iresp.Diagnostics)...)
	resp.State.Raw = m.out(iresp.State.Raw, &resp.Diagnostics)
	resp.Private = iresp.Private
}
