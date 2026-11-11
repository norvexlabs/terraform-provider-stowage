package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
	"github.com/norvexlabs/terraform-provider-stowage/internal/testutil"
)

type oneReply struct {
	stdout, stderr string
	exit           int
	argv           []string
}

func (o *oneReply) Run(_ context.Context, argv []string, _ []byte) ([]byte, []byte, int, error) {
	o.argv = argv
	return []byte(o.stdout), []byte(o.stderr), o.exit, nil
}

func TestGivenFile(t *testing.T) {
	t.Parallel()
	got, err := GivenFile("notes", map[string]string{"B": "say \"hi\"\nthere", "A": ""})
	if err != nil || got != "project: \"notes\"\ninputs:\n  A: \"\"\n  B: \"say \\\"hi\\\"\\nthere\"\n" {
		t.Fatalf("%q %v", got, err)
	}
	for _, bad := range []map[string]string{nil, {"A=B": "x"}, {"tls": "x"}} {
		if _, err := GivenFile("notes", bad); err == nil {
			t.Errorf("%v: no error", bad)
		}
	}
}

func TestK8sStatusNoRelease(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		k        K8s
		r        oneReply
		gone     bool
		hasError bool
	}{
		"no release (absent)": {K8s{Name: "notes"}, oneReply{exit: 3, stdout: `{"format":1,"refused":{"reason":"no release notes in notes","fix":"notes-stow k8s install","absent":true}}`, stderr: "error: no release notes in notes\n"}, true, false},
		"another refusal":     {K8s{Name: "notes"}, oneReply{exit: 3, stdout: `{"format":1,"refused":{"reason":"Secret notes-stowage was written by another tool","fix":""}}`}, false, true},
		"no refusal object":   {K8s{Name: "notes"}, oneReply{exit: 3, stderr: "error: no release notes in notes: …\n"}, false, true},
		"a malformed object":  {K8s{Name: "notes"}, oneReply{exit: 3, stdout: "{}"}, false, true},
		"an unknown format":   {K8s{Name: "notes"}, oneReply{exit: 3, stdout: `{"format":2,"refused":{"reason":"x","absent":true}}`}, false, true},
	} {
		r := c.r
		c.k.Runner = &r
		st, err := c.k.Status(context.Background())
		if (st == nil && err == nil) != c.gone || (err != nil) != c.hasError {
			t.Errorf("%s: %v %v", name, st, err)
		}
	}
	k := K8s{Runner: &oneReply{}, Name: "notes", Namespace: "prod", Context: "kind-a", Dir: "/run", Kubeconfig: "/run/kubeconfig"}
	_, _ = k.Status(context.Background())
	argv := k.Runner.(*oneReply).argv
	if got := strings.Join(argv[4:], " "); got != "/run /run/kubeconfig    sh ./notes.sh k8s status --namespace prod --kube-context kind-a --json" {
		t.Fatalf("argv %q", got)
	}
}

