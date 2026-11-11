package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

const edKey = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

// vmRecorder is a Runner answering every run with one reply, recording the last call.
type vmRecorder struct {
	argv   []string
	stdin  []byte
	stdout string
	stderr string
	exit   int
}

func (r *vmRecorder) Run(_ context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	r.argv, r.stdin = argv, stdin
	return []byte(r.stdout), []byte(r.stderr), r.exit, nil
}

const vmStatus = `{"format":1,"name":"notes","os":{"version":"1.1.0","revision":1},"installed":{"version":"1.1.0","revision":1},
"previous":{"version":"1.0.0","revision":1},"inputs":["ADMIN_EMAIL"],"channel":"stable","boot":"active","on_trial":false,
"failed_boot":{"release":{"version":"1.2.0","revision":1},"at":"2026-11-10T10:00:00Z","reason":"api not ready","dir":"/x"},
"reboot_pending":false,"boot_id":"b1","job":"idle","events":3,"disk":{"total":1}}`

func TestParseVMStatus(t *testing.T) {
	t.Parallel()
	st, err := ParseVMStatus([]byte(vmStatus))
	if err != nil {
		t.Fatal(err)
	}
	if st.Installed.Version != "1.1.0" || st.Previous.Version != "1.0.0" || st.BootID != "b1" || st.Boot != "active" ||
		st.FailedBoot == nil || st.FailedBoot.Reason != "api not ready" || st.OS.Version != "1.1.0" || st.Inputs == nil || (*st.Inputs)[0] != "ADMIN_EMAIL" {
		t.Errorf("parsed %+v", st)
	}
	for name, c := range map[string]struct{ in, want string }{
		"no format":          {`{"name":"notes"}`, "no format"},
		"no name":            {`{"format":1}`, "no name"},
		"trailing data":      {`{"format":1,"name":"n"} {}`, "data after"},
		"failed boot no v":   {`{"format":1,"name":"n","failed_boot":{"release":{}}}`, "failed_boot.release has no version"},
		"os without version": {`{"format":1,"name":"n","os":{}}`, "os has no version"},
		"on_trial a string":  {`{"format":1,"name":"n","on_trial":"yes"}`, "cannot unmarshal"},
	} {
		if _, err := ParseVMStatus([]byte(c.in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	a, b := &FailedBoot{Reason: "x"}, &FailedBoot{Reason: "x"}
	if !a.Same(b) || a.Same(nil) || !(*FailedBoot)(nil).Same(nil) || a.Same(&FailedBoot{Reason: "y"}) {
		t.Error("Same")
	}
}

func TestVMVerbs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &vmRecorder{stdout: vmStatus}
	v := VM{Runner: r, Short: "notes"}
	if _, err := v.Status(ctx); err != nil || !slices.Equal(r.argv, []string{"notes-stow", "status", "--json"}) {
		t.Errorf("status: %v %q", err, r.argv)
	}
	r.stdout = `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`
	if _, err := v.Upgrade(ctx, "1.2.0"); err != nil || !slices.Equal(r.argv, []string{"sudo", "-n", "notes-stow", "upgrade", "--version", "1.2.0", "--yes", "--json"}) {
		t.Errorf("upgrade: %v %q", err, r.argv)
	}
	if _, err := v.Rollback(ctx); err != nil || !slices.Equal(r.argv, []string{"sudo", "-n", "notes-stow", "rollback", "--yes", "--json"}) {
		t.Errorf("rollback: %v %q", err, r.argv)
	}
	for _, bad := range []string{"", "-x", "1 2"} {
		if _, err := v.Upgrade(ctx, bad); err == nil {
			t.Errorf("upgrade --version %q is taken", bad)
		}
	}
	// ssh's own failure with no result: the reboot may have cut it.
	r.stdout, r.stderr, r.exit = "", "Connection closed by remote host", 255
	if _, err := v.Upgrade(ctx, "1.2.0"); !errors.Is(err, ErrCut) {
		t.Errorf("a cut upgrade: %v", err)
	}
	// A refusal stays one.
	r.stdout, r.exit = `{"format":1,"result":"nothing","refused":{"reason":"older","fix":"none"}}`, 3
	var refused *RefusedError
	if _, err := v.Upgrade(ctx, "0.9.0"); !errors.As(err, &refused) {
		t.Errorf("a refused upgrade: %v", err)
	}
	if _, err := (VM{Runner: r, Short: "../x"}).Status(ctx); err == nil || !strings.Contains(err.Error(), "short_name") {
		t.Errorf("a bad short name: %v", err)
	}
	r.stdout, r.exit = "ssh-ed25519 "+edKey+" root@vm\n", 0
	if k, err := v.HostKey(ctx); err != nil || k != "ssh-ed25519 "+edKey {
		t.Errorf("host key: %q %v", k, err)
	}
	r.stdout = "ssh-rsa AAAA\n"
	if _, err := v.HostKey(ctx); err == nil {
		t.Error("an rsa key is taken for the ed25519 one")
	}
}

// TestVMInstallKeepsSecretsOffTheCommandLine: the secret inputs, the given material and the
// export reach the VM on standard input alone.
func TestVMInstallKeepsSecretsOffTheCommandLine(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{stdout: `{"format":1,"result":"done","release":{"version":"1.1.0","revision":1}}`}
	v := VM{Runner: r, Short: "notes"}
	_, err := v.Install(context.Background(), VMInstall{
		Change: Change{Inputs: map[string]string{"ADMIN_EMAIL": "a@b"}, SecretInputs: map[string]string{"API_KEY": "s3cr3t"}, TLS: "internal"},
		Given:  "project: notes\ninputs: {X: given-s3cr3t}\n", Secrets: "export-s3cr3t",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(r.argv, " "), "s3cr3t") {
		t.Fatalf("a secret on the command line: %q", r.argv)
	}
	if r.argv[2] != RunScript || r.argv[4] != "." || r.argv[5] != "answers given secrets" ||
		!slices.Equal(r.argv[6:], []string{"sudo", "-n", "notes-stow", "install", "--no-input", "--json", "--set", "ADMIN_EMAIL=a@b", "--tls", "internal"}) {
		t.Errorf("argv %q", r.argv)
	}
	files := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(r.stdin))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(tr)
		files[h.Name] = string(b)
	}
	if files["answers"] != "API_KEY: \"s3cr3t\"\n" || !strings.Contains(files["given"], "given-s3cr3t") || files["secrets"] != "export-s3cr3t" {
		t.Errorf("files %q", files)
	}
}

func TestParseSeed(t *testing.T) {
	t.Parallel()
	good := `{"format":1,"platform":"truenas","user_data":"#cloud-config\n","known_hosts_line":"vm-seed-host-key ssh-ed25519 ` + edKey + `",
"fingerprint":"SHA256:x","host_key_private":"-----BEGIN OPENSSH PRIVATE KEY-----\n","disk":{"truenas":{"format":"qcow2","image":"r/notes-disk@sha256:1","os_gib":23,"disk_gib":43},"aws":{"format":"raw","image":"r/notes-disk@sha256:2","os_gib":23,"disk_gib":43,"cloud_image":"notes-0123456789ab"}},
"waiting":["B","A"],"meta_data":"instance-id: app\n","app_gib":20,"disk_file":{"path":"/d/notes.qcow2","format":"qcow2","sha256":"ab","disk_gib":43},"await_secrets":true}`
	s, err := ParseSeed([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if s.Disk["truenas"].Format != "qcow2" || s.Disk["truenas"].DiskGiB != 43 || s.Disk["aws"].CloudImage != "notes-0123456789ab" || !slices.Equal(s.Waiting, []string{"A", "B"}) ||
		s.MetaData != "instance-id: app\n" || s.AppGiB != 20 || s.DiskFile == nil || s.DiskFile.Path != "/d/notes.qcow2" || !s.AwaitSecrets || s.AwaitTLS {
		t.Errorf("parsed %+v", s)
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"no user data":         {`"user_data":"#cloud-config\n"`, `"user_data":""`, "no user_data"},
		"no private key":       {`"host_key_private":"-----BEGIN OPENSSH PRIVATE KEY-----\n"`, `"host_key_private":""`, "no host_key_private"},
		"a marker line":        {`"known_hosts_line":"vm-seed-host-key`, `"known_hosts_line":"@revoked vm-seed-host-key`, "marker"},
		"disk no image":        {`"image":"r/notes-disk@sha256:1"`, `"image":""`, "no format or image"},
		"a relative disk file": {`"path":"/d/notes.qcow2"`, `"path":"d/notes.qcow2"`, "disk_file has no absolute path"},
		"format 2":             {`"format":1`, `"format":2`, "newer"},
	} {
		in := strings.Replace(good, c.old, c.new, 1)
		if in == good {
			t.Fatalf("%s: the mutation does nothing", name)
		}
		if _, err := ParseSeed([]byte(in)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestSeedArgs(t *testing.T) {
	t.Parallel()
	a := SeedArgs{Platform: "truenas", Hostname: "app", Channel: "beta", IPv6: true, HTTPSPort: 8443,
		SSHAllow: []string{"192.0.2.0/24"}, Hosts: map[string]string{"db": "192.0.2.5"}, Interfaces: map[string]string{"edge": "static,address=192.0.2.10/24"},
		Domains: map[string]string{"app": "app.example"}, Inputs: map[string]string{"ADMIN_EMAIL": "a@b"},
		SSHPublicKey: "ssh-ed25519 AAAA me", Registry: "mirror.example/notes", RegistryCA: "PEM", AwaitSecrets: true, DiskDir: "/disks"}
	args, files, err := a.argv()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vm", "seed", "--json", "--platform=truenas", "--hostname=app", "--channel=beta", "--registry=mirror.example/notes", "--disk-dir=/disks", "--ipv6", "--await-secrets",
		"--https-port=8443", "--ssh-allow=192.0.2.0/24", "--add-host=db=192.0.2.5", "--interface=edge=static,address=192.0.2.10/24",
		"--domains=app=app.example", "--set", "ADMIN_EMAIL=a@b"}
	if !slices.Equal(args, want) {
		t.Errorf("args\n%q\nwant\n%q", args, want)
	}
	if files["ssh-key"] != "ssh-ed25519 AAAA me\n" || files["registry-ca"] != "PEM" || len(files) != 2 {
		t.Errorf("files %q", files)
	}
	for name, bad := range map[string]SeedArgs{
		"a comma in ssh_allow": {SSHAllow: []string{"a,b"}},
		"a newline":            {Hostname: "a\nb"},
		"a bad input key":      {Inputs: map[string]string{"A=B": "x"}},
		"a host with =":        {Hosts: map[string]string{"a=b": "x"}},
	} {
		if _, _, err := bad.argv(); err == nil {
			t.Errorf("%s is taken", name)
		}
	}
}

func TestSeedRun(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{exit: 3, stdout: `{"format":1,"refused":{"reason":"--platform aws: the raw disk is not in r","fix":"give --platform truenas"}}`,
		stderr: "fetching…\nerror: --platform aws: the raw disk is not in r (offers truenas)\n"}
	d := Docker{Runner: r, WorkDir: "/w", Name: "notes"}
	_, err := d.Seed(context.Background(), SeedArgs{Platform: "aws"}, nil)
	var refused *RefusedError
	if !errors.As(err, &refused) || refused.Reason != "--platform aws: the raw disk is not in r" || refused.Fix != "give --platform truenas" {
		t.Errorf("a refused seed: %v", err)
	}
	r.stdout = ""
	if _, err := d.Seed(context.Background(), SeedArgs{Platform: "aws"}, nil); errors.As(err, &refused) || err == nil {
		t.Errorf("exit 3 without its object is taken for a refusal: %v", err)
	}
	if r.argv[2] != RunLoginScript || !slices.Equal(r.argv[4:11], []string{"/w", "", "", "", "sh", "./notes.sh", "vm"}) {
		t.Errorf("argv %q", r.argv)
	}
	// With a login: its host and user, the password in the tar, never an argument.
	r.exit, r.stdout = 0, ""
	_, _ = d.Seed(context.Background(), SeedArgs{Platform: "aws"}, &Login{Host: "mirror.lan", Username: "puller", Password: "pw-s3cr3t"})
	if r.argv[6] != "mirror.lan" || r.argv[7] != "puller" || strings.Contains(strings.Join(r.argv, " "), "pw-s3cr3t") || !bytes.Contains(r.stdin, []byte("pw-s3cr3t")) {
		t.Errorf("a login: %q", r.argv)
	}
	r.exit, r.stdout = 3, `{"format":1,"refused":{"reason":"x"}}`
	r.exit, r.stderr = 1, "boom"
	var failed *FailedError
	if _, err := d.Seed(context.Background(), SeedArgs{Platform: "aws"}, nil); !errors.As(err, &failed) {
		t.Errorf("a failed seed: %v", err)
	}
}

func TestHasCtl(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{}
	v := VM{Runner: r, Short: "notes"}
	if has, err := v.HasCtl(context.Background()); err != nil || !has || r.argv[2] != CtlScript || r.argv[4] != "notes-stow" {
		t.Errorf("found: %v %v %q", has, err, r.argv)
	}
	r.exit = 1
	if has, err := v.HasCtl(context.Background()); err != nil || has {
		t.Errorf("not found: %v %v", has, err)
	}
	r.exit = 255
	if _, err := v.HasCtl(context.Background()); err == nil {
		t.Error("ssh failing is taken for an answer")
	}
	// Describe blames an old installer for its exit 1 alone.
	r.exit, r.stderr = 127, "sh: jq: not found"
	if _, err := v.Describe(context.Background()); err == nil || strings.Contains(err.Error(), "made by Stowage before") {
		t.Errorf("exit 127: %v", err)
	}
	r.exit, r.stderr = 1, `unknown command "describe"`
	if _, err := v.Describe(context.Background()); err == nil || !strings.Contains(err.Error(), "made by Stowage before") {
		t.Errorf("exit 1: %v", err)
	}
}

// RetireScript under a real sh, its paths moved under a test directory: on the active system it
// removes the key and its drop-in and marks the retirement; on the recovery or live system it
// changes nothing and exits 3.
func TestRetireScript(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "recovery_mode", "live_mode"} {
		root := t.TempDir()
		script := strings.NewReplacer("/run/", root+"/run/", "/etc/ssh/", root+"/etc/ssh/", "/oem/", root+"/oem/").Replace(RetireScript)
		for _, d := range []string{"run/cos", "etc/ssh/sshd_config.d", "oem"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		key, dropIn := filepath.Join(root, "run/notes-seed-host-key"), filepath.Join(root, "etc/ssh/sshd_config.d/01-notes-seed-host-key.conf")
		for _, f := range []string{key, dropIn} {
			if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if mode != "" {
			if err := os.WriteFile(filepath.Join(root, "run/cos", mode), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		_, stderr, exit, err := target.Local{}.Run(context.Background(), []string{"env", "PATH=/usr/bin:/bin", "sh", "-c", script, "sh", "notes"}, nil)
		_, keyErr := os.Stat(key)
		_, markErr := os.Stat(filepath.Join(root, "oem/notes-seed-host-key.retired"))
		switch {
		case err != nil:
			t.Fatal(err)
		case mode == "" && (exit != 0 || !os.IsNotExist(keyErr) || markErr != nil):
			t.Errorf("active: exit %d %s, key %v, mark %v", exit, stderr, keyErr, markErr)
		case mode != "" && (exit != 3 || keyErr != nil || !os.IsNotExist(markErr)):
			t.Errorf("%s: exit %d %s, key %v, mark %v", mode, exit, stderr, keyErr, markErr)
		}
	}
}

// An uninstall the kept installer refuses as absent (no install there) is nothing to do; another
// refusal stays one.
func TestUninstallAbsent(t *testing.T) {
	t.Parallel()
	r := &vmRecorder{exit: 3, stdout: `{"format":1,"result":"nothing","refused":{"reason":"/srv/notes holds no install","absent":true}}`}
	d := Docker{Runner: r, WorkDir: "/srv", Name: "notes", Short: "notes"}
	if res, err := d.Uninstall(context.Background(), false); err != nil || res.Result != "nothing" {
		t.Fatalf("absent: %+v %v", res, err)
	}
	r.stdout = `{"format":1,"result":"nothing","refused":{"reason":"another run holds the lock"}}`
	var refused *RefusedError
	if _, err := d.Uninstall(context.Background(), false); !errors.As(err, &refused) {
		t.Fatalf("another refusal: %v", err)
	}
}

// A 0.34.0 installer on Kubernetes or the VM is refused naming the hand upgrade that moves the
// release on.
func TestDescribePre035Hints(t *testing.T) {
	t.Parallel()
	old := `{"format":1,"name":"notes","short_name":"notes","release":{"version":"1"}}`
	if _, err := (VM{Runner: &vmRecorder{stdout: old}, Short: "notes"}).Describe(context.Background()); !errors.Is(err, ErrPreRoutes) || !strings.Contains(err.Error(), "sudo notes-stow upgrade --version V") {
		t.Errorf("vm: %v", err)
	}
	if _, err := (K8s{Runner: &vmRecorder{stdout: old}, Name: "notes"}).Describe(context.Background()); !errors.Is(err, ErrPreRoutes) || !strings.Contains(err.Error(), "its own `k8s upgrade --version V`") {
		t.Errorf("k8s: %v", err)
	}
}
