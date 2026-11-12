// Package provider is the OpenTofu provider for Stowage: it drives a Stowage product's
// installer through its machine interface and decides nothing the installer does not decide.
package provider

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// Connection is how a resource's target is reached over SSH; Host empty is this machine.
type Connection struct {
	Host           string
	Port           int
	User           string
	KeyFile        string
	KnownHostsLine string
	// AlsoKnownHostsLines are other keys the host may serve (target.SSH's).
	AlsoKnownHostsLines []string
}

// RunnerFactory gives the Runner that reaches a target.
type RunnerFactory func(Connection) (target.Runner, error)

// DefaultRunner runs on this machine without a host, else through the system ssh.
func DefaultRunner(c Connection) (target.Runner, error) {
	if c.Host == "" {
		return target.Local{}, nil
	}
	s := target.SSH{Host: c.Host, Port: c.Port, User: c.User, KeyFile: c.KeyFile, KnownHostsLine: c.KnownHostsLine, AlsoKnownHostsLines: c.AlsoKnownHostsLines}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Data is what the provider's configuration gives its resources.
type Data struct {
	Username   string
	Password   string
	Registry   string
	RegistryCA string
	// RegistryUsername and RegistryPassword: the mirror's own login ("" for none).
	RegistryUsername, RegistryPassword string
	// RegistryKnown is false while the configuration's registry is not known yet (it depends
	// on another resource): the plan-time comparison with the install's is then skipped.
	RegistryKnown bool
	// RegistryCAKnown is the same for registry_ca.
	RegistryCAKnown bool
	// UsernameUnknown is true while the pull account's username is not known yet.
	UsernameUnknown bool
	NewRunner       RunnerFactory
	// Kubernetes is the provider's kubernetes block, nil without one: the cluster every
	// kubernetes target reaches.
	Kubernetes *k8sConnModel
	// Poll is how often a create reads an install still installing.
	Poll time.Duration
	// TempDir holds each Kubernetes run's private directory; "" is the system's.
	TempDir string
	// CacheDir keeps each installer's bootstrap script by digest, for the Kubernetes runs; ""
	// keeps none.
	CacheDir string
}

// fetchLogin is the login an installer image's fetch from host runs with: the mirror's own when
// host is the mirror's, else the provider's pull account (the installer registry's); nil without.
func (d *Data) fetchLogin(host string) *installer.Login {
	if m := d.mirrorLogin(); m != nil && strings.EqualFold(m.Host, host) {
		return m
	}
	if d == nil || d.Username == "" {
		return nil
	}
	return &installer.Login{Host: host, Username: d.Username, Password: d.Password}
}

// mirrorLogin is the mirror's own login for the host of the provider's registry; nil without one.
func (d *Data) mirrorLogin() *installer.Login {
	if d == nil || d.RegistryUsername == "" || d.Registry == "" {
		return nil
	}
	return &installer.Login{Host: registryHostOf(d.Registry), Username: d.RegistryUsername, Password: d.RegistryPassword}
}

// defaultPoll is how often a create reads an install still installing.
const defaultPoll = 10 * time.Second

type stowageProvider struct {
	version   string
	newRunner RunnerFactory
	poll      time.Duration
	tmp       string
	cache     string
}

type providerModel struct {
	Username   types.String `tfsdk:"username"`
	Password   types.String `tfsdk:"password"`
	Registry   types.String `tfsdk:"registry"`
	RegistryCA types.String `tfsdk:"registry_ca"`
	// RegistryUsername and RegistryPassword are the mirror's (registry's) own login.
	RegistryUsername types.String  `tfsdk:"registry_username"`
	RegistryPassword types.String  `tfsdk:"registry_password"`
	Kubernetes       *k8sConnModel `tfsdk:"kubernetes"`
}

// New returns the provider's constructor, reaching targets with DefaultRunner.
func New(version string) func() provider.Provider {
	return NewWithRunner(version, DefaultRunner)
}

// NewWithRunner returns the provider's constructor, reaching targets with f.
func NewWithRunner(version string, f RunnerFactory) func() provider.Provider {
	return newWithPoll(version, f, defaultPoll)
}

func newWithPoll(version string, f RunnerFactory, poll time.Duration) func() provider.Provider {
	return newIn(version, f, poll, "", defaultCache())
}

// defaultCache is the provider's cache of bootstrap scripts: the user's cache directory's
// terraform-provider-stowage/installers; "" (none) without one.
func defaultCache() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "terraform-provider-stowage", "installers")
}

