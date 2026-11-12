package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// K8sScript runs a program in a directory of the machine running OpenTofu, as RunScript does, with
// KUBECONFIG set to the files named when any are (the kubeconfig the provider wrote for the run,
// the one the configuration names, or the environment's made absolute), else unset. The runner
// leaves the input keys and HelmEnv out of its environment before it starts (EnvFilter): the
// installer reads an input from the environment before an answers file, and helm's HELM_KUBE*
// would reach another cluster than the kubeconfig's. With a login user, the tar holds
// `login-password` too (never named as a file flag), and the program runs with a Docker
// configuration of its own holding that login (LoginScript): the one `--image-pull-login` writes
// the cluster's pull Secret from. Arguments: the directory, the kubeconfig ("" leaves none), the
// file names (space-separated, may be empty), the login's registry host and user ("" for none),
// then the program and its arguments. Its own variables are named __stowage_*: an exported
// variable of another name passes to the program unchanged.
var K8sScript = runScript(true, true)

// ToolsScript says which of the programs a Kubernetes run needs on the machine running OpenTofu
// are missing from PATH: one line each on standard output, exit 0 either way.
const ToolsScript = `for t in docker helm kubectl; do command -v "$t" >/dev/null 2>&1 || echo "$t"; done`

// HelmEnv are the environment variables helm reads its cluster and namespace from, over the
// kubeconfig: unset for every Kubernetes run, so helm and kubectl reach the same cluster.
// HELM_DRIVER chooses where helm keeps its releases (another store would hold none).
var HelmEnv = []string{"HELM_DRIVER", "HELM_DRIVER_SQL_CONNECTION_STRING", "HELM_KUBEAPISERVER", "HELM_KUBEASGROUPS", "HELM_KUBEASUSER",
	"HELM_KUBECAFILE", "HELM_KUBECONTEXT", "HELM_KUBEINSECURE_SKIP_TLS_VERIFY", "HELM_KUBETLS_SERVER_NAME", "HELM_KUBETOKEN", "HELM_NAMESPACE"}

