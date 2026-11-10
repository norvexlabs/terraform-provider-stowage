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
	d, err := ParseDescribe([]byte(`{"format":1,"name":"notes","release":{"version":"1.2.0","revision":2},"channel":"stable",
		"shapes":["docker"],"inputs":[{"key":"ADMIN_EMAIL","required":true,"default":"a@b"}],"a_later_field":{"x":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "notes" || d.Release.Version != "1.2.0" || d.Release.Revision != 2 || d.Inputs[0].Key != "ADMIN_EMAIL" {
		t.Fatalf("got %+v", d)
	}
	for doc, want := range map[string]string{
		`{"name":"notes","release":{"version":"1"}}`:               "predates the machine interface; use an installer made by Stowage " + MinStowage,
		`{"format":2,"name":"notes","release":{"version":"1"}}`:    "format 2 is newer than this provider reads (format 1): upgrade terraform-provider-stowage",
		`{"format":0,"name":"notes","release":{"version":"1"}}`:    "format 0 is unknown",
		`{"format":1,"name":"notes","release":{"version":"1"}} {}`: "data after",
		`{"format":"1","name":"notes"}`:                            "cannot unmarshal",
		`{"format":1,"release":{"version":"1"}}`:                   "no name",
		`{"format":1,"name":"notes","release":{}}`:                 "release has no version",
	} {
		_, err := ParseDescribe([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()
	s, err := ParseStatus([]byte(`{"name":"notes","installed":{"version":"1.2.0","revision":1,"installer":"r/notes-installer@` + digest + `"},
		"channel":"stable","previous":null,"inputs":["B","A","A"],"services":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if s.Installed.Installer == "" || s.Previous != nil || !slices.Equal(s.Inputs, []string{"A", "B"}) {
		t.Fatalf("got %+v", s)
	}
	s, err = ParseStatus([]byte(`{"format":1,"name":"notes","installed":null,"installing":true,"inputs":{"Z":{},"Y":true}}`))
	if err != nil || s.Installed != nil || !s.Installing || !slices.Equal(s.Inputs, []string{"Y", "Z"}) {
		t.Fatalf("got %+v %v", s, err)
	}
	for doc, want := range map[string]string{
		`{"format":2,"name":"notes"}`:                "format 2 is newer",
		`{"name":"notes","previous":{"revision":1}}`: "previous has no version",
		`{"name":"notes","inputs":"A"}`:              "inputs: want an array",
		`{"name":"notes","installing":"yes"}`:        "cannot unmarshal",
		`{"installed":{"version":"1"}}`:              "no name",
	} {
		_, err := ParseStatus([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

func TestOutcome(t *testing.T) {
	t.Parallel()
	r, err := Outcome("install", []byte("log line\n{\"format\":1,\"result\":\"done\",\"release\":{\"version\":\"1.0\",\"revision\":1}}\n"), nil, 0)
	if err != nil || r.Result != "done" || r.Release.Version != "1.0" {
		t.Fatalf("done: %+v %v", r, err)
	}
	if r, err := Outcome("upgrade", []byte(`{"format":1,"result":"nothing"}`), nil, 0); err != nil || r.Result != "nothing" {
		t.Fatalf("nothing: %+v %v", r, err)
	}

	_, err = Outcome("install", []byte(`{"format":1,"refused":{"reason":"ADMIN_EMAIL is recorded","fix":"run upgrade"}}`), []byte("x"), 3)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Fix != "run upgrade" || !strings.Contains(err.Error(), "install refused: ADMIN_EMAIL is recorded\nFix: run upgrade") {
		t.Fatalf("refused: %v", err)
	}

	var failed *FailedError
	for name, c := range map[string]struct {
		out  string
		exit int
		want string
	}{
		"exit 1":           {"", 1, "install failed (exit 1)\nboom"},
		"exit 3, no fix":   {`{"format":1,"result":"done"}`, 3, "exit 3 without a refusal"},
		"exit 3, no json":  {"text", 3, "no JSON object"},
		"exit 0, bad word": {`{"format":1,"result":"maybe"}`, 0, `result "maybe" is neither`},
		"exit 0, no json":  {"", 0, "no JSON object"},
		"exit 255":         {"", 255, "ssh's own failure"},
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
login) echo "login $* pw=$(cat) cfg=$DOCKER_CONFIG" >> `+log+` ;;
run) echo "run $*" >> `+log+`; echo 'echo bootstrap "$@"' ;;
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
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "login login --username pull --password-stdin registry.example.com pw=pw-SECRET cfg=") || lines[1] != "run run --rm "+img {
		t.Fatalf("docker calls %q", lines)
	}
	cfg := strings.TrimPrefix(lines[0][strings.Index(lines[0], "cfg="):], "cfg=")
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

	// StowScript: no install directory is NotInstalled; one installer is run with --dir.
	if s, err := d.Status(ctx); s != nil || err != nil {
		t.Fatalf("not installed: %+v %v", s, err)
	}
	writeExe(t, filepath.Join(work, "notes", "notes-stow"), `echo '{"name":"notes","installed":{"version":"1.0","revision":1},"args":"'"$*"'"}'
`)
	s, err := d.Status(ctx)
	if err != nil || s.Installed.Version != "1.0" {
		t.Fatalf("status: %+v %v", s, err)
	}
	out, _, _, _ = d.Runner.Run(ctx, []string{"sh", "-c", StowScript, "sh", work, "notes", "status", "--json"}, nil)
	if !strings.Contains(string(out), `"args":"status --json --dir ./notes"`) {
		t.Fatalf("stow args: %s", out)
	}
	writeExe(t, filepath.Join(work, "notes", "other-stow"), "true\n")
	if _, err := d.Status(ctx); err == nil || !strings.Contains(err.Error(), "more than one *-stow") {
		t.Fatalf("two installers: %v", err)
	}
}
