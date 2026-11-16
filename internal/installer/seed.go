package installer

import (
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
	// MetaData is the NoCloud meta-data beside UserData (instance-id, local-hostname).
	MetaData string `json:"meta_data"`
	// AppGiB is the app's space the release recommends, which each disk's DiskGiB adds.
	AppGiB int64 `json:"app_gib"`
	// DiskFile is the platform's disk written as a file (--disk-dir); nil otherwise.
	DiskFile *SeedDiskFile `json:"disk_file"`
	// AwaitSecrets and AwaitTLS: the first install waits for a secrets export (--await-secrets),
	// or for a byo certificate (--tls byo), given over SSH.
	AwaitSecrets bool `json:"await_secrets"`
	AwaitTLS     bool `json:"await_tls"`
	// Interfaces are the NICs the VM is made with, sorted by role (Stowage 0.38.0 or later; nil
	// from an earlier installer, which names none).
	Interfaces *[]SeedInterface `json:"interfaces"`
	// AwaitGiven are the secret inputs the answers file holds, which the first install waits for
	// over SSH (never a value).
	AwaitGiven []string `json:"await_given"`
	// RegistryLogin is the host the VM's own login is for (--registry-login-file), never the
	// login.
	RegistryLogin string `json:"registry_login"`
	// Size is the VM's size the release takes (Stowage 0.40.0 or later; nil before).
	Size *SeedSize `json:"size"`
	// Firewall is what a platform's firewall in front of the VM lets through (Stowage 0.40.0 or
	// later; nil before).
	Firewall *SeedFirewall `json:"firewall"`
}

// SeedSize is the VM's size the release takes: its CPUs, its memory in MiB and the app's space in
// GiB, each at least Min, by default Recommended.
type SeedSize struct {
	CPUs      SeedBound `json:"cpus"`
	MemoryMiB SeedBound `json:"memory_mib"`
	AppGiB    SeedBound `json:"app_gib"`
}

// SeedBound is a size's least and its default.
type SeedBound struct {
	Min         int64 `json:"min"`
	Recommended int64 `json:"recommended"`
}

// SeedFirewall is what the VM's own firewall opens: SSH from SSHSources, and each of OpenPorts.
type SeedFirewall struct {
	SSHSources []string   `json:"ssh_sources"`
	OpenPorts  []SeedPort `json:"open_ports"`
}

// SeedPort is a port the VM opens besides SSH, from its sources.
type SeedPort struct {
	Protocol string   `json:"protocol"`
	Port     int64    `json:"port"`
	Sources  []string `json:"sources"`
}

// SeedInterface is a NIC a seed's VM is made with: its role, the MAC its cloud-config names it by
// ("" for a VM of one NIC, named from the NIC it has, and on a cloud), the network it attaches to
// ("" the primary's), and whether it is the primary.
type SeedInterface struct {
	Role    string `json:"role"`
	MAC     string `json:"mac"`
	Attach  string `json:"attach"`
	Primary bool   `json:"primary"`
}

// SeedDisk is a disk image: its format, its reference by digest, what Kairos takes of a VM's
// disk of it (OSGiB) and the VM's disk size to create (DiskGiB), and on a cloud the release
// publishes to, the name its image is registered under there.
type SeedDisk struct {
	Format     string `json:"format"`
	Image      string `json:"image"`
	OSGiB      int64  `json:"os_gib"`
	DiskGiB    int64  `json:"disk_gib"`
	CloudImage string `json:"cloud_image"`
}

