package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// NotInstalled is the exit status StowScript gives when the install directory does not exist,
// with NotInstalledMark alone on stderr: both, so an installer's own exit 100 is not taken for it.
const (
	NotInstalled     = 100
	NotInstalledMark = "stowage-provider: no install directory"
)

// FetchScript writes the bootstrap script an installer image prints to <work_dir>/<name>.sh, on
// the target. With a user, standard input is a tar holding `password`, and the login goes to a
// Docker configuration of its own that is removed afterwards, so the credential is not left on
// the host. Arguments: work_dir, image, name, registry host, user (empty: no login).
const FetchScript = `set -eu
cd -- "$1"
img=$2 name=$3 host=$4 user=$5
old=$(umask)
umask 077
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
trap 'exit 130' HUP INT TERM
if [ -n "$user" ]; then
  tar -xf - -C "$d"
  DOCKER_CONFIG=$d/docker
  export DOCKER_CONFIG
  mkdir "$DOCKER_CONFIG"
  docker login --username "$user" --password-stdin "$host" < "$d/password" >&2
fi
docker run --rm "$img" > "$d/script"
[ -s "$d/script" ] || { echo "$img printed no bootstrap script" >&2; exit 1; }
umask "$old"
cat "$d/script" > "./.$name.sh.part"
mv -f "./.$name.sh.part" "./$name.sh"
`

// RunScript runs a program in work_dir with files given on standard input: a tar of the files
// named, each passed as --<name> <path> after the program's arguments, in a private directory
// removed afterwards. The program runs with the caller's umask. Arguments: work_dir, the file
// names (space-separated, may be empty), then the program and its arguments.
const RunScript = `set -eu
cd -- "$1"
files=$2
shift 2
old=$(umask)
umask 077
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
trap 'exit 130' HUP INT TERM
if [ -n "$files" ]; then
  tar -xf - -C "$d"
  for f in $files; do set -- "$@" "--$f" "$d/$f"; done
fi
umask "$old"
rc=0
"$@" || rc=$?
exit "$rc"
`

// StowScript runs the installer the install keeps, <work_dir>/<name>/<short>-stow (else the one
// *-stow there, when the short name changed since), with the
// arguments given, then --dir when asked, and with files given on standard input as RunScript
// does. Exit NotInstalled: no install directory. Arguments: work_dir, name, short, the file
// names (space-separated, may be empty), "dir" or "" (append --dir), then the installer's
// arguments.
const StowScript = `set -eu
cd -- "$1"
dir=./$2 stow=./$2/$3-stow files=$4 withdir=$5
shift 5
[ -d "$dir" ] || { echo "stowage-provider: no install directory" >&2; exit 100; }
if ! { [ -f "$stow" ] && [ -x "$stow" ]; }; then
  # Another short name since (a release that renamed its installer): the one *-stow kept.
  found=
  for f in "$dir"/*-stow; do
    [ -f "$f" ] && [ -x "$f" ] || continue
    [ -z "$found" ] || { echo "$stow: no installer kept there, and $dir holds more than one *-stow" >&2; exit 1; }
    found=$f
  done
  [ -n "$found" ] || { echo "$stow: no installer kept there" >&2; exit 1; }
  stow=$found
fi
[ -z "$withdir" ] || set -- "$@" --dir "$dir"
old=$(umask)
umask 077
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
trap 'exit 130' HUP INT TERM
if [ -n "$files" ]; then
  tar -xf - -C "$d"
  for f in $files; do set -- "$@" "--$f" "$d/$f"; done
fi
umask "$old"
rc=0
"$stow" "$@" || rc=$?
exit "$rc"
`

