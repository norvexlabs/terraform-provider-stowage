package installer

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// Seed is `vm seed --json`: a new VM's first-boot material.
type Seed struct {
	Format   *int   `json:"format"`
	Platform string `json:"platform"`
	// UserData is the cloud-config for Platform: no secret input, but the one-time host key's
	// private key.
	UserData string `json:"user_data"`
	// KnownHostsLine is the one-time host key as a known_hosts line; Fingerprint its SHA256.
	KnownHostsLine string `json:"known_hosts_line"`
	Fingerprint    string `json:"fingerprint"`
	HostKeyPrivate string `json:"host_key_private"`
	// Disk is, per platform the registry holds a disk for, the disk that platform boots.
	Disk map[string]SeedDisk `json:"disk"`
	// Waiting are the required secret inputs nothing gives, which the first install waits for.
	Waiting []string `json:"waiting"`
}

// SeedDisk is a disk image: its format and its reference by digest.
type SeedDisk struct {
	Format string `json:"format"`
	Image  string `json:"image"`
}

// ParseSeed reads `vm seed --json`.
func ParseSeed(b []byte) (*Seed, error) {
	const doc = "vm seed --json"
	var s Seed
	if err := decodeOne(doc, b, &s); err != nil {
		return nil, err
	}
	if err := checkFormat(doc, s.Format); err != nil {
		return nil, err
	}
	switch {
	case s.Platform == "":
		return nil, fmt.Errorf("%s: no platform", doc)
	case s.UserData == "":
		return nil, fmt.Errorf("%s: no user_data", doc)
	case s.HostKeyPrivate == "":
		return nil, fmt.Errorf("%s: no host_key_private", doc)
	case s.Fingerprint == "":
		return nil, fmt.Errorf("%s: no fingerprint", doc)
	}
	if _, err := target.ParseKnownHostsLine(s.KnownHostsLine); err != nil {
		return nil, fmt.Errorf("%s: %w", doc, err)
	}
	for p, d := range s.Disk {
		if d.Format == "" || d.Image == "" {
			return nil, fmt.Errorf("%s: disk %s has no format or image", doc, p)
		}
	}
	s.Waiting = sortedUnique(s.Waiting)
	return &s, nil
}

// SeedArgs are `vm seed`'s flags. Strings left empty and zero ports are not given.
type SeedArgs struct {
	Platform, Hostname, User, Channel             string
	Network, Address, Gateway, Address6, Gateway6 string
	TuneProfile, Timezone, Device, TLS            string
	IPv6                                          bool
	HTTPPort, HTTPSPort                           int64
	DNS, SSHAllow                                 []string
	Hosts, Interfaces, Domains, Inputs            map[string]string
	// SSHPublicKey is the administrator's key, a file on standard input (--ssh-key).
	SSHPublicKey string
	Registry     string
	RegistryCA   string // a file on standard input (--registry-ca)
}

// argv is a's flags after `vm seed --json`, and the files they name.
func (a SeedArgs) argv() ([]string, map[string]string, error) {
	args := []string{"vm", "seed", "--json"}
	for _, f := range []struct{ flag, v string }{
		{"--platform", a.Platform}, {"--hostname", a.Hostname}, {"--user", a.User}, {"--channel", a.Channel},
		{"--network", a.Network}, {"--address", a.Address}, {"--gateway", a.Gateway},
		{"--address6", a.Address6}, {"--gateway6", a.Gateway6}, {"--tune-profile", a.TuneProfile},
		{"--timezone", a.Timezone}, {"--device", a.Device}, {"--tls", a.TLS}, {"--registry", a.Registry},
	} {
		if f.v == "" {
			continue
		}
		if strings.ContainsAny(f.v, "\r\n") {
			return nil, nil, fmt.Errorf("%s %q: one line", f.flag, f.v)
		}
		args = append(args, f.flag+"="+f.v)
	}
	if a.IPv6 {
		args = append(args, "--ipv6")
	}
	for flag, p := range map[string]int64{"--http-port": a.HTTPPort, "--https-port": a.HTTPSPort} {
		if p != 0 {
			args = append(args, flag+"="+strconv.FormatInt(p, 10))
		}
	}
	for _, f := range []struct {
		flag string
		vs   []string
	}{{"--dns", a.DNS}, {"--ssh-allow", a.SSHAllow}} {
		for _, v := range f.vs {
			if v == "" || strings.ContainsAny(v, ", \t\r\n") {
				return nil, nil, fmt.Errorf("%s %q: one value", f.flag, v)
			}
			args = append(args, f.flag+"="+v)
		}
	}
	for _, f := range []struct {
		flag string
		m    map[string]string
	}{{"--add-host", a.Hosts}, {"--interface", a.Interfaces}} {
		for _, k := range slices.Sorted(maps.Keys(f.m)) {
			if k == "" || strings.ContainsAny(k, "= \t\r\n") || strings.ContainsAny(f.m[k], "\r\n") {
				return nil, nil, fmt.Errorf("%s %q: NAME=VALUE on one line", f.flag, k)
			}
			args = append(args, f.flag+"="+k+"="+f.m[k])
		}
	}
	if v, err := routeList("domains", a.Domains); err != nil {
		return nil, nil, err
	} else if v != "" {
		args = append(args, "--domains="+v)
	}
	for _, k := range slices.Sorted(maps.Keys(a.Inputs)) {
		if !ValidInputKey(k) {
			return nil, nil, fmt.Errorf("inputs: %q is not an input key", k)
		}
		args = append(args, "--set", k+"="+a.Inputs[k])
	}
	files := map[string]string{}
	if a.SSHPublicKey != "" {
		files["ssh-key"] = strings.TrimSpace(a.SSHPublicKey) + "\n"
	}
	if a.RegistryCA != "" {
		files["registry-ca"] = a.RegistryCA
	}
	return args, files, nil
}

// Seed runs the fetched bootstrap script's `vm seed --json` with a: d is the machine running
// OpenTofu, its work directory a private one. A refusal (exit 3) is a *RefusedError, its reason
// the installer's error line.
func (d Docker) Seed(ctx context.Context, a SeedArgs) (*Seed, error) {
	args, files, err := a.argv()
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := d.run(ctx, files, append([]string{"sh", "./" + d.Name + ".sh"}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("vm seed: %w", err)
	}
	switch exit {
	case 0:
		return ParseSeed(stdout)
	case 3:
		return nil, &RefusedError{Verb: "vm seed", Refusal: Refusal{Reason: errorLine(stderr)}}
	}
	return nil, &FailedError{Verb: "vm seed", Exit: exit, Stderr: tail(stderr)}
}

// errorLine is the installer's error from its standard error: the text after the last
// `error: `, else the end of it.
func errorLine(stderr []byte) string {
	if i := bytes.LastIndex(stderr, []byte("error: ")); i >= 0 {
		return strings.TrimSpace(string(stderr[i+len("error: "):]))
	}
	return tail(stderr)
}
