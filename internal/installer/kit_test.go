package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-log/tflogtest"
)

const issued = `{"kit":"-----BEGIN AGE ENCRYPTED FILE-----\nKIT-s3cr3t\n","code":"CODE-s3cr3t","log":""}`

var kitReq = KitRequest{Instance: "site-2", Answers: map[string]string{"SITE": "x", "TOKEN": "answer-s3cr3t"}, Expires: 3600e9}

// noSecret fails when s holds the request's secret answer, the kit or its code.
func noSecret(t *testing.T, what, s string) {
	t.Helper()
	for _, secret := range []string{"answer-s3cr3t", "KIT-s3cr3t", "CODE-s3cr3t"} {
		if strings.Contains(s, secret) {
			t.Errorf("%s holds %s: %s", what, secret, s)
		}
	}
}

func TestDockerKitCreate(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{stdout: issued}
	d := Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}
	k, err := d.KitCreate(context.Background(), "client", kitReq)
	if err != nil || k.Kit != "-----BEGIN AGE ENCRYPTED FILE-----\nKIT-s3cr3t\n" || k.Code != "CODE-s3cr3t" {
		t.Fatalf("%+v %v", k, err)
	}
	noSecret(t, "the command line", strings.Join(r.argv, " "))
	if r.argv[2] != StowScript || !slices.Equal(r.argv[4:], []string{"/srv", "notes", "notes", "request", "dir", "kit", "create", "client", "--json"}) {
		t.Errorf("argv %q", r.argv[3:])
	}
	var got KitRequest
	if err := json.Unmarshal([]byte(untar(t, r.stdin)["request"]), &got); err != nil || got.Instance != "site-2" || got.Answers["TOKEN"] != "answer-s3cr3t" || got.Expires != 3600e9 {
		t.Errorf("request %+v %v", got, err)
	}
}

func TestK8sKitCreate(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{stdout: issued}
	k := K8s{Runner: r, Dir: "/op", Name: "notes", Namespace: "ns", Release: "rel", Context: "ctx"}
	if _, err := k.KitCreate(context.Background(), "client", KitRequest{Instance: "i"}, "CA-PEM"); err != nil {
		t.Fatal(err)
	}
	if r.argv[2] != K8sScript || r.argv[6] != "registry-ca request" ||
		!slices.Equal(r.argv[9:], []string{"sh", "./notes.sh", "k8s", "kit", "create", "client", "--json", "--namespace", "ns", "--release", "rel", "--kube-context", "ctx"}) {
		t.Errorf("argv %q", r.argv[4:])
	}
	files := untar(t, r.stdin)
	if files["registry-ca"] != "CA-PEM" || files["request"] != `{"instance":"i","answers":{}}` {
		t.Errorf("files %q", files)
	}
}

// The VM's request goes on standard input, as the VM's kit create --json has always read it; its
// answer, the kit and its code, is never logged.
func TestVMKitCreateNeverLogged(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &buf)
	r := &vmRecorder{stdout: issued, stderr: "the run's log"}
	k, err := VM{Runner: r, Short: "notes"}.KitCreate(ctx, "client", kitReq)
	if err != nil || k.Code != "CODE-s3cr3t" {
		t.Fatalf("%+v %v", k, err)
	}
	if !slices.Equal(r.argv, []string{"sudo", "-n", "notes-stow", "kit", "create", "client", "--json"}) {
		t.Errorf("argv %q", r.argv)
	}
	var got KitRequest
	if err := json.Unmarshal(r.stdin, &got); err != nil || got.Answers["TOKEN"] != "answer-s3cr3t" {
		t.Errorf("stdin %q %v", r.stdin, err)
	}
	noSecret(t, "the log", buf.String())
}