var (
	digestRef = regexp.MustCompile(`^([a-z0-9][a-z0-9._:-]*(?:/[a-z0-9][a-z0-9._-]*)*)/([a-z0-9][a-z0-9._-]*)-installer@sha256:[0-9a-f]{64}$`)
	inputKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	routeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	fileName  = regexp.MustCompile(`^[a-z][a-z-]*$`)
	// projectName is the <name> of `<name>-installer`, digestRef's second group, and an
	// installer's short name.
	projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// ParseInstaller reads an installer image reference pinned by digest,
// `<prefix>/<name>-installer@sha256:<64 hex>`, and returns the project's name and the registry
// host a login goes to (docker.io for a reference without one).
func ParseInstaller(ref string) (name, host string, err error) {
	m := digestRef.FindStringSubmatch(ref)
	if m == nil {
		return "", "", fmt.Errorf("installer %q: want <prefix>/<name>-installer@sha256:<64 hex>, the digest `ship` prints (a tag can move)", ref)
	}
	host, _, _ = strings.Cut(m[1], "/")
	if !strings.ContainsAny(host, ".:") && host != "localhost" {
		host = "docker.io"
	}
	return m[2], host, nil
}

// ValidName says whether name can be a project's name, as an installer image names it.
func ValidName(name string) bool { return projectName.MatchString(name) }

// ValidInputKey says whether key can be an input's key on the command line and in an answers
// file.
func ValidInputKey(key string) bool { return inputKey.MatchString(key) && key != "tls" }

// ValidRoute says whether name can be a route's name in --domains and --ports.
func ValidRoute(name string) bool { return routeName.MatchString(name) }

// Docker drives an install on the Docker shape: <work_dir>/<name>.sh is the bootstrap script,
// <work_dir>/<name> the install directory.
// Short is the installer's short name, so <work_dir>/<name>/<Short>-stow is the installer the
// install keeps.
type Docker struct {
	Runner  target.Runner
	WorkDir string
	Name    string
	Short   string
}

// Login is the pull account the installer image is fetched with.
type Login struct {
	Host, Username, Password string
}

// Fetch writes the installer's bootstrap script on the target.
func (d Docker) Fetch(ctx context.Context, installer string, login *Login) error {
	argv := []string{"sh", "-c", FetchScript, "sh", d.WorkDir, installer, d.Name, "", ""}
	var stdin []byte
	if login != nil && login.Username != "" {
		argv[7], argv[8] = login.Host, login.Username
		var err error
		if stdin, err = tarFiles(map[string]string{"password": login.Password}); err != nil {
			return err
		}
	}
	_, stderr, exit, err := d.Runner.Run(ctx, argv, stdin)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", installer, err)
	}
	if exit != 0 {
		return &FailedError{Verb: "fetching the installer " + installer, Exit: exit, Stderr: tail(stderr)}
	}
	return nil
}

// Describe runs the fetched bootstrap script's `describe --json`.
func (d Docker) Describe(ctx context.Context) (*Describe, error) {
	stdout, stderr, exit, err := d.run(ctx, nil, "sh", "./"+d.Name+".sh", "describe", "--json")
	if err != nil {
		return nil, fmt.Errorf("describe: %w", err)
	}
	if exit != 0 {
		return nil, &FailedError{Verb: "describe", Exit: exit, Stderr: tail(stderr)}
	}
	return ParseDescribe(stdout)
}

// Install is a first install's settings.
type Install struct {
	Inputs       map[string]string // --set, in the clear on the command line
	SecretInputs map[string]string // an answers file on standard input
	Given        string            // --given, on standard input
	Secrets      string            // --secrets, on standard input
	TLS          string
	Domains      map[string]string
	Ports        map[string]string
	Address      string
	Registry     string
	RegistryCA   string // --registry-ca, on standard input
}

// InstallArgv is the install's command line and standard input: nothing secret is on the
// command line.
func (d Docker) InstallArgv(in Install) ([]string, []byte, error) {
	args := []string{"sh", "./" + d.Name + ".sh", "--no-input", "--json", "--dir", "./" + d.Name}
	for _, k := range slices.Sorted(maps.Keys(in.Inputs)) {
		if !ValidInputKey(k) {
			return nil, nil, fmt.Errorf("inputs: %q is not an input key", k)
		}
		args = append(args, "--set", k+"="+in.Inputs[k])
	}
	if in.TLS != "" {
		args = append(args, "--tls", in.TLS)
	}
	if v, err := routeList("domains", in.Domains); err != nil {
		return nil, nil, err
	} else if v != "" {
		args = append(args, "--domains", v)
	}
	if v, err := routeList("ports", in.Ports); err != nil {
		return nil, nil, err
	} else if v != "" {
		args = append(args, "--ports", v)
	}
	if in.Address != "" {
		args = append(args, "--address", in.Address)
	}
	if in.Registry != "" {
		args = append(args, "--registry", in.Registry)
	}
	files, err := withAnswers(in.SecretInputs, map[string]string{"given": in.Given, "secrets": in.Secrets, "registry-ca": in.RegistryCA})
	if err != nil {
		return nil, nil, err
	}
	names, stdin, err := filesInput(files)
	if err != nil {
		return nil, nil, err
	}
	return append([]string{"sh", "-c", RunScript, "sh", d.WorkDir, names}, args...), stdin, nil
}

