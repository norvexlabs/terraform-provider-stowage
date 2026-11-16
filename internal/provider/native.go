package provider

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/norvexlabs/terraform-provider-stowage/internal/fetch"
	"github.com/norvexlabs/terraform-provider-stowage/internal/installer"
	"github.com/norvexlabs/terraform-provider-stowage/internal/target"
)

// Platform is this machine's, `<os>/<arch>`: the installer image entry a run on it takes.
var Platform = runtime.GOOS + "/" + runtime.GOARCH

// Want is an installer a run asks for.
type Want struct {
	// Ref is the installer image by digest.
	Ref string
	// Configured are the installers the configuration names: the provider's pull account goes to
	// their registries' hosts (and the provider's registry's) alone (Data.fetchLogin).
	Configured []string
	// Installed is the trust of the release an install runs now, for an upgrade over it: the new
	// installer must be signed by one of its keys; nil for none.
	Installed *fetch.Trust
}

// Program is an installer as a program of this machine.
type Program struct {
	// Path is the file a run executes, in the run's own directory: a prune of the cache never
	// removes it under the run.
	Path string
	// Kept is the program the cache keeps (its entry's `<short>-stow`), "" when the cache does
	// not keep it.
	Kept string
	// Keys are the public keys the release it carries trusts (its payload's keys/).
	Keys map[string][]byte
}

// InstallerSource gives the installer of an image reference as a program of this machine, and
// checks one a target fetches itself.
type InstallerSource interface {
	// Program writes w.Ref's installer for this machine (its index entry for Platform), checked
	// against the trust (Data.trust), and returns it: a file of dir, the run's private directory
	// (Data.runDir), which the run executes — a hard link to the one the cache keeps (a copy when
	// the file system takes no link), else the file itself when there is no cache.
	Program(ctx context.Context, d *Data, w Want, dir string) (*Program, error)
	// Check checks every platform's image of w.Ref against the trust, reading no layer but one
	// (the keys its release carries, which a trust requires): what a Docker target then pulls by
	// that digest is what was checked. Nothing is read without a trust.
	Check(ctx context.Context, d *Data, w Want) error
	// Keys are the public keys w.Ref's release carries, read from its payload, unchecked: the
	// installed release's, by the digest its install records.
	Keys(ctx context.Context, d *Data, w Want) (map[string][]byte, error)
}

// nativeSource reads the installer image from its registry (get: fetch.Get, check: fetch.Check,
// verify: fetch.VerifyImage; nil is each's fetch function) and keeps each platform's file in the
// cache by the index's digest.
type nativeSource struct {
	get    func(context.Context, string, fetch.Options) (*fetch.Installer, error)
	check  func(context.Context, string, fetch.Options) (map[string][]byte, error)
	verify func(context.Context, string, fetch.Options) error
	// link is os.Link when nil: how a run's program is linked to the kept one.
	link func(oldname, newname string) error
}

// options are the registry options of a fetch of w from host, under trust.
func (d *Data) options(host string, w Want, trust []fetch.Trust) fetch.Options {
	o := fetch.Options{Platform: Platform, CA: d.RegistryCA, Trust: trust}
	if l := d.fetchLogin(host, w.Configured...); l != nil {
		o.Logins = append(o.Logins, fetch.Login{Host: l.Host, Username: l.Username, Password: l.Password})
	}
	return o
}

