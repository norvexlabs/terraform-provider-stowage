package target

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	out, _, exit, err := s.Run(context.Background(), []string{"echo", "a b"}, []byte("secret-on-stdin"))
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
