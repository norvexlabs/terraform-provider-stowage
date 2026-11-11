package target

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const edKey = "AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"

func TestParseKnownHostsLine(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		"host.example ssh-ed25519 " + edKey,
		"[10.0.0.5]:2222 ssh-ed25519 " + edKey + " root@vm",
		"|1|abc=|def= ssh-ed25519 " + edKey,
		"ssh-ed25519 " + edKey,
		"  host ssh-ed25519 " + edKey + "  \n",
	} {
		k, err := ParseKnownHostsLine(line)
		if err != nil {
			t.Fatalf("%q: %v", line, err)
		}
		if k.Type != "ssh-ed25519" || k.Key != edKey {
			t.Fatalf("%q: got %+v", line, k)
		}
		if got := k.KnownHostsFile(); got != HostKeyAlias+" ssh-ed25519 "+edKey+"\n" {
			t.Fatalf("%q: file %q", line, got)
		}
	}
	for line, want := range map[string]string{
		"":                                       "empty",
		"host ssh-ed25519 " + edKey + "\nx":      "more than one line",
		"@cert-authority * ssh-ed25519 " + edKey: "marker",
		"@revoked host ssh-ed25519 " + edKey:     "marker",
		"host ssh-dss " + edKey:                  "unknown key type",
		"host ssh-ed25519":                       "want",
		"host ssh-ed25519 not*base64":            "not base64",
	} {
		_, err := ParseKnownHostsLine(line)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%q: got %v, want %q", line, err, want)
		}
	}
}

func TestSSHArgvIsStrict(t *testing.T) {
	t.Parallel()
	s := SSH{Host: "app.example", Port: 2222, User: "ops", KeyFile: "/keys/id"}
	argv := s.Argv("/priv/known_hosts", "sh -c 'x'")
	joined := strings.Join(argv, " ")
	for _, opt := range []string{
		"BatchMode=yes",
		"StrictHostKeyChecking=yes",
		"UserKnownHostsFile=/priv/known_hosts",
		"GlobalKnownHostsFile=/dev/null",
		"KnownHostsCommand=none",
		"HostKeyAlias=" + HostKeyAlias,
		"UpdateHostKeys=no",
		"VerifyHostKeyDNS=no",
		"ControlMaster=no",
		"ControlPath=none",
		"IdentitiesOnly=yes",
	} {
		i := slices.Index(argv, opt)
		if i < 1 || argv[i-1] != "-o" {
			t.Errorf("option %s missing as -o: %s", opt, joined)
		}
	}
	if !strings.Contains(joined, " -p 2222 ") || !strings.Contains(joined, " -l ops ") || !strings.Contains(joined, " -i /keys/id ") {
		t.Errorf("port, user or key missing: %s", joined)
	}
	if got := argv[len(argv)-3:]; !slices.Equal(got, []string{"--", "app.example", "sh -c 'x'"}) {
		t.Errorf("tail %q: want -- host command", got)
	}
	// No key file: the agent, and no IdentitiesOnly that would hide its keys.
	agent := SSH{Host: "h", User: "u"}.Argv("/k", "true")
	if slices.Contains(agent, "-i") || slices.Contains(agent, "IdentitiesOnly=yes") || !slices.Contains(agent, "22") {
		t.Errorf("agent argv %q", agent)
	}
}