// SeedDiskFile is a disk written as a file: its absolute path, format, the SHA-256 of its bytes
// and the VM's disk size to create.
type SeedDiskFile struct {
	Path    string `json:"path"`
	Format  string `json:"format"`
	SHA256  string `json:"sha256"`
	DiskGiB int64  `json:"disk_gib"`
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
	if f := s.DiskFile; f != nil && (!strings.HasPrefix(f.Path, "/") || f.Format == "" || f.SHA256 == "") {
		return nil, fmt.Errorf("%s: disk_file has no absolute path, format or sha256", doc)
	}
	if s.Interfaces != nil {
		roles := map[string]bool{}
		for _, i := range *s.Interfaces {
			if i.Role == "" || roles[i.Role] {
				return nil, fmt.Errorf("%s: interfaces: a NIC with no role, or a role named twice (%q)", doc, i.Role)
			}
			roles[i.Role] = true
		}
	}
	if z := s.Size; z != nil {
		for n, b := range map[string]SeedBound{"cpus": z.CPUs, "memory_mib": z.MemoryMiB, "app_gib": z.AppGiB} {
			if b.Min < 1 || b.Recommended < b.Min {
				return nil, fmt.Errorf("%s: size.%s: min %d and recommended %d are not a size", doc, n, b.Min, b.Recommended)
			}
		}
	}
	if f := s.Firewall; f != nil {
		for _, p := range f.OpenPorts {
			if (p.Protocol != "tcp" && p.Protocol != "udp") || p.Port < 1 || p.Port > 65535 {
				return nil, fmt.Errorf("%s: firewall.open_ports: %s/%d is not a port", doc, p.Protocol, p.Port)
			}
		}
	}
	s.Waiting = sortedUnique(s.Waiting)
	s.AwaitGiven = sortedUnique(s.AwaitGiven)
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
	// AwaitSecrets: the first install waits for a secrets export given over SSH.
	AwaitSecrets bool
	// DiskDir is an existing directory the platform's disk is written into (--disk-dir).
	DiskDir string
}

// argv is a's flags after `vm seed --json`, and the files they name.
func (a SeedArgs) argv() ([]string, map[string]string, error) {
	args := []string{"vm", "seed", "--json"}
	for _, f := range []struct{ flag, v string }{
		{"--platform", a.Platform}, {"--hostname", a.Hostname}, {"--user", a.User}, {"--channel", a.Channel},
		{"--network", a.Network}, {"--address", a.Address}, {"--gateway", a.Gateway},
		{"--address6", a.Address6}, {"--gateway6", a.Gateway6}, {"--tune-profile", a.TuneProfile},
		{"--timezone", a.Timezone}, {"--device", a.Device}, {"--tls", a.TLS}, {"--registry", a.Registry},
		{"--disk-dir", a.DiskDir},
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
	if a.AwaitSecrets {
		args = append(args, "--await-secrets")
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
// OpenTofu, its work directory a private one; with login, the run reads the registry (its disks)
// with that login, in a Docker configuration of its own (RunLoginScript). A refusal (exit 3) is a
// *RefusedError, read from the refusal object on standard output.
func (d Docker) Seed(ctx context.Context, a SeedArgs, login *Login) (*Seed, error) {
	args, files, err := a.argv()
	if err != nil {
		return nil, err
	}
	if d.Mirror != nil && !(login != nil && strings.EqualFold(login.Host, d.Mirror.Host)) {
		if files, err = MirrorFiles(files, d.Mirror); err != nil {
			return nil, err
		}
	}
	names, host, user, stdin, err := loginInput(files, login)
	if err != nil {
		return nil, err
	}
	argv := append([]string{"sh", "-c", RunLoginScript, "sh", d.WorkDir, names, host, user, "sh", "./" + d.Name + ".sh"}, args...)
	stdout, stderr, exit, err := d.Runner.Run(ctx, argv, stdin)
	if err != nil {
		return nil, fmt.Errorf("vm seed: %w", err)
	}
	switch exit {
	case 0:
		return ParseSeed(stdout)
	case 3:
		r, err := ParseRefused("vm seed --json", stdout, stderr)
		if err != nil {
			return nil, err
		}
		return nil, r
	}
	return nil, &FailedError{Verb: "vm seed", Exit: exit, Stderr: tail(stderr)}
}