// Program implements InstallerSource.
func (n nativeSource) Program(ctx context.Context, d *Data, w Want, dir string) (*Program, error) {
	_, host, err := parseInstaller(w.Ref)
	if err != nil {
		return nil, err
	}
	_, digest, _ := strings.Cut(w.Ref, "@sha256:")
	plat := strings.ReplaceAll(Platform, "/", "-")
	trust := d.trust(w.Installed)
	o := d.options(host, w, trust)
	var entry string
	if d.CacheDir != "" {
		entry = filepath.Join(d.CacheDir, digest, plat)
		if p, image, keys, ok := cachedProgram(entry); ok {
			// Kept under whatever trust fetched it: checked again against this one.
			err := fetch.RequireKeys(keys, trust)
			if err == nil {
				err = n.verifyFn()(ctx, image, o)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", w.Ref, err)
			}
			// The run's own link to it, which a prune of the cache cannot take away.
			run, err := n.runLink(p, dir, filepath.Base(p))
			if err != nil {
				return nil, fmt.Errorf("linking the installer into the run's directory: %w", err)
			}
			return &Program{Path: run, Kept: p, Keys: keys}, nil
		}
		if err := os.MkdirAll(entry, 0o700); err != nil {
			entry = ""
		} else {
			removeStaleParts(entry, time.Now())
		}
	}
	o.Dir = cmp.Or(entry, dir)
	got, err := n.getFn()(ctx, w.Ref, o)
	if err != nil {
		var np *fetch.NoPlatformError
		if errors.As(err, &np) {
			return nil, fmt.Errorf("%w: the installer runs on the machine running OpenTofu, %s; run OpenTofu on a machine of one of the platforms the image has", err, Platform)
		}
		return nil, err
	}
	if !installer.ValidShortName(got.Short) {
		_ = os.Remove(got.Path)
		return nil, fmt.Errorf("%s: %q is not an installer's short name", w.Ref, got.Short)
	}
	if entry != "" {
		// The run's link first, while the file is where Get wrote it: no prune reaches it.
		run, err := n.runLink(got.Path, dir, got.Short+"-stow")
		if err != nil {
			_ = os.Remove(got.Path)
			return nil, fmt.Errorf("linking the installer into the run's directory: %w", err)
		}
		if p, ok := keepProgram(d.CacheDir, digest, entry, got); ok {
			return &Program{Path: run, Kept: p, Keys: got.Keys}, nil
		}
		_ = os.Remove(got.Path)
		return &Program{Path: run, Keys: got.Keys}, nil
	}
	p := filepath.Join(dir, got.Short+"-stow")
	if err := os.Rename(got.Path, p); err != nil {
		_ = os.Remove(got.Path)
		return nil, fmt.Errorf("writing the installer: %w", err)
	}
	return &Program{Path: p, Keys: got.Keys}, nil
}

// Check implements InstallerSource.
func (n nativeSource) Check(ctx context.Context, d *Data, w Want) error {
	trust := d.trust(w.Installed)
	if len(trust) == 0 {
		return nil
	}
	_, host, err := parseInstaller(w.Ref)
	if err != nil {
		return err
	}
	_, err = n.checkFn()(ctx, w.Ref, d.options(host, w, trust))
	return err
}

// Keys implements InstallerSource: the keys of this machine's platform's image when the cache
// keeps it, else read from the registry and kept beside it (`<digest>/keys.json`; the release's,
// by its digest).
func (n nativeSource) Keys(ctx context.Context, d *Data, w Want) (map[string][]byte, error) {
	_, host, err := parseInstaller(w.Ref)
	if err != nil {
		return nil, err
	}
	_, digest, _ := strings.Cut(w.Ref, "@sha256:")
	var kept string
	if d.CacheDir != "" {
		if _, _, keys, ok := cachedProgram(filepath.Join(d.CacheDir, digest, strings.ReplaceAll(Platform, "/", "-"))); ok {
			return keys, nil
		}
		kept = filepath.Join(d.CacheDir, digest, "keys.json")
		var keys map[string][]byte
		if b, err := os.ReadFile(kept); err == nil && json.Unmarshal(b, &keys) == nil && keys != nil {
			touch(filepath.Dir(kept))
			return keys, nil
		}
	}
	keys, err := n.checkFn()(ctx, w.Ref, d.options(host, w, nil))
	if err != nil {
		return nil, err
	}
	if kept != "" && os.MkdirAll(filepath.Dir(kept), 0o700) == nil {
		if b, err := json.Marshal(keys); err == nil && writeFile(filepath.Dir(kept), "keys.json", b) {
			prune(d.CacheDir, digest)
		}
	}
	return keys, nil
}