func TestSSHValidate(t *testing.T) {
	t.Parallel()
	good := SSH{Host: "h", User: "u", KnownHostsLine: "h ssh-ed25519 " + edKey}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]SSH{
		"host option": {Host: "-oProxyCommand=x", User: "u", KnownHostsLine: good.KnownHostsLine},
		"empty host":  {User: "u", KnownHostsLine: good.KnownHostsLine},
		"user option": {Host: "h", User: "-x", KnownHostsLine: good.KnownHostsLine},
		"no key":      {Host: "h", User: "u"},
		"bad port":    {Host: "h", User: "u", Port: 70000, KnownHostsLine: good.KnownHostsLine},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The command line a remote shell splits must give back argv exactly.
func TestShellJoinRoundTrips(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	argv := []string{"printf", `%s\n`, "plain", "it's", `$HOME`, "a b", "`id`", "", "A=b", "~", "*", "x;y", "-n"}
	out, err := exec.Command(sh, "-c", ShellJoin(argv)).Output()
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(argv[2:], "\n") + "\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestLocalRun(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	out, errOut, exit, err := Local{}.Run(context.Background(), []string{"sh", "-c", "cat; echo e >&2; exit 3"}, []byte("in"))
	if err != nil || exit != 3 || string(out) != "in" || string(errOut) != "e\n" {
		t.Fatalf("got %q %q %d %v", out, errOut, exit, err)
	}
	if _, _, exit, err := (Local{}).Run(context.Background(), []string{"/nonexistent/prog"}, nil); err == nil || exit != -1 {
		t.Fatalf("missing program: exit %d err %v", exit, err)
	}
}

// A run cut by its context returns once the program is killed, even when a program it started
// keeps the output open (it sets waitDelay: no t.Parallel).
func TestRunCutByItsContextReturns(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	defer func(d time.Duration) { waitDelay = d }(waitDelay)
	waitDelay = 50 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	// sh is killed; the sleep it started holds stdout for 3 seconds.
	_, _, exit, err := Local{}.Run(ctx, []string{"sh", "-c", "sleep 3; true"}, nil)
	if err == nil || exit != -1 {
		t.Fatalf("a run cut by its context: exit %d err %v", exit, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the run returned after %v, held by the output its child kept open", d)
	}
}

// A program that exits while one it started keeps the output open keeps its exit status (it
// sets waitDelay: no t.Parallel).
func TestRunExitedKeepsItsStatus(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	defer func(d time.Duration) { waitDelay = d }(waitDelay)
	waitDelay = 50 * time.Millisecond
	start := time.Now()
	out, _, exit, err := Local{}.Run(context.Background(), []string{"sh", "-c", "echo done; sleep 3 & exit 0"}, nil)
	if err != nil || exit != 0 || string(out) != "done\n" {
		t.Fatalf("got %q exit %d err %v", out, exit, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the run returned after %v, held by the output its child kept open", d)
	}
}

func TestCappedOutput(t *testing.T) {
	t.Parallel()
	var c capped
	c.Write(make([]byte, MaxOutput-1))
	c.Write([]byte("abc"))
	if got := c.Bytes(); len(got) != MaxOutput+len("\n[output truncated]\n") {
		t.Fatalf("len %d", len(got))
	}
}

// Run hands ssh a private known_hosts file holding the pinned key alone, under the alias, and the
// standard input; the file is gone afterwards.
func TestSSHRunWritesPinnedKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nfor a; do case $a in UserKnownHostsFile=*) f=${a#UserKnownHostsFile=}; echo \"$f\"; cat \"$f\"; ls -l \"$f\" | cut -c1-10;; esac; done\neval \"last=\\${$#}\"; echo \"cmd=$last\"\ncat\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := SSH{Host: "h", User: "u", KnownHostsLine: "h ssh-ed25519 " + edKey, Binary: fake}
	out, _, exit, err := runScript(s, []string{"echo", "a b"}, []byte("secret-on-stdin"))
	if err != nil || exit != 0 {
		t.Fatalf("exit %d err %v", exit, err)
	}
	path, rest, _ := strings.Cut(string(out), "\n")
	want := HostKeyAlias + " ssh-ed25519 " + edKey + "\n-rw-------\ncmd=echo 'a b'\nsecret-on-stdin"
	if rest != want {
		t.Fatalf("got %q\nwant %q", rest, want)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("known_hosts directory %s left behind: %v", path, err)
	}
}

// Run pins the other keys the host may serve beside the first, each under the alias, and refuses
// one that does not parse before any connection.
func TestSSHRunPinsEveryKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fake := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nfor a; do case $a in UserKnownHostsFile=*) cat \"${a#UserKnownHostsFile=}\";; esac; done\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	other := "AAAAC3NzaC1lZDI1NTE5AAAAIA6m3nbmS3rbQ6jH1gm8K07n6l9Tqv9c0vY2pY6J6oTd"
	s := SSH{Host: "h", User: "u", KnownHostsLine: "h ssh-ed25519 " + edKey, AlsoKnownHostsLines: []string{"vm-seed-host-key ssh-ed25519 " + other}, Binary: fake}
	out, _, _, err := runScript(s, []string{"true"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := HostKeyAlias + " ssh-ed25519 " + edKey + "\n" + HostKeyAlias + " ssh-ed25519 " + other + "\n"; string(out) != want {
		t.Fatalf("known_hosts %q, want %q", out, want)
	}
	s.AlsoKnownHostsLines = []string{"@revoked * ssh-ed25519 " + other}
	if _, _, _, err := s.Run(context.Background(), []string{"true"}, nil); err == nil {
		t.Fatal("a marker line among the other keys is taken")
	}
}

// runScript runs s, whose Binary is a script the test just wrote, again while exec reports it busy
// (ETXTBSY): a parallel test's fork may hold the descriptor the script was written through until
// its own exec closes it.
func runScript(s SSH, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	for i := 0; ; i++ {
		out, errOut, exit, err := s.Run(context.Background(), argv, stdin)
		if err == nil || !strings.Contains(err.Error(), "text file busy") || i == 50 {
			return out, errOut, exit, err
		}
		time.Sleep(time.Millisecond)
	}
}