// withAnswers is files, the empty ones left out, with an answers file of secrets when there are
// any.
func withAnswers(secrets, files map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for name, v := range files {
		if v != "" {
			out[name] = v
		}
	}
	if len(secrets) > 0 {
		answers, err := answersFile(secrets)
		if err != nil {
			return nil, err
		}
		out["answers"] = answers
	}
	return out, nil
}

// filesInput is the file names a script takes (sorted, space-separated) and the tar it reads
// them from; none gives "" and no input.
func filesInput(files map[string]string) (string, []byte, error) {
	if len(files) == 0 {
		return "", nil, nil
	}
	stdin, err := tarFiles(files)
	return strings.Join(slices.Sorted(maps.Keys(files)), " "), stdin, err
}

// run runs args in work_dir by RunScript, with files on standard input.
func (d Docker) run(ctx context.Context, files map[string]string, args ...string) ([]byte, []byte, int, error) {
	names, stdin, err := filesInput(files)
	if err != nil {
		return nil, nil, -1, err
	}
	return d.Runner.Run(ctx, append([]string{"sh", "-c", RunScript, "sh", d.WorkDir, names}, args...), stdin)
}

// stow runs the kept installer with args (then --dir when withDir) and files on standard input.
func (d Docker) stow(ctx context.Context, files map[string]string, withDir bool, args ...string) ([]byte, []byte, int, error) {
	if !ValidShortName(d.Short) {
		return nil, nil, -1, fmt.Errorf("short_name %q in state is not an installer's short name: import the install again (tofu state rm, then tofu import)", d.Short)
	}
	names, stdin, err := filesInput(files)
	if err != nil {
		return nil, nil, -1, err
	}
	dir := ""
	if withDir {
		dir = "dir"
	}
	argv := append([]string{"sh", "-c", StowScript, "sh", d.WorkDir, d.Name, d.Short, names, dir}, args...)
	return d.Runner.Run(ctx, argv, stdin)
}

// KeptDescribe runs `describe --json` of the installer the install keeps: the running release's
// offer. Exit NotInstalled is an error here: the caller read the install first.
func (d Docker) KeptDescribe(ctx context.Context) (*Describe, error) {
	stdout, stderr, exit, err := d.stow(ctx, nil, false, "describe", "--json")
	if err != nil {
		return nil, fmt.Errorf("describe: %w", err)
	}
	if exit != 0 {
		return nil, &FailedError{Verb: "describe (the installer the install keeps)", Exit: exit, Stderr: tail(stderr)}
	}
	return ParseDescribe(stdout)
}

// Change is what a re-run or an upgrade is given. An input given "" is `--set KEY=`, the
// installer's rule for an empty value deciding what it does.
type Change struct {
	Inputs       map[string]string // --set, in the clear on the command line
	SecretInputs map[string]string // an answers file on standard input
	TLS          string            // --tls; "" keeps the recorded mode
	Domains      map[string]string // --domains
	Ports        map[string]string // --ports
}

// Empty says whether the change gives nothing.
func (c Change) Empty() bool {
	return len(c.Inputs) == 0 && len(c.SecretInputs) == 0 && c.TLS == "" && len(c.Domains) == 0 && len(c.Ports) == 0
}

// changeArgs is c as flags after the verb's own, and its files.
func changeArgs(c Change, verbArgs ...string) ([]string, map[string]string, error) {
	args := append([]string(nil), verbArgs...)
	for _, k := range slices.Sorted(maps.Keys(c.Inputs)) {
		if !ValidInputKey(k) {
			return nil, nil, fmt.Errorf("inputs: %q is not an input key", k)
		}
		args = append(args, "--set", k+"="+c.Inputs[k])
	}
	if c.TLS != "" {
		args = append(args, "--tls", c.TLS)
	}
	if v, err := routeList("domains", c.Domains); err != nil {
		return nil, nil, err
	} else if v != "" {
		args = append(args, "--domains", v)
	}
	if v, err := routeList("ports", c.Ports); err != nil {
		return nil, nil, err
	} else if v != "" {
		args = append(args, "--ports", v)
	}
	files, err := withAnswers(c.SecretInputs, nil)
	return args, files, err
}