// A kit create that fails says what it may have issued is lost, unless the installer refused a
// flag (one made before KitMinStowage); no error quotes the answer.
func TestKitCreateFailures(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		r    vmRecorder
		lost bool
		want string
	}{
		"no code":       {r: vmRecorder{stdout: `{"kit":"KIT-s3cr3t"}`}, lost: true, want: "holds no kit or no code"},
		"two objects":   {r: vmRecorder{stdout: issued + issued}, lost: true, want: "does not read as a kit"},
		"not JSON":      {r: vmRecorder{stdout: "kit KIT-s3cr3t code CODE-s3cr3t"}, lost: true, want: "not one JSON object"},
		"wrong type":    {r: vmRecorder{stdout: `{"kit":"KIT-s3cr3t","code":7}`}, lost: true, want: "does not read as a kit"},
		"run failed":    {r: vmRecorder{exit: 1, stderr: "the handoff's run did not finish"}, lost: true, want: "did not finish"},
		"old installer": {r: vmRecorder{exit: 1, stderr: "Error: unknown flag: --request"}, want: "Stowage " + KitMinStowage + " or later"},
		"refused":       {r: vmRecorder{exit: 3, stderr: "error: the Job notes-stowage-kit-client is there"}, want: "refused before the run: nothing was issued"},
		"old on the VM": {r: vmRecorder{exit: 1, stderr: "Error: unknown flag: --json"}, want: "Stowage " + KitMinStowage + " or later"},
	} {
		r := c.r
		_, err := Docker{Runner: &r, WorkDir: "/srv", Name: "notes", Short: "notes"}.KitCreate(context.Background(), "client", kitReq)
		if err == nil || !strings.Contains(err.Error(), c.want) || errors.Is(err, ErrKitLost) != c.lost {
			t.Errorf("%s: %v (lost %v)", name, err, c.lost)
		}
		if err != nil {
			noSecret(t, name, err.Error())
		}
	}
}

// kit check's exit 0 and 1 are a verdict only with the line saying it: sudo, ssh or the script
// failing exit 1 too, and are no answer.
func TestKitVerdict(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		stdout string
		exit   int
		want   KitCheck
		err    bool
	}{
		{"client site-2: proven\n", 0, KitProven, false},
		{"", 0, KitUnknown, true},
		{"client site-3: proven\n", 0, KitUnknown, true},
		{"client site-2: not proven (its check, or starting it, exited 1)\n", 1, KitNotProven, false},
		{"", 1, KitUnknown, true},
		{"client site-2: proven\n", 1, KitUnknown, true},
		{"client site-2: unknown\n", 2, KitUnknown, false},
		{"", 2, KitUnknown, false},
		{"", 255, KitUnknown, true},
		{"client site-2: not proven (its check, or starting it, exited 7)\n", 7, KitUnknown, true},
	} {
		got, err := kitVerdict("kit check", "client", "site-2", []byte(c.stdout), []byte("sudo: a password is required"), c.exit)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%q exit %d: %v %v", c.stdout, c.exit, got, err)
		}
	}
}

func TestKitCheckArgv(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &vmRecorder{stdout: "client site-2: proven\n"}
	if v, err := (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).KitCheck(ctx, "client", "site-2"); err != nil || v != KitProven ||
		!slices.Equal(r.argv[4:], []string{"/srv", "notes", "notes", "", "dir", "kit", "check", "client", "site-2"}) {
		t.Errorf("docker: %v %v %q", v, err, r.argv)
	}
	if v, err := (K8s{Runner: r, Dir: "/op", Name: "notes", Namespace: "ns"}).KitCheck(ctx, "client", "site-2", ""); err != nil || v != KitProven ||
		!slices.Equal(r.argv[9:], []string{"sh", "./notes.sh", "k8s", "kit", "check", "client", "site-2", "--namespace", "ns"}) {
		t.Errorf("k8s: %v %v %q", v, err, r.argv)
	}
	if v, err := (VM{Runner: r, Short: "notes"}).KitCheck(ctx, "client", "site-2"); err != nil || v != KitProven ||
		!slices.Equal(r.argv, []string{"sudo", "-n", "notes-stow", "kit", "check", "client", "site-2"}) {
		t.Errorf("vm: %v %v %q", v, err, r.argv)
	}
}