// newIn is newWithPoll whose Kubernetes runs make their private directories in tmp and keep the
// bootstrap scripts in cache.
func newIn(version string, f RunnerFactory, poll time.Duration, tmp, cache string) func() provider.Provider {
	return func() provider.Provider {
		return &stowageProvider{version: version, newRunner: f, poll: poll, tmp: tmp, cache: cache}
	}
}

func (p *stowageProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "stowage"
	resp.Version = p.version
}

func (p *stowageProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Drives a Stowage product's installer through its machine interface (`describe --json`, `status --json`, the verbs' `--json` result and exit status).",
		Attributes: map[string]schema.Attribute{
			"username": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The registry's pull account, with `password`: the target logs in with it, in a Docker configuration of its own removed afterwards, to fetch the installer image. Left out, the target's own `docker login` (or none) fetches it.",
			},
			"password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "The pull account's password or token, with `username`. Sent to the target on standard input only.",
			},
			"registry": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "A mirror's prefix the install pulls from instead of the project's registry (the installer's `--registry`). First-install only: a later change is a plan-time error.",
			},
			"registry_ca": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "PEM of the certificate authority the mirror's certificate chains to (the installer's `--registry-ca`), sent on standard input.",
			},
			"registry_username": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "The mirror's own login, with `registry_password`, for the host of `registry` alone: every run on the machine running OpenTofu or a Docker target that reads the mirror (a Docker target's installer, the Kubernetes runs, `stowage_vm_seed`'s) gets it in a Docker configuration of its own, removed afterwards, the password on standard input. Needs `registry`. Left out, the mirror is read with the machine's own Docker login (`username` is the installer registry's, never sent to another host). A VM reads the mirror by itself, with what its seed gave it.",
			},
			"registry_password": schema.StringAttribute{
				Optional:            true,
				Sensitive:           true,
				MarkdownDescription: "The mirror's password or token, with `registry_username`. Sent on standard input only.",
			},
		},
		Blocks: map[string]schema.Block{"kubernetes": k8sConnBlock()},
	}
}

func (p *stowageProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var m providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !m.Username.IsUnknown() && !m.Password.IsUnknown() && (m.Username.ValueString() == "") != (m.Password.ValueString() == "") {
		resp.Diagnostics.AddAttributeError(path.Root("username"), "username and password go together",
			"Give both the pull account's username and its password, or neither.")
		return
	}
	if !m.RegistryUsername.IsUnknown() && !m.RegistryPassword.IsUnknown() && (m.RegistryUsername.ValueString() == "") != (m.RegistryPassword.ValueString() == "") {
		resp.Diagnostics.AddAttributeError(path.Root("registry_username"), "registry_username and registry_password go together",
			"Give both the mirror's username and its password, or neither.")
		return
	}
	if m.RegistryUsername.ValueString() != "" && !m.Registry.IsUnknown() && m.Registry.ValueString() == "" {
		resp.Diagnostics.AddAttributeError(path.Root("registry_username"), "registry_username needs registry",
			"The mirror's login is for the host of the provider's registry: give registry, or leave the login out.")
		return
	}
	if ca := m.RegistryCA.ValueString(); ca != "" && !strings.Contains(ca, "-----BEGIN CERTIFICATE-----") {
		resp.Diagnostics.AddAttributeError(path.Root("registry_ca"), "registry_ca is not PEM",
			"Give the certificate authority's PEM (-----BEGIN CERTIFICATE-----), e.g. file(\"mirror-ca.pem\").")
		return
	}
	if m.Kubernetes != nil {
		validateK8sConn(m.Kubernetes, &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	d := &Data{
		Kubernetes:       m.Kubernetes,
		Username:         m.Username.ValueString(),
		Password:         m.Password.ValueString(),
		Registry:         strings.TrimSuffix(m.Registry.ValueString(), "/"), // as the installer records it
		RegistryCA:       m.RegistryCA.ValueString(),
		RegistryUsername: m.RegistryUsername.ValueString(),
		RegistryPassword: m.RegistryPassword.ValueString(),
		RegistryKnown:    !m.Registry.IsUnknown(),
		RegistryCAKnown:  !m.RegistryCA.IsUnknown(),
		UsernameUnknown:  m.Username.IsUnknown(),
		NewRunner:        p.newRunner,
		Poll:             p.poll,
		TempDir:          p.tmp,
		CacheDir:         p.cache,
	}
	resp.ResourceData = d
	resp.DataSourceData = d
}

func (p *stowageProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{newInstallResource, newSeedResource}
}

func (p *stowageProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{newInstallDataSource}
}