func TestK8sRefusesTheFrontsSettings(t *testing.T) {
	t.Parallel()
	k := K8s{Runner: &oneReply{}, Name: "notes"}
	for name, run := range map[string]func() error{
		"install tls":     func() error { _, err := k.Install(context.Background(), Install{TLS: "acme"}); return err },
		"install address": func() error { _, err := k.Install(context.Background(), Install{Address: "127.0.0.2"}); return err },
		"install secrets": func() error { _, err := k.Install(context.Background(), Install{Secrets: "x"}); return err },
		"upgrade ports": func() error {
			_, err := k.Upgrade(context.Background(), Change{Ports: map[string]string{"web": "80"}}, "")
			return err
		},
		"reconfigure tls":   func() error { _, err := k.Reconfigure(context.Background(), Change{TLS: "acme"}, ""); return err },
		"reconfigure empty": func() error { _, err := k.Reconfigure(context.Background(), Change{}, ""); return err },
	} {
		if err := run(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if k.Runner.(*oneReply).argv != nil {
		t.Fatal("the installer ran")
	}
}

// K8sScript under a real sh: the program runs in the directory, with KUBECONFIG the file named
// (left as the environment has it when none is), and each file from standard input passed by
// its flag.
func TestK8sScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stdin, err := tarFiles(map[string]string{"given": "project: notes\n"})
	if err != nil {
		t.Fatal(err)
	}
	prog := []string{"sh", "-c", `printf '%s|%s|' "$PWD" "${KUBECONFIG-unset}"; cat "$2"`, "prog"}
	out, stderr, exit, err := target.Local{}.Run(context.Background(), append([]string{"sh", "-c", K8sScript, "sh", dir, "/k/config", "given", "", ""}, prog...), stdin)
	if err != nil || exit != 0 {
		t.Fatalf("%v %d %s", err, exit, stderr)
	}
	if want := dir + "|/k/config|project: notes\n"; string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	out, _, _, _ = target.Local{}.Run(context.Background(), append([]string{"sh", "-c", K8sScript, "sh", dir, "", "", "", ""}, "sh", "-c", `printf '%s' "${KUBECONFIG-unset}"`), nil)
	if string(out) != "unset" {
		t.Fatalf("no kubeconfig given: KUBECONFIG %q", out)
	}
}

// With a login, K8sScript runs the program with a Docker configuration of its own holding it,
// the password read from the tar, never an argument; the configuration keeps the Docker context
// in use and its contexts, and is gone after the run.
func TestK8sScriptLogin(t *testing.T) {
	t.Parallel()
	dir, bin, home := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".docker", "contexts", "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\ncase $1 in\ncontext) echo rootless ;;\n*) echo \"$*\" >> " + filepath.Join(dir, "log") + " ;;\nesac\n"
	testutil.WriteExecutable(t, filepath.Join(bin, "docker"), fake)
	stdin, err := tarFiles(map[string]string{"given": "g", "login-password": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	prog := []string{"sh", "-c", `cat "$DOCKER_CONFIG/config.json"; ls "$DOCKER_CONFIG/contexts"; printf '|%s' "$*"; echo "$DOCKER_CONFIG" > dc`, "prog"}
	argv := append([]string{"env", "-u", "DOCKER_CONFIG", "PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "sh", "-c", K8sScript, "sh", dir, "", "given", "mirror.lan", "puller"}, prog...)
	out, stderr, exit, err := target.Local{}.Run(context.Background(), argv, stdin)
	if err != nil || exit != 0 {
		t.Fatalf("%v %d %s", err, exit, stderr)
	}
	want := `{"auths":{"mirror.lan":{"auth":"cHVsbGVyOnMzY3JldA=="}},"currentContext":"rootless"}` + "\nmeta\n|--given "
	if !strings.HasPrefix(string(out), want) || strings.Contains(string(out), "s3cret") || strings.Contains(string(out), "login-password") {
		t.Fatalf("got %q, want %q…", out, want)
	}
	if log, _ := os.ReadFile(filepath.Join(dir, "log")); len(log) != 0 {
		t.Fatalf("docker called beyond context show: %q", log)
	}
	dc, _ := os.ReadFile(filepath.Join(dir, "dc"))
	if _, err := os.Stat(strings.TrimSpace(string(dc))); !os.IsNotExist(err) {
		t.Fatalf("the login's configuration %s is left: %v", dc, err)
	}
	if _, _, _, _, err := loginInput(nil, &Login{Host: `a"b`, Username: "u", Password: "p"}); err == nil {
		t.Fatal("a host that is no registry host is taken")
	}
}

// Without leaves out the inputs' names but those the run needs itself, on top of HelmEnv.
func TestK8sWithout(t *testing.T) {
	t.Parallel()
	k := K8s{Runner: Filtered(target.Local{})}.Without(&Describe{Inputs: []Input{{Key: "ADMIN_EMAIL"}, {Key: "PATH"}, {Key: "HOME"}, {Key: "DOCKER_HOST"}, {Key: "XDG_CACHE_HOME"}, {Key: "SMTP_PASSWORD"}}})
	unset := k.Runner.(target.Local).Unset
	if got := strings.Join(unset[len(HelmEnv):], " "); got != "ADMIN_EMAIL SMTP_PASSWORD" || unset[0] != "HELM_DRIVER" {
		t.Fatalf("unset %q", unset)
	}
}

// A Docker install on this machine leaves the releases' input keys out of the installer's
// environment; a remote one's runner is left as it is.
func TestDockerWithout(t *testing.T) {
	t.Parallel()
	a := &Describe{Inputs: []Input{{Key: "ADMIN_EMAIL"}, {Key: "PATH"}}}
	b := &Describe{Inputs: []Input{{Key: "NEW_TOKEN"}}}
	d := Docker{Runner: target.Local{}}.Without(a, nil, b)
	if l := d.Runner.(target.Local); strings.Join(l.Unset, " ") != "ADMIN_EMAIL NEW_TOKEN" || !d.Local() {
		t.Fatalf("unset %q", l.Unset)
	}
	remote := &oneReply{}
	if d := (Docker{Runner: remote}).Without(a); d.Runner != remote || d.Local() {
		t.Fatal("a remote runner was changed")
	}
}

func TestGivenKeys(t *testing.T) {
	t.Parallel()
	for given, want := range map[string]string{
		"project: notes\ninputs:\n  LANG: x\n  \"HOME\": y\n  ADMIN_EMAIL: z\n  DOCKER_HOST: w\nPATH: top-level\n": "DOCKER_HOST HOME LANG",
		"project: x\ninputs: {HOME: /srv, 'KUBECONFIG': /k, ADMIN_EMAIL: a}\n":                                     "HOME KUBECONFIG",
		"project: x\ninputs: {\"\\u0048OME\": /data}\n":                                                            "HOME",
		"project: x\ninputs:\n  APP_CONF: |\n    HOME: /srv\n  NOTE: \"see PATH: the search path\"\n":              "",
		"not: [yaml": "",
	} {
		if got := strings.Join(GivenKeys(given), " "); got != want {
			t.Errorf("%q: %q, want %q", given, got, want)
		}
	}
}

// A Kubernetes run leaves HelmEnv and the input keys out of its environment through the runner,
// before its shell starts; an exported variable named as a script's word (files) is not an
// input, and passes unchanged.
func TestK8sRunEnvironment(t *testing.T) {
	t.Setenv("HELM_DRIVER", "memory")
	t.Setenv("files", "from-the-environment")
	t.Setenv("ADMIN_EMAIL", "env@example.com")
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/notes.sh", []byte(`printf '%s|%s|%s' "${HELM_DRIVER-unset}" "${files-unset}" "${ADMIN_EMAIL-unset}"`), 0o644); err != nil {
		t.Fatal(err)
	}
	k := K8s{Runner: Filtered(target.Local{}), Dir: dir, Name: "notes"}.Without(&Describe{Inputs: []Input{{Key: "ADMIN_EMAIL"}}})
	out, stderr, exit, err := k.run(context.Background(), nil)
	if err != nil || exit != 0 || string(out) != "unset|from-the-environment|unset" {
		t.Fatalf("%q %d %v %s", out, exit, err, stderr)
	}
}

func TestLocalWithoutEnvAdds(t *testing.T) {
	t.Parallel()
	r := target.Local{}.WithoutEnv([]string{"A"}).(target.EnvFilter).WithoutEnv([]string{"B"}).(target.Local)
	if strings.Join(r.Unset, " ") != "A B" {
		t.Fatalf("%q", r.Unset)
	}
}

// A relative DOCKER_CONFIG names a directory of the directory the script was started in, not of
// the one it moves to: its contexts are the login's.
func TestLoginKeepsARelativeDockerConfigsContexts(t *testing.T) {
	t.Parallel()
	start, dir, bin := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(start, "cfg", "contexts", "meta"), 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.WriteExecutable(t, filepath.Join(bin, "docker"), "#!/bin/sh\necho rootless\n")
	stdin, _ := tarFiles(map[string]string{"login-password": "pw"})
	script := `cd "$1" && shift && exec env DOCKER_CONFIG=cfg PATH="$0:/usr/bin:/bin" sh -c "$@"`
	out, stderr, exit, err := target.Local{}.Run(context.Background(), []string{"sh", "-c", script, bin, start,
		`exec sh -c "$0" sh "$@"`, RunLoginScript, dir, "", "mirror.lan", "u", "sh", "-c", `ls "$DOCKER_CONFIG/contexts"`}, stdin)
	if err != nil || exit != 0 || strings.TrimSpace(string(out)) != "meta" {
		t.Fatalf("%q %s %d %v", out, stderr, exit, err)
	}
}
