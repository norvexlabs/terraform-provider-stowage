package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseDescribe(t *testing.T) {
	t.Parallel()
	d, err := ParseDescribe([]byte(`{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.2.0","revision":2},"channel":"stable",
		"shapes":["docker"],"inputs":[{"key":"ADMIN_EMAIL","required":true,"default":"a@b"}],"first_install_only":["address"],"a_later_field":{"x":1},"routes":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "notes" || d.ShortName != "notes" || d.Release.Version != "1.2.0" || d.Release.Revision != 2 || d.Inputs[0].Key != "ADMIN_EMAIL" ||
		!slices.Equal(d.FirstInstallOnly, []string{"address"}) || !d.Declares("ADMIN_EMAIL") || d.Declares("OTHER") {
		t.Fatalf("got %+v", d)
	}
	for doc, want := range map[string]string{
		`{"name":"notes","release":{"version":"1"}}`:                                               "predates the machine interface; use an installer made by Stowage " + MinStowage,
		`{"format":2,"name":"notes","release":{"version":"1"}}`:                                    "format 2 is newer than this provider reads (format 1): upgrade terraform-provider-stowage",
		`{"format":0,"name":"notes","release":{"version":"1"}}`:                                    "format 0 is unknown",
		`{"format":1,"name":"notes","short_name":"n","release":{"version":"1"},"routes":[]} {}`:    "data after",
		`{"format":1,"name":"notes","short_name":"n","release":{"version":"1"}}`:                   "no routes: the installer predates the machine interface this provider reads; use one made by Stowage " + MinStowage,
		`{"format":"1","name":"notes"}`:                                                            "cannot unmarshal",
		`{"format":1,"release":{"version":"1"}}`:                                                   "no name",
		`{"format":1,"name":"notes","short_name":"n","release":{},"routes":[]}`:                    "release has no version",
		`{"format":1,"name":"notes","release":{"version":"1"}}`:                                    `short_name "" is not`,
		`{"format":1,"name":"notes","short_name":"../x","release":{"version":"1"},"routes":[]}`:    `short_name "../x" is not`,
		`{"format":1,"name":"notes","short_name":".hidden","release":{"version":"1"},"routes":[]}`: `short_name ".hidden" is not`,
	} {
		_, err := ParseDescribe([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	s, err := ParseStatus([]byte(`{"format":1,"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"r/notes-installer@` + digest + `"},
		"channel":"stable","previous":null,"inputs":["B","A","A"],"restore_awaited":true,"volumes_split":true,"services":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Installed.Installer == "" || s.Previous != nil || s.Inputs == nil || !slices.Equal(*s.Inputs, []string{"A", "B"}) || !s.RestoreAwaited || !s.VolumesSplit {
		t.Fatalf("got %+v", s)
	}
	s, err = ParseStatus([]byte(`{"format":1,"name":"notes","installed":null,"installing":true,"inputs":["Z","Y"]}`))
	if err != nil || s.Installed != nil || !s.Installing || !slices.Equal(*s.Inputs, []string{"Y", "Z"}) {
		t.Fatalf("got %+v %v", s, err)
	}
	// inputs absent is unknown, never none; [] is none.
	if s, err = ParseStatus([]byte(`{"format":1,"name":"notes"}`)); err != nil || s.Inputs != nil {
		t.Fatalf("absent inputs: %+v %v", s, err)
	}
	if s, err = ParseStatus([]byte(`{"format":1,"name":"notes","inputs":[]}`)); err != nil || s.Inputs == nil || len(*s.Inputs) != 0 {
		t.Fatalf("no inputs: %+v %v", s, err)
	}
	for doc, want := range map[string]string{
		`{"format":2,"name":"notes"}`:                           "format 2 is newer",
		`{"name":"notes"}`:                                      "status --json: the output has no format",
		`{"format":1,"name":"notes","previous":{"revision":1}}`: "previous has no version",
		`{"format":1,"name":"notes","inputs":"A"}`:              "inputs: want an array",
		`{"format":1,"name":"notes","inputs":{"A":"v"}}`:        "inputs: want an array",
		`{"format":1,"name":"notes","installing":"yes"}`:        "cannot unmarshal",
		`{"format":1,"installed":{"version":"1"}}`:              "no name",
	} {
		_, err := ParseStatus([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

// The results as Stowage's installers print them (printResult: indented, one object, a newline).
const (
	resultDone = `{
  "format": 1,
  "result": "done",
  "release": {
    "version": "2.0.0",
    "revision": 1
  }
}
`
	resultNothing = `{
  "format": 1,
  "result": "nothing",
  "release": {
    "version": "1.0.0",
    "revision": 1
  }
}
`
	resultUninstalled = `{
  "format": 1,
  "result": "done"
}
`
	resultRefused = `{
  "format": 1,
  "result": "nothing",
  "release": {
    "version": "1.0.0",
    "revision": 1
  },
  "refused": {
    "reason": "notes 1.0.0 (revision 1) is installed, after this installer's 0.9.0",
    "fix": "nothing to do: the install runs a later release; upgrade with notes-stow or a later release's installer"
  }
}
`
)

func TestOutcome(t *testing.T) {
	t.Parallel()
	r, err := Outcome("upgrade", []byte(resultDone), []byte("pulling…\n"), 0)
	if err != nil || r.Result != "done" || r.Release.Version != "2.0.0" || r.Release.Revision != 1 {
		t.Fatalf("done: %+v %v", r, err)
	}
	if r, err := Outcome("upgrade", []byte(resultNothing), nil, 0); err != nil || r.Result != "nothing" || r.Release.Version != "1.0.0" {
		t.Fatalf("nothing: %+v %v", r, err)
	}
	if r, err := Outcome("uninstall", []byte(resultUninstalled), nil, 0); err != nil || r.Result != "done" || r.Release != nil {
		t.Fatalf("uninstalled: %+v %v", r, err)
	}
	// A field this provider does not read is ignored.
	if _, err := Outcome("install", []byte(`{"format":1,"result":"done","later":true}`), nil, 0); err != nil {
		t.Fatalf("unknown field: %v", err)
	}

	_, err = Outcome("upgrade", []byte(resultRefused), []byte("error: …"), 3)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Verb != "upgrade" ||
		refused.Reason != "notes 1.0.0 (revision 1) is installed, after this installer's 0.9.0" ||
		!strings.HasPrefix(refused.Fix, "nothing to do: the install runs a later release") {
		t.Fatalf("refused: %v", err)
	}

	var failed *FailedError
	for name, c := range map[string]struct {
		out  string
		exit int
		want string
	}{
		"exit 1":                  {"", 1, "install failed (exit 1)\nboom"},
		"exit 1 with an object":   {resultDone, 1, "install failed (exit 1)\nboom"},
		"exit 255":                {"", 255, "ssh's own failure"},
		"exit 3, no object":       {"", 3, "install --json: no result object: the installer breaks the machine interface, whose exit 3"},
		"exit 3, text":            {"text", 3, "invalid character"},
		"exit 3, no refusal":      {resultNothing, 3, "exit 3 without a refusal"},
		"exit 3, no reason":       {`{"format":1,"result":"nothing","refused":{"reason":"","fix":"x"}}`, 3, "exit 3 without a refusal"},
		"exit 3, done":            {`{"format":1,"result":"done","refused":{"reason":"r","fix":"x"}}`, 3, `a refusal whose result is "done"`},
		"exit 0, refused":         {resultRefused, 0, "a refusal with exit 0"},
		"exit 0, bad word":        {`{"format":1,"result":"maybe"}`, 0, `result "maybe" is neither`},
		"exit 0, no object":       {"", 0, "no result object"},
		"exit 0, text first":      {"log line\n" + resultDone, 0, "invalid character"},
		"exit 0, data after":      {resultDone + "{}", 0, "data after the JSON object"},
		"exit 0, release no vers": {`{"format":1,"result":"done","release":{"revision":1}}`, 0, "release has no version"},
		"exit 0, wrong type":      {`{"format":1,"result":"done","release":{"version":2}}`, 0, "cannot unmarshal"},
	} {
		_, err := Outcome("install", []byte(c.out), []byte("boom\n"), c.exit)
		if !errors.As(err, &failed) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
	var ferr *FormatError
	if _, err := Outcome("install", []byte(`{"format":2,"result":"done"}`), nil, 0); !errors.As(err, &ferr) || ferr.Format != 2 {
		t.Fatalf("format: %v", err)
	}
	if _, err := Outcome("install", []byte(`{"result":"nothing","refused":{"reason":"r","fix":"x"}}`), nil, 3); !errors.As(err, &ferr) || !ferr.Missing {
		t.Fatalf("no format: %v", err)
	}
}

func TestParseInstaller(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string][2]string{
		"registry.example.com/notes/notes-installer@" + digest: {"notes", "registry.example.com"},
		"localhost:5000/x/atlas-search-installer@" + digest:    {"atlas-search", "localhost:5000"},
		"acme/notes-installer@" + digest:                       {"notes", "docker.io"},
	} {
		name, host, err := ParseInstaller(ref)
		if err != nil || name != want[0] || host != want[1] {
			t.Errorf("%s: got %q %q %v", ref, name, host, err)
		}
	}
	for _, ref := range []string{
		"registry.example.com/notes/notes-installer:latest",
		"registry.example.com/notes/notes-installer:1.0@" + digest,
		"registry.example.com/notes/notes@" + digest,
		"registry.example.com/notes/notes-installer@sha256:0123",
		"registry.example.com/notes/notes-installer@sha256:" + strings.ToUpper(strings.TrimPrefix(digest, "sha256:")),
		"notes-installer@" + digest,
		" registry.example.com/notes/notes-installer@" + digest,
	} {
		if _, _, err := ParseInstaller(ref); err == nil || !strings.Contains(err.Error(), "@sha256:<64 hex>") {
			t.Errorf("%s: accepted (%v)", ref, err)
		}
	}
}

func untar(t *testing.T, b []byte) map[string]string {
	t.Helper()
	out := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(tr)
		out[h.Name] = string(data)
	}
}

func TestInstallArgvKeepsSecretsOffTheCommandLine(t *testing.T) {
	t.Parallel()
	d := Docker{WorkDir: "/srv", Name: "notes"}
	in := Install{
		Inputs:       map[string]string{"B": "2", "A": "1 one"},
		SecretInputs: map[string]string{"SMTP_PASSWORD": "pa\"ss\nword-SECRET"},
		Given:        "project: notes\ninputs: {K: given-SECRET}\n",
		Secrets:      "exported-SECRET",
		TLS:          "internal",
		Domains:      map[string]string{"web": "notes.lan", "api": "api.notes.lan"},
		Ports:        map[string]string{"web": "8080"},
		Registry:     "mirror.lan/notes",
		RegistryCA:   "-----BEGIN CERTIFICATE-----\n",
	}
	argv, stdin, err := d.InstallArgv(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(argv, " "), "SECRET") {
		t.Fatalf("a secret is on the command line: %q", argv)
	}
	want := []string{"sh", "-c", RunScript, "sh", "/srv", "answers given registry-ca secrets",
		"sh", "./notes.sh", "--no-input", "--json", "--dir", "./notes", "--set", "A=1 one", "--set", "B=2",
		"--tls", "internal", "--domains", "api=api.notes.lan,web=notes.lan", "--ports", "web=8080", "--registry", "mirror.lan/notes"}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
	files := untar(t, stdin)
	if files["answers"] != "SMTP_PASSWORD: \"pa\\\"ss\\nword-SECRET\"\n" || files["given"] != in.Given || files["secrets"] != in.Secrets || files["registry-ca"] != in.RegistryCA {
		t.Fatalf("files %q", files)
	}
	again, _, _ := d.InstallArgv(in)
	stdin2, _ := func() ([]byte, error) { _, s, err := d.InstallArgv(in); return s, err }()
	if !slices.Equal(argv, again) || !bytes.Equal(stdin, stdin2) {
		t.Fatal("the same install gives another command line or input")
	}
	if _, _, err := d.InstallArgv(Install{Inputs: map[string]string{"A=B": "x"}}); err == nil {
		t.Fatal("a key with = accepted")
	}
	if _, _, err := d.InstallArgv(Install{SecretInputs: map[string]string{"tls": "x"}}); err == nil {
		t.Fatal("the answers file's reserved tls key accepted")
	}
	if _, _, err := d.InstallArgv(Install{Domains: map[string]string{"web": "a,b"}}); err == nil {
		t.Fatal("a domain with a comma accepted")
	}
	argv, stdin, err = d.InstallArgv(Install{})
	if err != nil || stdin != nil || argv[5] != "" {
		t.Fatalf("no files: %q %v %v", argv, stdin, err)
	}
}

// envRunner runs on this machine with dir first in PATH, where fake programs stand in for
// docker.
type envRunner struct{ dir string }

func (e envRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	return target.Local{}.Run(ctx, append([]string{"env", "PATH=" + e.dir + ":" + os.Getenv("PATH")}, argv...), stdin)
}

func needSh(t *testing.T) {
	t.Helper()
	for _, p := range []string{"sh", "tar", "env", "mktemp"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skip("needs " + p)
		}
	}
}

func writeExe(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The scripts run for real under sh: the files arrive as flags, the kept installer is found,
// the login stays in a configuration of its own.
func TestScriptsUnderSh(t *testing.T) {
	t.Parallel()
	needSh(t)
	work, bin := t.TempDir(), t.TempDir()
	log := filepath.Join(bin, "log")
	writeExe(t, filepath.Join(bin, "docker"), `case $1 in
login) echo "login $*" >> `+log+` ;;
run) echo "run $* cfg=$DOCKER_CONFIG $(cat "$DOCKER_CONFIG/config.json")" >> `+log+`; echo 'echo bootstrap "$@"' ;;
esac
`)
	d := Docker{Runner: envRunner{bin}, WorkDir: work, Name: "notes"}
	ctx := context.Background()
	img := "registry.example.com/notes/notes-installer@" + digest
	if err := d.Fetch(ctx, img, &Login{Host: "registry.example.com", Username: "pull", Password: "pw-SECRET"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(got)), "\n")
	// The login is the configuration's own auths entry, never docker login (a credential helper
	// would keep it), and the password is on no command line.
	wantCfg := `{"auths":{"registry.example.com":{"auth":"cHVsbDpwdy1TRUNSRVQ="}}}`
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "run run --rm "+img+" cfg=") || !strings.HasSuffix(lines[0], " "+wantCfg) {
		t.Fatalf("docker calls %q", lines)
	}
	cfg := strings.Fields(strings.TrimPrefix(lines[0][strings.Index(lines[0], "cfg="):], "cfg="))[0]
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Fatalf("the login's configuration %s is left: %v", cfg, err)
	}
	if b, _ := os.ReadFile(filepath.Join(work, "notes.sh")); string(b) != "echo bootstrap \"$@\"\n" {
		t.Fatalf("bootstrap script %q", b)
	}

	// RunScript: each file is a flag, its content intact, its directory gone afterwards.
	writeExe(t, filepath.Join(work, "show"), `while [ $# -gt 0 ]; do case $1 in --*) echo "$1=$(cat "$2")"; echo "$2" >> `+log+`; shift 2;; *) shift;; esac; done; exit 7
`)
	stdin, err := tarFiles(map[string]string{"answers": "K: \"v\"", "given": "g"})
	if err != nil {
		t.Fatal(err)
	}
	out, _, exit, err := d.Runner.Run(ctx, []string{"sh", "-c", RunScript, "sh", work, "answers given", "./show"}, stdin)
	if err != nil || exit != 7 || string(out) != "--answers=K: \"v\"\n--given=g\n" {
		t.Fatalf("run: %q %d %v", out, exit, err)
	}
	got, _ = os.ReadFile(log)
	last := strings.Split(strings.TrimSpace(string(got)), "\n")
	if _, err := os.Stat(filepath.Dir(last[len(last)-1])); !os.IsNotExist(err) {
		t.Fatalf("the files' directory is left: %v", err)
	}

	// StowScript: no install directory is NotInstalled; the kept installer is run with --dir.
	d.Short = "nt"
	if s, err := d.Status(ctx); s != nil || err != nil {
		t.Fatalf("not installed: %+v %v", s, err)
	}
	d.Short = "nt"
	if err := os.Mkdir(filepath.Join(work, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Status(ctx); err == nil || !strings.Contains(err.Error(), "./notes/nt-stow: no installer kept there") {
		t.Fatalf("no kept installer: %v", err)
	}
	writeExe(t, filepath.Join(work, "notes", "other-stow"), "echo '{}'\n")
	writeExe(t, filepath.Join(work, "notes", "nt-stow"), `printf '{"format":1,"name":"notes","installed":{"version":"1.0","revision":1},"args":"%s"' "$*"
for a; do case $a in /*) printf ',"file":"%s"' "$(cat "$a")";; esac; done
echo '}'
`)
	s, err := d.Status(ctx)
	if err != nil || s.Installed.Version != "1.0" {
		t.Fatalf("status: %+v %v", s, err)
	}
	out, _, _, _ = d.stow(ctx, nil, true, "status", "--json")
	if !strings.Contains(string(out), `"args":"status --json --dir ./notes"`) {
		t.Fatalf("stow args: %s", out)
	}
	out, _, _, _ = d.stow(ctx, nil, false, "describe", "--json")
	if !strings.Contains(string(out), `"args":"describe --json"`) {
		t.Fatalf("describe takes no --dir: %s", out)
	}
	// A re-run's answers arrive as a file, after --dir.
	out, _, _, _ = d.stow(ctx, map[string]string{"answers": "K: v-SECRET"}, true, "--no-input")
	if !strings.Contains(string(out), `"args":"--no-input --dir ./notes --answers /`) || !strings.Contains(string(out), `"file":"K: v-SECRET"`) {
		t.Fatalf("stow files: %s", out)
	}
	d.Short = "../x"
	if _, err := d.Status(ctx); err == nil || !strings.Contains(err.Error(), "import the install again") {
		t.Fatalf("a short name out of the directory: %v", err)
	}
	// A short name changed since: the one *-stow kept runs; two are an error.
	d.Short = "renamed"
	if err := os.Remove(filepath.Join(work, "notes", "other-stow")); err != nil {
		t.Fatal(err)
	}
	if s, err := d.Status(ctx); err != nil || s.Installed.Version != "1.0" {
		t.Fatalf("the one kept installer: %+v %v", s, err)
	}
	writeExe(t, filepath.Join(work, "notes", "other-stow"), "true\n")
	if _, err := d.Status(ctx); err == nil || !strings.Contains(err.Error(), "more than one *-stow") {
		t.Fatalf("two kept installers: %v", err)
	}
}

func contains(list []string, s string) bool { return slices.Contains(list, s) }

func TestVerbArgv(t *testing.T) {
	t.Parallel()
	f := &recorder{}
	d := Docker{Runner: f, WorkDir: "/srv", Name: "notes", Short: "nt"}
	ctx := context.Background()
	c := Change{Inputs: map[string]string{"B": "", "A": "1"}, SecretInputs: map[string]string{"S": "x-SECRET"}, TLS: "acme",
		Domains: map[string]string{"web": "n.lan"}, Ports: map[string]string{"web": "8443"}}
	f.out = `{"format":1,"result":"done"}`
	if _, err := d.Rerun(ctx, c); err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "-c", StowScript, "sh", "/srv", "notes", "nt", "answers", "dir",
		"--no-input", "--json", "--set", "A=1", "--set", "B=", "--tls", "acme", "--domains", "web=n.lan", "--ports", "web=8443"}
	if !slices.Equal(f.argv, want) || untar(t, f.stdin)["answers"] != "S: \"x-SECRET\"\n" {
		t.Fatalf("re-run\n got %q\nwant %q", f.argv, want)
	}
	if _, err := d.Upgrade(ctx, Change{Inputs: map[string]string{"N": "v"}, Ports: map[string]string{"api": "9000"}}); err != nil {
		t.Fatal(err)
	}
	want = []string{"sh", "-c", RunScript, "sh", "/srv", "", "sh", "./notes.sh", "upgrade", "--no-input", "--json", "--dir", "./notes", "--set", "N=v", "--ports", "api=9000"}
	if !slices.Equal(f.argv, want) || f.stdin != nil {
		t.Fatalf("upgrade\n got %q\nwant %q", f.argv, want)
	}
	// --tls and --domains are a re-run's: an upgrade never passes them.
	if _, err := d.Upgrade(ctx, Change{TLS: "acme", Domains: map[string]string{"web": "n.lan"}}); err != nil || contains(f.argv, "--tls") || contains(f.argv, "--domains") {
		t.Fatalf("upgrade %q %v", f.argv, err)
	}
	if _, err := d.Rollback(ctx); err != nil || !slices.Equal(f.argv[9:], []string{"rollback", "--json"}) || f.argv[8] != "dir" {
		t.Fatalf("rollback %q %v", f.argv, err)
	}
	f.out = `{"format":1,"name":"notes","short_name":"nt","release":{"version":"1"},"routes":[]}`
	if desc, err := d.KeptDescribe(ctx); err != nil || desc.ShortName != "nt" || f.argv[8] != "" || !slices.Equal(f.argv[9:], []string{"describe", "--json"}) {
		t.Fatalf("kept describe %q %v", f.argv, err)
	}
	f.out, f.exit = "", 1
	if _, err := d.KeptDescribe(ctx); err == nil || !strings.Contains(err.Error(), "the installer the install keeps") {
		t.Fatalf("a failed describe: %v", err)
	}
	if (Change{}).Empty() != true || (Change{TLS: "acme"}).Empty() || (Change{Inputs: map[string]string{"A": ""}}).Empty() {
		t.Fatal("Empty")
	}
}

// recorder is a Runner keeping the last call and answering out with exit.
type recorder struct {
	argv   []string
	stdin  []byte
	out    string
	errOut string
	exit   int
}

func (r *recorder) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	r.argv, r.stdin = argv, stdin
	return []byte(r.out), []byte(r.errOut), r.exit, nil
}

// The mark of a missing install directory is the last line of stderr: ssh prints the server's
// banner before it.
func TestNotInstalledMark(t *testing.T) {
	t.Parallel()
	for stderr, want := range map[string]bool{
		NotInstalledMark + "\n":                                    true,
		"Authorized use only.\r\n\n" + NotInstalledMark + "\n":     true,
		NotInstalledMark + "\nsomething after\n":                   false,
		"stowage-provider: no install directory, said otherwise\n": false,
		"": false,
	} {
		if got := notInstalled(nil, []byte(stderr), NotInstalled); got != want {
			t.Errorf("%q: %v, want %v", stderr, got, want)
		}
	}
	if notInstalled([]byte("{}"), []byte(NotInstalledMark), NotInstalled) || notInstalled(nil, []byte(NotInstalledMark), 1) {
		t.Fatal("the mark with stdout, or another exit, taken for no install directory")
	}
}

// A create reads the install directory with the fetched installer first: a refusal saying absent
// is no install; another refusal is an error, never taken for none.
func TestFetchedStatus(t *testing.T) {
	t.Parallel()
	f := &recorder{exit: 3, out: `{"format":1,"refused":{"reason":"/srv/notes holds no install","fix":"x","absent":true}}`, errOut: "error: /srv/notes holds no install\n"}
	d := Docker{Runner: f, WorkDir: "/srv", Name: "notes"}
	ctx := context.Background()
	if s, err := d.FetchedStatus(ctx); s != nil || err != nil {
		t.Fatalf("no install: %+v %v", s, err)
	}
	want := []string{"sh", "-c", RunScript, "sh", "/srv", "", "sh", "./notes.sh", "status", "--json", "--dir", "./notes"}
	if !slices.Equal(f.argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", f.argv, want)
	}
	f.exit, f.out = 0, `{"format":1,"name":"notes","installed":{"version":"1.0","revision":1},"installing":true}`
	if s, err := d.FetchedStatus(ctx); err != nil || s == nil || !s.Installing {
		t.Fatalf("an install: %+v %v", s, err)
	}
	f.exit, f.out = 3, `{"format":1,"refused":{"reason":"the install's .env is not readable","fix":"x"}}`
	var refused *RefusedError
	if s, err := d.FetchedStatus(ctx); s != nil || !errors.As(err, &refused) || refused.Absent {
		t.Fatalf("another refusal: %+v %v", s, err)
	}
	f.exit, f.out = 1, ""
	if _, err := d.FetchedStatus(ctx); err == nil || !strings.Contains(err.Error(), "status failed (exit 1)") {
		t.Fatalf("a failure: %v", err)
	}
}

// A kept installer of Stowage 0.34.0 (no routes) is refused naming the hand upgrade that moves
// the install on.
func TestKeptDescribePre035(t *testing.T) {
	t.Parallel()
	f := &recorder{out: `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1"}}`}
	d := Docker{Runner: f, WorkDir: "/srv", Name: "notes", Short: "notes"}
	_, err := d.KeptDescribe(context.Background())
	if !errors.Is(err, ErrPreRoutes) || !strings.Contains(err.Error(), "`<short>-stow upgrade --version V --dir /srv/notes`, the `*-stow` in /srv/notes), then run OpenTofu again") {
		t.Fatalf("got %v", err)
	}
}

// Docker Hub's login goes under the key its client looks it up by; any other under its host. A
// login whose password file is empty stops the script.
func TestAuthKeyAndLoginGuards(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]string{"docker.io": "https://index.docker.io/v1/", "Index.Docker.io": "https://index.docker.io/v1/",
		"registry-1.docker.io": "registry-1.docker.io", "mirror.lan:5000": "mirror.lan:5000", "ghcr.io": "ghcr.io"} {
		if got := AuthKey(host); got != want {
			t.Errorf("%s: %q", host, got)
		}
	}
	if _, host, _, _, err := loginInput(nil, &Login{Host: "docker.io", Username: "u", Password: "p"}); err != nil || host != "https://index.docker.io/v1/" {
		t.Errorf("docker.io: %q %v", host, err)
	}
	dir := t.TempDir()
	stdin, _ := tarFiles(map[string]string{"x": "y"})
	_, stderr, exit, err := target.Local{}.Run(context.Background(), []string{"sh", "-c", RunLoginScript, "sh", dir, "", "mirror.lan", "u", "true"}, stdin)
	if err != nil || exit == 0 || !strings.Contains(string(stderr), "the login's password did not arrive") {
		t.Errorf("no password: %d %s %v", exit, stderr, err)
	}
}

// ResolveScript names a home reached through a symbolic link by the link's name, as $HOME does,
// even with no PWD (an SSH run's); another directory as cd names it.
func TestResolveScriptKeepsTheLinksName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real, link := filepath.Join(dir, "real"), filepath.Join(dir, "home")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	got, err := Docker{Runner: target.Local{}, WorkDir: link}.Resolve(context.Background())
	if err != nil || got != link {
		t.Fatalf("%q %v", got, err)
	}
	// "." in the home, as an SSH run starts: $HOME's name, though the process has no PWD.
	out, _, exit, err := target.Local{}.Run(context.Background(), []string{"env", "-u", "PWD", "HOME=" + link, "sh", "-c",
		`cd "$HOME" && unset PWD && sh -c "$0" sh .`, ResolveScript}, nil)
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != link {
		t.Fatalf("the home: %q %d %v", out, exit, err)
	}
}

// PlanDirScript and PlanRemoveScript under sh: a private directory beside the install, its
// absolute path printed; removed, and nothing but such a directory is.
func TestPlanDirScripts(t *testing.T) {
	t.Parallel()
	work := t.TempDir()
	d := Docker{Runner: target.Local{}, WorkDir: work, Name: "notes"}
	dir, err := d.PlanDir(context.Background())
	if err != nil || filepath.Dir(dir) != work || !strings.HasPrefix(filepath.Base(dir), ".stowage-plan.") {
		t.Fatalf("%q %v", dir, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	if err := d.RemovePlanDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("left: %v", err)
	}
	if err := d.RemovePlanDir(context.Background(), work); err == nil {
		t.Fatal("another directory removed")
	}
	if _, err := os.Stat(work); err != nil {
		t.Fatalf("the work directory: %v", err)
	}
}

// ResolveScript takes $HOME only when it is an absolute directory the run is in: a missing or
// relative $HOME, or another directory, falls back to cd and pwd -L; Resolve cleans the spelling.
func TestResolveScriptHomeGuards(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := func(home string) string {
		out, _, _, _ := target.Local{}.Run(context.Background(), []string{"env", "HOME=" + home, "sh", "-c", `cd "$0" && unset PWD && sh -c "$1" sh .`, dir, ResolveScript}, nil)
		return strings.TrimSpace(string(out))
	}
	for home, want := range map[string]string{dir: dir, dir + "/": dir + "/", ".": dir, dir + "/missing": dir, "/": dir} {
		if got := run(home); got != want && !(want == dir && strings.HasSuffix(got, filepath.Base(dir))) {
			t.Errorf("HOME=%q: %q, want %q", home, got, want)
		}
	}
	got, err := Docker{Runner: &recorder{out: dir + "//\n"}, WorkDir: "."}.Resolve(context.Background())
	if err != nil || got != dir {
		t.Fatalf("cleaned: %q %v", got, err)
	}
}