func TestDescribeHandoffs(t *testing.T) {
	t.Parallel()
	d, err := ParseDescribe([]byte(`{"format":1,"name":"notes","short_name":"notes","release":{"version":"1.0.0","revision":1},
"shapes":["docker"],"inputs":[],"tls_inputs":[],"first_install_only":[],"routes":[],"l4_routes":[],
"handoffs":[{"name":"client","for":"notes","instance":"","ask":[{"key":"T","prompt":"p","secret":true}],"gives":["A"],"check":true}]}`))
	if err != nil {
		t.Fatal(err)
	}
	h := d.Handoff("client")
	if h == nil || h.For != "notes" || !h.Check || len(h.Ask) != 1 || !h.Ask[0].Secret || !slices.Equal(h.Gives, []string{"A"}) || d.Handoff("other") != nil {
		t.Errorf("%+v", d.Handoffs)
	}
}

// A kit is taken by each shape's re-run, the kit and its code files on standard input; one
// without its code is refused before anything runs.
func TestTakeKit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	done := `{"format":1,"result":"done","release":{"version":"1.0.0","revision":1}}`
	r := &vmRecorder{stdout: done}
	if _, err := (Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}).TakeKit(ctx, "KIT-s3cr3t", "CODE-s3cr3t"); err != nil ||
		r.argv[2] != StowScript || !slices.Equal(r.argv[4:], []string{"/srv", "notes", "notes", "kit kit-code-file", "dir", "--no-input", "--json"}) {
		t.Errorf("docker: %v %q", err, r.argv)
	}
	if f := untar(t, r.stdin); f["kit"] != "KIT-s3cr3t" || f["kit-code-file"] != "CODE-s3cr3t" {
		t.Errorf("docker files %q", f)
	}
	noSecret(t, "docker's command line", strings.Join(r.argv, " "))
	if _, err := (K8s{Runner: r, Dir: "/op", Name: "notes", Namespace: "ns"}).TakeKit(ctx, "KIT-s3cr3t", "CODE-s3cr3t", "CA-PEM"); err != nil ||
		r.argv[6] != "kit kit-code-file registry-ca" || !slices.Equal(r.argv[9:], []string{"sh", "./notes.sh", "k8s", "upgrade", "--namespace", "ns", "--no-input", "--json"}) {
		t.Errorf("k8s: %v %q", err, r.argv)
	}
	noSecret(t, "k8s's command line", strings.Join(r.argv, " "))
	if _, err := (VM{Runner: r, Short: "notes"}).TakeKit(ctx, "KIT-s3cr3t", "CODE-s3cr3t"); err != nil ||
		r.argv[2] != RunScript || !slices.Equal(r.argv[5:], []string{"kit kit-code-file", "sudo", "-n", "notes-stow", "install", "--no-input", "--json"}) {
		t.Errorf("vm: %v %q", err, r.argv)
	}
	noSecret(t, "the VM's command line", strings.Join(r.argv, " "))
	r.argv = nil
	for _, take := range []func() error{
		func() error { _, err := (Docker{Runner: r, Short: "notes"}).TakeKit(ctx, "KIT", ""); return err },
		func() error { _, err := (K8s{Runner: r}).TakeKit(ctx, "", "CODE", ""); return err },
		func() error { _, err := (VM{Runner: r, Short: "notes"}).TakeKit(ctx, "KIT", ""); return err },
		func() error { _, err := (Docker{Runner: r, Short: "notes"}).TakeKit(ctx, "", ""); return err },
	} {
		if err := take(); err == nil || r.argv != nil {
			t.Errorf("a kit without its code: %v %q", err, r.argv)
		}
	}
}