func (n nativeSource) getFn() func(context.Context, string, fetch.Options) (*fetch.Installer, error) {
	if n.get == nil {
		return fetch.Get
	}
	return n.get
}

func (n nativeSource) checkFn() func(context.Context, string, fetch.Options) (map[string][]byte, error) {
	if n.check == nil {
		return fetch.Check
	}
	return n.check
}

func (n nativeSource) verifyFn() func(context.Context, string, fetch.Options) error {
	if n.verify == nil {
		return fetch.VerifyImage
	}
	return n.verify
}

// cachedProgram is the program kept in entry (a directory holding `<short>-stow`, its checksum
// and the platform's image by digest), checked against that checksum, and the keys its payload
// carries: one that does not match, records no image or carries no payload is not used, and is
// fetched again. Its use time is refreshed.
func cachedProgram(entry string) (path, image string, keys map[string][]byte, ok bool) {
	files, err := filepath.Glob(filepath.Join(entry, "*-stow"))
	if err != nil || len(files) != 1 {
		return "", "", nil, false
	}
	sum, err := fileChecksum(files[0])
	want, serr := os.ReadFile(files[0] + ".sha256")
	img, ierr := os.ReadFile(filepath.Join(entry, "image"))
	if err != nil || serr != nil || ierr != nil || sum != string(want) || len(img) == 0 {
		return "", "", nil, false
	}
	if keys, err = fetch.FileKeys(files[0]); err != nil {
		return "", "", nil, false
	}
	touch(filepath.Dir(entry))
	return files[0], string(img), keys, true
}

// keepProgram moves got's file into the cache's entry as `<short>-stow` (0700), beside its
// checksum and its image, then removes the entries but the cacheKeep last used; its path, or
// false when the cache could not be written (got's file is then where it was).
func keepProgram(cache, digest, entry string, got *fetch.Installer) (string, bool) {
	sum, err := fileChecksum(got.Path)
	if err != nil {
		return "", false
	}
	// The checksum and the image first: a program without them is never taken from the cache.
	if !writeFile(entry, got.Short+"-stow.sha256", []byte(sum)) || !writeFile(entry, "image", []byte(got.Image)) {
		return "", false
	}
	p := filepath.Join(entry, got.Short+"-stow")
	if os.Rename(got.Path, p) != nil {
		return "", false
	}
	prune(cache, digest)
	return p, true
}