// RunEnv is what the run itself needs from the environment, never unset for an input of the same
// name: the programs' search path, the home, temporary and cache directories, Docker's and the
// proxies' settings, the locale.
func RunEnv(key string) bool {
	switch key {
	case "PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "LANG", "TERM", "KUBECONFIG",
		"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy":
		return true
	}
	for _, p := range []string{"DOCKER_", "XDG_", "SSL_CERT_", "LC_"} {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// K8s drives an install on the Kubernetes shape, on the machine running OpenTofu: Dir is a
// private directory of the run, which holds the fetched installer's bootstrap script `<name>.sh`
// and is the installer's working directory; Kubeconfig what KUBECONFIG is set to (absolute paths;
// "" unsets it: kubectl's ~/.kube/config); Context, Namespace and Release the verbs'
// --kube-context, --namespace and --release ("" leaves the installer's default). Its Runner
// leaves HelmEnv and the input keys out of the environment (Filtered, Without).
type K8s struct {
	Runner     target.Runner
	Dir        string
	Name       string
	Kubeconfig string
	Context    string
	Namespace  string
	Release    string
	// PullLogin, when set, is the login an install or upgrade writes the cluster's pull Secret
	// from (`--image-pull-login`): the mirror's login (Mirror) when one is given, else the
	// provider's pull account, for the mirror's host.
	PullLogin *Login
	// Mirror, when set, is the mirror's own login every run reads the mirror with (MirrorFiles).
	Mirror *Login
}

// Filtered is r leaving HelmEnv out of its programs' environment, when it can (EnvFilter: the
// machine running OpenTofu); else r, which carries no environment.
func Filtered(r target.Runner) target.Runner {
	if f, ok := r.(target.EnvFilter); ok {
		return f.WithoutEnv(slices.Clone(HelmEnv))
	}
	return r
}

// ReleaseName is the helm release's name: Release, else the project's name, as the installer's
// default.
func (k K8s) ReleaseName() string {
	if k.Release != "" {
		return k.Release
	}
	return k.Name
}

// Without is k whose runs unset the input keys desc declares: the installer reads an exported
// variable of an input's name before an answers file or a --given file. A key the run itself
// needs (RunEnv) is kept.
func (k K8s) Without(desc *Describe) K8s {
	if f, ok := k.Runner.(target.EnvFilter); ok {
		k.Runner = f.WithoutEnv(InputEnv(desc))
	}
	return k
}

// InputEnv is the input keys desc declares that a run on this machine leaves out of its
// environment: every one but those the run needs itself (RunEnv). Nil desc: none.
func InputEnv(desc *Describe) []string {
	if desc == nil {
		return nil
	}
	var out []string
	for _, i := range desc.Inputs {
		if ValidInputKey(i.Key) && !RunEnv(i.Key) {
			out = append(out, i.Key)
		}
	}
	return out
}

// GivenKeys is the input keys a --given file gives that RunEnv keeps, sorted: what a run on this
// machine cannot give from it while its environment sets them. The file is read as the installer
// reads it (YAML, its inputs map); one that does not parse gives none (the installer refuses it).
func GivenKeys(given string) []string {
	var g struct {
		Inputs map[string]any `yaml:"inputs"`
	}
	if given == "" || yaml.Unmarshal([]byte(given), &g) != nil {
		return nil
	}
	var out []string
	for key := range g.Inputs {
		if RunEnv(key) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// Missing is the programs a run needs that the machine lacks, sorted.
func (k K8s) Missing(ctx context.Context) ([]string, error) {
	stdout, stderr, exit, err := k.Runner.Run(ctx, []string{"sh", "-c", ToolsScript}, nil)
	if err != nil {
		return nil, fmt.Errorf("looking for docker, helm and kubectl: %w", err)
	}
	if exit != 0 {
		return nil, &FailedError{Verb: "looking for docker, helm and kubectl", Exit: exit, Stderr: tail(stderr)}
	}
	return slices.Sorted(slices.Values(strings.Fields(string(stdout)))), nil
}

// docker is the bootstrap script's fetch, which runs as Docker's in Dir.
func (k K8s) docker() Docker { return Docker{Runner: k.Runner, WorkDir: k.Dir, Name: k.Name} }

// Fetch writes the installer's bootstrap script to Dir/<name>.sh (over one fetched before).
func (k K8s) Fetch(ctx context.Context, installer string, login *Login) error {
	return k.docker().Fetch(ctx, installer, login)
}

// run runs the fetched bootstrap script with args, files on standard input.
func (k K8s) run(ctx context.Context, files map[string]string, args ...string) ([]byte, []byte, int, error) {
	return k.runLogin(ctx, nil, files, args...)
}

// runLogin is run with the program given login's Docker configuration (K8sScript), when set.
func (k K8s) runLogin(ctx context.Context, login *Login, files map[string]string, args ...string) ([]byte, []byte, int, error) {
	// The mirror's login, but when the run's own login is that host's (the same one, given once).
	if k.Mirror != nil && !(login != nil && strings.EqualFold(login.Host, k.Mirror.Host)) {
		var err error
		if files, err = MirrorFiles(files, k.Mirror); err != nil {
			return nil, nil, -1, err
		}
	}
	names, host, user, stdin, err := loginInput(files, login)
	if err != nil {
		return nil, nil, -1, err
	}
	argv := append([]string{"sh", "-c", K8sScript, "sh", k.Dir, k.Kubeconfig, names, host, user, "sh", "./" + k.Name + ".sh"}, args...)
	return k.Runner.Run(ctx, argv, stdin)
}

// pull is the install's or upgrade's `--image-pull-login` and the login it runs with, when
// PullLogin asks for it.
func (k K8s) pull(args []string) ([]string, *Login) {
	if k.PullLogin == nil {
		return args, nil
	}
	return append(args, "--image-pull-login"), k.PullLogin
}

// scope is the release's flags: --namespace, --release and --kube-context, each when set.
func (k K8s) scope() []string {
	var out []string
	for _, f := range [][2]string{{"--namespace", k.Namespace}, {"--release", k.Release}, {"--kube-context", k.Context}} {
		if f[1] != "" {
			out = append(out, f[0], f[1])
		}
	}
	return out
}

// verb is `k8s <verb>`, then the release's flags, then args.
func (k K8s) verb(verb string, args ...string) []string {
	return append(append([]string{"k8s", verb}, k.scope()...), args...)
}

// Describe runs the fetched bootstrap script's `describe --json`.
func (k K8s) Describe(ctx context.Context) (*Describe, error) {
	stdout, stderr, exit, err := k.run(ctx, nil, "describe", "--json")
	if err != nil {
		return nil, fmt.Errorf("describe: %w", err)
	}
	if exit != 0 {
		return nil, &FailedError{Verb: "describe", Exit: exit, Stderr: tail(stderr), Cause: errOldInstaller}
	}
	desc, err := ParseDescribe(stdout)
	if errors.Is(err, ErrPreRoutes) {
		return nil, fmt.Errorf("%w; a release that runs such an installer is first moved on by hand: its own `k8s upgrade --version V` (Stowage %s or later), then run OpenTofu again", err, MinStowage)
	}
	return desc, err
}

// Status runs the fetched bootstrap script's `k8s status --json`: nil when the namespace holds no
// release of the name, which the installer refuses (exit 3) with `refused.absent`. Another refusal
// is a *RefusedError.
func (k K8s) Status(ctx context.Context) (*Status, error) {
	stdout, stderr, exit, err := k.run(ctx, nil, k.verb("status", "--json")...)
	if err != nil {
		return nil, fmt.Errorf("k8s status: %w", err)
	}
	return readStatus("k8s status", stdout, stderr, exit)
}

// k8sChange is c as an install's or an upgrade's flags: inputs by --set, secret inputs by an
// answers file. The front's settings are Docker's and the VM's: Kubernetes takes none.
func k8sChange(c Change) ([]string, map[string]string, error) {
	if c.TLS != "" || len(c.Domains) > 0 || len(c.Ports) > 0 {
		return nil, nil, errors.New("tls, domains and ports are the front's, which Kubernetes does not run: the chart's ingress owns them")
	}
	return changeArgs(c)
}

// Install runs `k8s install` with the fetched bootstrap script.
func (k K8s) Install(ctx context.Context, in Install) (*Result, error) {
	if in.Address != "" || in.Secrets != "" {
		return nil, errors.New("address and secrets_wo are not Kubernetes's: the release keeps its secrets in the cluster")
	}
	args, files, err := k8sChange(Change{Inputs: in.Inputs, SecretInputs: in.SecretInputs, TLS: in.TLS, Domains: in.Domains, Ports: in.Ports})
	if err != nil {
		return nil, err
	}
	args = append(k.verb("install", "--no-input", "--json"), args...)
	if in.Registry != "" {
		args = append(args, "--image-registry", in.Registry)
	}
	for name, v := range map[string]string{"given": in.Given, "registry-ca": in.RegistryCA} {
		if v != "" {
			files[name] = v
		}
	}
	args, login := k.pull(args)
	stdout, stderr, exit, err := k.runLogin(ctx, login, files, args...)
	if err != nil {
		return nil, fmt.Errorf("k8s install: %w", err)
	}
	return Outcome("k8s install", stdout, stderr, exit)
}

// Reconfigure applies the release that runs again with inputs and secret inputs given as
// `--given` (`k8s upgrade --given`, by the installer of that release), which replace the values
// the release's Secret records and adds those it does not. An input given "" is given empty.
func (k K8s) Reconfigure(ctx context.Context, c Change, registryCA string) (*Result, error) {
	if c.TLS != "" || len(c.Domains) > 0 || len(c.Ports) > 0 {
		return nil, errors.New("tls, domains and ports are the front's, which Kubernetes does not run")
	}
	inputs := maps.Clone(c.Inputs)
	if inputs == nil {
		inputs = map[string]string{}
	}
	maps.Copy(inputs, c.SecretInputs)
	given, err := GivenFile(k.Name, inputs)
	if err != nil {
		return nil, err
	}
	return k.Give(ctx, given, registryCA)
}

// Give applies the release that runs again given a --given file as it is (`k8s upgrade --given`,
// by the installer of that release): what another install gives, whose inputs replace the values
// the release's Secret records.
func (k K8s) Give(ctx context.Context, given, registryCA string) (*Result, error) {
	files := map[string]string{"given": given}
	if registryCA != "" {
		files["registry-ca"] = registryCA
	}
	args, login := k.pull(k.verb("upgrade", "--no-input", "--json"))
	stdout, stderr, exit, err := k.runLogin(ctx, login, files, args...)
	if err != nil {
		return nil, fmt.Errorf("k8s upgrade --given: %w", err)
	}
	return Outcome("k8s upgrade --given", stdout, stderr, exit)
}

// Reapply runs `k8s upgrade` given nothing: the release applied again by the installer of the
// release it runs, as a re-run finishes a first install.
func (k K8s) Reapply(ctx context.Context, registryCA string) (*Result, error) {
	files := map[string]string{}
	if registryCA != "" {
		files["registry-ca"] = registryCA
	}
	args, login := k.pull(k.verb("upgrade", "--no-input", "--json"))
	stdout, stderr, exit, err := k.runLogin(ctx, login, files, args...)
	if err != nil {
		return nil, fmt.Errorf("k8s upgrade: %w", err)
	}
	return Outcome("k8s upgrade", stdout, stderr, exit)
}

// Upgrade runs the fetched bootstrap script's `k8s upgrade`: the new release's installer, given
// the inputs the release adds.
func (k K8s) Upgrade(ctx context.Context, c Change, registryCA string) (*Result, error) {
	args, files, err := k8sChange(Change{Inputs: c.Inputs, SecretInputs: c.SecretInputs, Ports: c.Ports})
	if err != nil {
		return nil, err
	}
	if registryCA != "" {
		files["registry-ca"] = registryCA
	}
	args, login := k.pull(append(k.verb("upgrade", "--no-input", "--json"), args...))
	stdout, stderr, exit, err := k.runLogin(ctx, login, files, args...)
	if err != nil {
		return nil, fmt.Errorf("k8s upgrade: %w", err)
	}
	return Outcome("k8s upgrade", stdout, stderr, exit)
}

// Rollback runs `k8s rollback --json`: back one upgrade, to the release status --json names
// previous.
func (k K8s) Rollback(ctx context.Context) (*Result, error) {
	stdout, stderr, exit, err := k.run(ctx, nil, k.verb("rollback", "--json")...)
	if err != nil {
		return nil, fmt.Errorf("k8s rollback: %w", err)
	}
	return Outcome("k8s rollback", stdout, stderr, exit)
}

// Uninstall runs `k8s uninstall --yes`, with --data when data is true.
func (k K8s) Uninstall(ctx context.Context, data bool) (*Result, error) {
	args := k.verb("uninstall", "--yes", "--no-input", "--json")
	if data {
		args = append(args, "--data")
	}
	stdout, stderr, exit, err := k.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("k8s uninstall: %w", err)
	}
	return Outcome("k8s uninstall", stdout, stderr, exit)
}

// GivenFile is the installer's --given file of project's inputs: `project` and `inputs`, each
// value a JSON string (a YAML double-quoted scalar), keys sorted.
func GivenFile(project string, inputs map[string]string) (string, error) {
	if len(inputs) == 0 {
		return "", errors.New("--given: no input to give")
	}
	p, err := json.Marshal(project)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project: %s\ninputs:\n", p)
	for _, key := range slices.Sorted(maps.Keys(inputs)) {
		if !ValidInputKey(key) {
			return "", fmt.Errorf("--given: %q is not an input key", key)
		}
		v, err := json.Marshal(inputs[key])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "  %s: %s\n", key, v)
	}
	return b.String(), nil
}