// Rerun runs the kept installer again on the install with the change: the running release
// reconfigured (`<short>-stow --no-input --json --set … --dir`).
func (d Docker) Rerun(ctx context.Context, c Change) (*Result, error) {
	args, files, err := changeArgs(c, "--no-input", "--json")
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.stow(ctx, files, true, args...)
	if err != nil {
		return nil, fmt.Errorf("re-run: %w", err)
	}
	return Outcome("re-run", stdout, stderr, exit)
}

// Upgrade runs the fetched bootstrap script's `upgrade` on the install: the new release's
// installer, given the inputs and ports the release adds.
func (d Docker) Upgrade(ctx context.Context, c Change) (*Result, error) {
	added := Change{Inputs: c.Inputs, SecretInputs: c.SecretInputs, Ports: c.Ports} // --tls and --domains are a re-run's
	args, files, err := changeArgs(added, "sh", "./"+d.Name+".sh", "upgrade", "--no-input", "--json", "--dir", "./"+d.Name)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.run(ctx, files, args...)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	return Outcome("upgrade", stdout, stderr, exit)
}

// Rollback runs the kept installer's `rollback --json`: back to the release the last upgrade
// replaced.
func (d Docker) Rollback(ctx context.Context) (*Result, error) {
	stdout, stderr, exit, err := d.stow(ctx, nil, true, "rollback", "--json")
	if err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}
	return Outcome("rollback", stdout, stderr, exit)
}

// Install runs the first install with the fetched bootstrap script.
func (d Docker) Install(ctx context.Context, in Install) (*Result, error) {
	argv, stdin, err := d.InstallArgv(in)
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.Runner.Run(ctx, argv, stdin)
	if err != nil {
		return nil, fmt.Errorf("install: %w", err)
	}
	return Outcome("install", stdout, stderr, exit)
}

// Status runs the kept installer's `status --json`; nil without an install directory.
func (d Docker) Status(ctx context.Context) (*Status, error) {
	stdout, stderr, exit, err := d.stow(ctx, nil, true, "status", "--json")
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	switch {
	case exit == 0:
		return ParseStatus(stdout)
	case notInstalled(stdout, stderr, exit):
		return nil, nil
	}
	return nil, &FailedError{Verb: "status", Exit: exit, Stderr: tail(stderr)}
}

// Uninstall runs the kept installer's `uninstall --yes`, with --data when data is true.
func (d Docker) Uninstall(ctx context.Context, data bool) (*Result, error) {
	args := []string{"uninstall", "--yes", "--no-input", "--json"}
	if data {
		args = append(args, "--data")
	}
	stdout, stderr, exit, err := d.stow(ctx, nil, true, args...)
	if err != nil {
		return nil, fmt.Errorf("uninstall: %w", err)
	}
	if notInstalled(stdout, stderr, exit) {
		return &Result{Result: "nothing"}, nil
	}
	return Outcome("uninstall", stdout, stderr, exit)
}

func notInstalled(stdout, stderr []byte, exit int) bool {
	return exit == NotInstalled && len(stdout) == 0 && strings.TrimSpace(string(stderr)) == NotInstalledMark
}

func routeList(field string, m map[string]string) (string, error) {
	parts := make([]string, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		v := m[k]
		if !ValidRoute(k) {
			return "", fmt.Errorf("%s: %q is not a route name", field, k)
		}
		if v == "" || strings.ContainsAny(v, ",= \t\r\n") {
			return "", fmt.Errorf("%s.%s: %q is not a single value", field, k, v)
		}
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ","), nil
}

// answersFile is an answers file of the inputs: one `KEY: "value"` line each, sorted; a JSON
// string is a YAML double-quoted scalar.
func answersFile(inputs map[string]string) (string, error) {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(inputs)) {
		if !ValidInputKey(k) {
			return "", fmt.Errorf("secret_inputs_wo: %q is not an input key", k)
		}
		v, err := json.Marshal(inputs[k])
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	return b.String(), nil
}

// tarFiles is a tar of the files, sorted, mode 0600, with no time or owner: the same files give
// the same bytes.
func tarFiles(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if !fileName.MatchString(name) {
			return nil, fmt.Errorf("file name %q", name)
		}
		data := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Unix(0, 0), Format: tar.FormatUSTAR}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(data)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if buf.Len() == 0 {
		return nil, errors.New("no file to send")
	}
	return buf.Bytes(), nil
}