// runLink puts the program at p into dir as name, replacing one there: the run executes it, and a
// prune of the cache removing p leaves it. A hard link when the file system takes one (dir is on
// the cache's, Data.runDir), else a copy: written to a temporary file of dir, closed, then renamed
// into place. A copy may still be held open for writing, for an instant, by a child another
// goroutine forks before its exec, and would not run then (ETXTBSY): busyRunner runs it again.
// Its path.
func (n nativeSource) runLink(p, dir, name string) (string, error) {
	link := n.link
	if link == nil {
		link = os.Link
	}
	dst := filepath.Join(dir, name)
	tmp, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	_ = os.Remove(tmpName)
	if lerr := link(p, tmpName); lerr != nil {
		if tmpName, err = copyProgram(p, dir, name); err != nil {
			return "", fmt.Errorf("linking: %v; copying: %w", lerr, err)
		}
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	return dst, nil
}

// copyProgram copies the program at p to a temporary file of dir (0700), closed: its path.
func copyProgram(p, dir, name string) (string, error) {
	in, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.CreateTemp(dir, "."+name+"-")
	if err != nil {
		return "", err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(0o700)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// busyTries is how many times busyRunner makes a run whose program was busy; busyDelay the wait
// before the second, doubled before each next one (busyDelay * 15 in all).
const (
	busyTries = 5
	busyDelay = 50 * time.Millisecond
)

// busyRunner is a Runner on this machine whose run is made again, up to busyTries times, when the
// shell could not exec a program of dir as it was busy (ETXTBSY: exit 126, the shell naming the
// program and "Text file busy"): a program copied into the run's directory may be held open for
// writing, for an instant, by a child another goroutine forks (runLink). The program never ran
// then. delay, when set, replaces busyDelay (tests).
type busyRunner struct {
	target.Runner
	dir   string
	delay time.Duration
}

// Run implements target.Runner.
func (b busyRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	delay := cmp.Or(b.delay, busyDelay)
	for try := 1; ; try++ {
		stdout, stderr, exit, err := b.Runner.Run(ctx, argv, stdin)
		if err != nil || try == busyTries || !busy(exit, stderr, b.dir) {
			return stdout, stderr, exit, err
		}
		select {
		case <-ctx.Done():
			return stdout, stderr, exit, err
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// WithoutEnv implements target.EnvFilter, when the Runner it wraps does; else it is b.
func (b busyRunner) WithoutEnv(keys []string) target.Runner {
	if f, ok := b.Runner.(target.EnvFilter); ok {
		b.Runner = f.WithoutEnv(keys)
	}
	return b
}

// busy is whether a run exiting exit with stderr is a shell that could not exec a program of dir
// as it was busy.
func busy(exit int, stderr []byte, dir string) bool {
	if exit != 126 || dir == "" {
		return false
	}
	for _, l := range strings.Split(string(stderr), "\n") {
		if strings.Contains(l, dir+string(filepath.Separator)) && strings.Contains(strings.ToLower(l), "text file busy") {
			return true
		}
	}
	return false
}

// runsStale is how old a run's directory in the cache (Data.runDir) is when it is taken for one a
// run cut off left, and removed.
const runsStale = 24 * time.Hour

// runDir makes a run's private directory (0700) for its program, its name starting with prefix:
// in the cache's runs/ when the provider keeps a cache, so that a run's program is a hard link to
// the one the cache keeps (nativeSource.runLink), else — no cache, or one that cannot be written,
// which keeps no program either — in TempDir. Never a file holding a credential: a Kubernetes
// run's kubeconfig and working directory are in TempDir (k8sRun). The caller removes it after the
// run; the runs' directories runsStale old or more are removed first.
func (d *Data) runDir(prefix string) (string, error) {
	if d.CacheDir != "" {
		runs := filepath.Join(d.CacheDir, "runs")
		if os.MkdirAll(runs, 0o700) == nil {
			removeStaleRuns(runs, time.Now())
			return os.MkdirTemp(runs, prefix)
		}
	}
	return os.MkdirTemp(d.TempDir, prefix)
}

// removeStaleRuns removes from runs the directories last changed runsStale or more before now.
func removeStaleRuns(runs string, now time.Time) {
	entries, _ := os.ReadDir(runs)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && e.IsDir() && now.Sub(fi.ModTime()) >= runsStale {
			_ = os.RemoveAll(filepath.Join(runs, e.Name()))
		}
	}
}

// partStale is how long a part file Get writes (fetch.PartPattern) goes unwritten before it is
// taken for one a fetch cut off left: a fetch under way writes it as the image streams.
const partStale = time.Hour

// removeStaleParts removes from dir the part files (fetch.PartPattern) last written partStale or
// more before now.
func removeStaleParts(dir string, now time.Time) {
	parts, _ := filepath.Glob(filepath.Join(dir, fetch.PartPattern))
	for _, p := range parts {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && now.Sub(fi.ModTime()) >= partStale {
			_ = os.Remove(p)
		}
	}
}

// fileChecksum is checksum of the file at p, read as it streams.
func fileChecksum(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// wrapper writes dir/<name>.sh, a script running program with its arguments: the bootstrap
// script's place, which every run on this machine starts (`sh ./<name>.sh …`).
func wrapper(dir, name, program string) error {
	if !installer.ValidName(name) {
		return fmt.Errorf("%q is not a project's name", name)
	}
	script := "#!/bin/sh\nexec " + shellQuote(program) + " \"$@\"\n"
	return os.WriteFile(filepath.Join(dir, name+".sh"), []byte(script), 0o600)
}

// shellQuote is s as one sh word.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
