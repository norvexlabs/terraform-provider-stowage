package target

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// HostKeyAlias is the name the pinned host key is recorded under in the private known_hosts
// file, and the name ssh looks it up by: the key is checked whatever address reaches the host.
const HostKeyAlias = "stowage-target"

// SSH runs argv on a host through the system ssh binary. The host's key is the one given, and
// no other: StrictHostKeyChecking=yes against a private known_hosts file holding that key
// alone, the user's and the system's known_hosts files and KnownHostsCommand out of the
// lookup. Nothing is asked (BatchMode=yes). A secret reaches the remote program only on its
// standard input: the command line is visible in the process table on both machines.
type SSH struct {
	Host string
	Port int // 0: 22
	User string
	// KeyFile is the private key's path; empty, ssh uses its agent and default keys.
	KeyFile string
	// KnownHostsLine is the host's known_hosts line (`[host] keytype base64`).
	KnownHostsLine string
	// Binary is the ssh program; empty is "ssh" from PATH.
	Binary string
	// ConnectTimeout in seconds; 0 is 30.
	ConnectTimeout int
}

// HostKey is a parsed known_hosts line: its key type and base64 key; the host field is
// dropped, as HostKeyAlias replaces it.
type HostKey struct {
	Type string
	Key  string
}

var keyTypes = map[string]bool{
	"ssh-ed25519": true, "ssh-rsa": true,
	"ecdsa-sha2-nistp256": true, "ecdsa-sha2-nistp384": true, "ecdsa-sha2-nistp521": true,
	"sk-ssh-ed25519@openssh.com": true, "sk-ecdsa-sha2-nistp256@openssh.com": true,
}

// ParseKnownHostsLine reads one known_hosts line: `host keytype key [comment]` or `keytype key
// [comment]`. The host field, hashed or a pattern, is dropped (HostKeyAlias replaces it); a
// marker (@cert-authority, @revoked), an unknown key type, a key that is not base64 and more
// than one line are refused.
func ParseKnownHostsLine(line string) (HostKey, error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return HostKey{}, errors.New("known_hosts line is empty")
	}
	if strings.ContainsAny(line, "\r\n") {
		return HostKey{}, errors.New("known_hosts line holds more than one line: give the host's key alone")
	}
	f := strings.Fields(line)
	if strings.HasPrefix(f[0], "@") {
		return HostKey{}, fmt.Errorf("known_hosts line: the marker %s is refused: give the host's own key", f[0])
	}
	if !keyTypes[f[0]] {
		f = f[1:]
	}
	if len(f) < 2 {
		return HostKey{}, errors.New("known_hosts line: want `host keytype key`, e.g. `host.example ssh-ed25519 AAAA…`")
	}
	if !keyTypes[f[0]] {
		return HostKey{}, fmt.Errorf("known_hosts line: unknown key type %q", f[0])
	}
	raw, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(raw) < 4 {
		return HostKey{}, fmt.Errorf("known_hosts line: the %s key is not base64", f[0])
	}
	return HostKey{Type: f[0], Key: f[1]}, nil
}

// KnownHostsFile is the private known_hosts file's content: the key under HostKeyAlias.
func (k HostKey) KnownHostsFile() string {
	return HostKeyAlias + " " + k.Type + " " + k.Key + "\n"
}

// Argv is the ssh command line running remote (already a shell command line, see ShellJoin)
// with the pinned key read from knownHosts.
func (s SSH) Argv(knownHosts, remote string) []string {
	bin := s.Binary
	if bin == "" {
		bin = "ssh"
	}
	port := s.Port
	if port == 0 {
		port = 22
	}
	timeout := s.ConnectTimeout
	if timeout == 0 {
		timeout = 30
	}
	argv := []string{bin, "-T",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "KnownHostsCommand=none",
		"-o", "HostKeyAlias=" + HostKeyAlias,
		"-o", "CheckHostIP=no",
		"-o", "UpdateHostKeys=no",
		"-o", "ConnectTimeout=" + strconv.Itoa(timeout),
		"-p", strconv.Itoa(port),
		"-l", s.User,
	}
	if s.KeyFile != "" {
		argv = append(argv, "-i", s.KeyFile, "-o", "IdentitiesOnly=yes")
	}
	return append(argv, "--", s.Host, remote)
}

// Validate refuses a connection ssh would misread: an empty host or user, one starting with
// '-', or a host key that does not parse.
func (s SSH) Validate() error {
	if err := ValidateHostUser(s.Host, s.User); err != nil {
		return err
	}
	if s.Port < 0 || s.Port > 65535 {
		return fmt.Errorf("ssh: port %d is out of range", s.Port)
	}
	_, err := ParseKnownHostsLine(s.KnownHostsLine)
	return err
}

// ValidateHostUser refuses a host or user ssh would misread: empty, starting with '-', or
// holding a space or a newline (and a user holding '@').
func ValidateHostUser(host, user string) error {
	if host == "" || strings.HasPrefix(host, "-") || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("ssh: host %q is not a host name or address", host)
	}
	if user == "" || strings.HasPrefix(user, "-") || strings.ContainsAny(user, " \t\r\n@") {
		return fmt.Errorf("ssh: user %q is not a user name", user)
	}
	return nil
}

// Run implements Runner: it writes the pinned key to a private known_hosts file, which it
// removes afterwards, and runs argv on the host.
func (s SSH) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	if len(argv) == 0 {
		return nil, nil, -1, errors.New("ssh: no program to run")
	}
	if err := s.Validate(); err != nil {
		return nil, nil, -1, err
	}
	key, _ := ParseKnownHostsLine(s.KnownHostsLine)
	dir, err := os.MkdirTemp("", "stowage-ssh-")
	if err != nil {
		return nil, nil, -1, fmt.Errorf("ssh: known_hosts: %w", err)
	}
	defer os.RemoveAll(dir)
	kh := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(kh, []byte(key.KnownHostsFile()), 0o600); err != nil {
		return nil, nil, -1, fmt.Errorf("ssh: known_hosts: %w", err)
	}
	full := s.Argv(kh, ShellJoin(argv))
	return run(ctx, exec.CommandContext(ctx, full[0], full[1:]...), stdin)
}
