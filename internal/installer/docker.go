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

// StowScript runs the installer the install keeps, <work_dir>/<name>/<short>-stow, with the
// arguments given and --dir. Exit NotInstalled: no install directory. Arguments: work_dir,
// name, then the installer's arguments.
const StowScript = `set -eu
cd -- "$1"
dir=./$2
shift 2
[ -d "$dir" ] || { echo "stowage-provider: no install directory" >&2; exit 100; }
stow=
for f in "$dir"/*-stow; do
  [ -f "$f" ] && [ -x "$f" ] || continue
  [ -z "$stow" ] || { echo "$dir holds more than one *-stow installer" >&2; exit 1; }
  stow=$f
done
[ -n "$stow" ] || { echo "$dir holds no *-stow installer" >&2; exit 1; }
exec "$stow" "$@" --dir "$dir"
`

var (
	digestRef = regexp.MustCompile(`^([a-z0-9][a-z0-9._:-]*(?:/[a-z0-9][a-z0-9._-]*)*)/([a-z0-9][a-z0-9._-]*)-installer@sha256:[0-9a-f]{64}$`)
	inputKey  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	routeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	fileName  = regexp.MustCompile(`^[a-z][a-z-]*$`)
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

// ValidInputKey says whether key can be an input's key on the command line and in an answers
// file.
func ValidInputKey(key string) bool { return inputKey.MatchString(key) && key != "tls" }

// ValidRoute says whether name can be a route's name in --domains and --ports.
func ValidRoute(name string) bool { return routeName.MatchString(name) }

// Docker drives an install on the Docker shape: <work_dir>/<name>.sh is the bootstrap script,
// <work_dir>/<name> the install directory.
type Docker struct {
	Runner  target.Runner
	WorkDir string
	Name    string
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
	argv := []string{"sh", "-c", RunScript, "sh", d.WorkDir, "", "sh", "./" + d.Name + ".sh", "describe", "--json"}
	stdout, stderr, exit, err := d.Runner.Run(ctx, argv, nil)
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
	files := map[string]string{}
	if len(in.SecretInputs) > 0 {
		answers, err := answersFile(in.SecretInputs)
		if err != nil {
			return nil, nil, err
		}
		files["answers"] = answers
	}
	for name, v := range map[string]string{"given": in.Given, "secrets": in.Secrets, "registry-ca": in.RegistryCA} {
		if v != "" {
			files[name] = v
		}
	}
	names := slices.Sorted(maps.Keys(files))
	argv := append([]string{"sh", "-c", RunScript, "sh", d.WorkDir, strings.Join(names, " ")}, args...)
	if len(files) == 0 {
		return argv, nil, nil
	}
	stdin, err := tarFiles(files)
	return argv, stdin, err
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
	argv := []string{"sh", "-c", StowScript, "sh", d.WorkDir, d.Name, "status", "--json"}
	stdout, stderr, exit, err := d.Runner.Run(ctx, argv, nil)
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
	argv := []string{"sh", "-c", StowScript, "sh", d.WorkDir, d.Name, "uninstall", "--yes", "--no-input", "--json"}
	if data {
		argv = append(argv, "--data")
	}
	stdout, stderr, exit, err := d.Runner.Run(ctx, argv, nil)
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
