// Package fetch reads an installer image from its registry, with no Docker: the index entry of a
// platform, its one file (`<short>-stow` with the release's payload appended), checked against the
// key sets a caller trusts as Stowage checks a signature — a sigstore bundle referring to the
// image's digest, signed by one of the public keys of each set, with no transparency log. With no
// set, nothing is checked: the reference's digest pins the installer.
package fetch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// MaxInstaller bounds the installer file read from an image: a static binary and its payload.
const MaxInstaller = 512 << 20

// Login is a registry login for one host.
type Login struct {
	Host, Username, Password string
}

// Trust is a set of public keys an installer must be signed by one of.
type Trust struct {
	// Name says whose keys they are, in an error: "the provider's public_keys".
	Name string
	// Keys are PEM public keys, by name.
	Keys map[string][]byte
}

// Options are how the registry is read.
type Options struct {
	// Logins are the logins by registry host; a host without one is read with the machine's own
	// Docker configuration (its auths and credential helpers), else anonymously.
	Logins []Login
	// CA is PEM of a certificate authority trusted besides the system's (a mirror's own).
	CA string
	// Platform is the image's platform, `<os>/<arch>`.
	Platform string
	// Transport, when set, carries the requests (tests); else Go's default, with CA.
	Transport http.RoundTripper
	// Trust are the key sets the installer must be signed by: one key of each. The release it
	// carries must then carry keys itself (one that does not sign is refused). Empty: nothing is
	// checked, the reference's digest pins the installer.
	Trust []Trust
	// Dir is the directory Get writes the file in (a temporary name, the caller's to rename or
	// remove); "" is the system's temporary directory.
	Dir string
}

// Installer is an installer image's file for one platform.
type Installer struct {
	// Short is the installer's short name: the file is `<Short>-stow`.
	Short string
	// Path is the file written in Options.Dir, 0700: the executable and the release's payload.
	Path string
	// Image is the platform's image by digest, `<repository>@sha256:<hex>`.
	Image string
	// Keys are the public keys the release carries (the payload's keys/), by file name; empty
	// for a project that does not sign.
	Keys map[string][]byte
}

// source is an installer reference read: its registry's options and its images.
type source struct {
	ref  name.Digest
	opts []remote.Option
	// images are its images by platform (`<os>/<arch>`), in the index's order; one image of no
	// index is the one entry.
	images []entry
	idx    v1.ImageIndex
	img    v1.Image // the image, when ref is no index
}

type entry struct {
	platform string
	digest   v1.Hash
}

// read resolves ref, an installer image by digest: an index's entries of a platform, or one image
// (an installer a stowage before 0.40.0 published: one linux/amd64 image).
func read(ctx context.Context, ref string, o Options) (*source, error) {
	d, err := name.NewDigest(ref)
	if err != nil {
		return nil, fmt.Errorf("installer %s: %w", ref, err)
	}
	opts, err := o.remote(ctx, d.Context().RegistryStr())
	if err != nil {
		return nil, err
	}
	desc, err := remote.Get(d, opts...)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", ref, err)
	}
	s := &source{ref: d, opts: opts}
	if desc.MediaType.IsIndex() {
		if s.idx, err = desc.ImageIndex(); err != nil {
			return nil, fmt.Errorf("reading %s: %w", ref, err)
		}
		m, err := s.idx.IndexManifest()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", ref, err)
		}
		for _, e := range m.Manifests {
			if e.Platform != nil {
				s.images = append(s.images, entry{e.Platform.OS + "/" + e.Platform.Architecture, e.Digest})
			}
		}
		return s, nil
	}
	if s.img, err = desc.Image(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", ref, err)
	}
	cf, err := s.img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("reading %s's configuration: %w", ref, err)
	}
	s.images = []entry{{cmpOr(cf.OS, "linux") + "/" + cmpOr(cf.Architecture, "amd64"), desc.Digest}}
	return s, nil
}

// of is s's entry for platform; a NoPlatformError without one.
func (s *source) of(platform string) (entry, error) {
	var have []string
	for _, e := range s.images {
		if e.platform == platform {
			return e, nil
		}
		have = append(have, e.platform)
	}
	return entry{}, &NoPlatformError{Ref: s.ref.String(), Platform: platform, Have: have}
}

// image is e's image.
func (s *source) image(e entry) (v1.Image, error) {
	if s.idx == nil {
		return s.img, nil
	}
	img, err := s.idx.Image(e.digest)
	if err != nil {
		return nil, fmt.Errorf("reading %s's %s image: %w", s.ref, e.platform, err)
	}
	return img, nil
}

// verify checks e's image against each of trust: a signature by one key of each.
func (s *source) verify(ctx context.Context, e entry, trust []Trust) error {
	return verifyImage(ctx, s.ref.Context().Digest(e.digest.String()), trust, s.opts)
}

func verifyImage(ctx context.Context, image name.Digest, trust []Trust, opts []remote.Option) error {
	for _, t := range trust {
		v, err := NewVerifier(t.Keys)
		if err != nil {
			return fmt.Errorf("%s: %w", t.Name, err)
		}
		if err := v.Verify(ctx, image, opts); err != nil {
			return fmt.Errorf("checking %s against %s: %w", image, t.Name, err)
		}
	}
	return nil
}

// Get reads ref, an installer image by digest, for o.Platform: an index's entry for that platform
// (refused, naming those it has, when there is none), or an image of one platform, which must be
// it. The image is checked against o.Trust before its layer is read; its one file is then written
// in o.Dir, its payload's keys read from it.
func Get(ctx context.Context, ref string, o Options) (*Installer, error) {
	s, err := read(ctx, ref, o)
	if err != nil {
		return nil, err
	}
	e, err := s.of(o.Platform)
	if err != nil {
		return nil, err
	}
	if err := s.verify(ctx, e, o.Trust); err != nil {
		return nil, err
	}
	img, err := s.image(e)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(o.Dir, PartPattern)
	if err != nil {
		return nil, fmt.Errorf("a file for the installer: %w", err)
	}
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(f.Name())
		}
	}()
	short, err := oneFile(img, f)
	if err != nil {
		return nil, fmt.Errorf("%s's %s image: %w", ref, o.Platform, err)
	}
	keys, err := fileKeys(f)
	if err == nil {
		err = RequireKeys(keys, o.Trust)
	}
	if err == nil {
		err = errors.Join(f.Chmod(0o700), f.Close())
	}
	if err != nil {
		return nil, fmt.Errorf("%s's %s image: %w", ref, o.Platform, err)
	}
	keep = true
	return &Installer{Short: short, Path: f.Name(), Image: s.ref.Context().Digest(e.digest.String()).String(), Keys: keys}, nil
}

// PartPattern names the file Get writes an installer into until it is whole (os.CreateTemp's
// pattern, its * random): one a fetch cut off leaves is recognised by it, and removed by the
// directory's owner.
const PartPattern = ".installer-*.part"

// Check checks every image of ref (each platform's) against o.Trust, reading no layer, then
// returns the keys its release carries, read from o.Platform's image (any image's when it has
// none: the payload is the release's, the same in each) as it streams, nothing written. With no
// trust it reads the keys alone.
func Check(ctx context.Context, ref string, o Options) (map[string][]byte, error) {
	s, err := read(ctx, ref, o)
	if err != nil {
		return nil, err
	}
	if len(s.images) == 0 {
		return nil, fmt.Errorf("%s has no image for any platform", ref)
	}
	for _, e := range s.images {
		if err := s.verify(ctx, e, o.Trust); err != nil {
			return nil, err
		}
	}
	e, err := s.of(o.Platform)
	if err != nil {
		e = s.images[0]
	}
	img, err := s.image(e)
	if err != nil {
		return nil, err
	}
	t := &tail{max: maxPayload + trailerLen}
	if _, err := oneFile(img, t); err != nil {
		return nil, fmt.Errorf("%s's %s image: %w", ref, e.platform, err)
	}
	keys, err := PayloadKeys(t.bytes())
	if err == nil {
		err = RequireKeys(keys, o.Trust)
	}
	if err != nil {
		return nil, fmt.Errorf("%s's %s image: %w", ref, e.platform, err)
	}
	return keys, nil
}

// VerifyImage checks image, a platform's image by digest, against o.Trust: a kept installer
// checked again against another trust than the one it was fetched under.
func VerifyImage(ctx context.Context, image string, o Options) error {
	d, err := name.NewDigest(image)
	if err != nil {
		return fmt.Errorf("installer image %s: %w", image, err)
	}
	if len(o.Trust) == 0 {
		return nil
	}
	opts, err := o.remote(ctx, d.Context().RegistryStr())
	if err != nil {
		return err
	}
	return verifyImage(ctx, d, o.Trust, opts)
}

// RequireKeys refuses a release carrying no key when a trust applies: its install could trust no
// key, and a later release would be taken unchecked.
func RequireKeys(keys map[string][]byte, trust []Trust) error {
	if len(trust) == 0 || len(keys) > 0 {
		return nil
	}
	names := make([]string, 0, len(trust))
	for _, t := range trust {
		names = append(names, t.Name)
	}
	return fmt.Errorf("the release carries no public key (its project does not sign), and %s must sign it: refused", strings.Join(names, " and "))
}

func cmpOr(a, b string) string {
	if a == "" {
		return b
	}
	return a
}

// NoPlatformError is an installer image without an image for the platform asked.
type NoPlatformError struct {
	Ref, Platform string
	Have          []string
}

func (e *NoPlatformError) Error() string {
	if len(e.Have) == 0 {
		return fmt.Sprintf("%s has no image for %s, nor for any platform", e.Ref, e.Platform)
	}
	return fmt.Sprintf("%s has no image for %s (it has %s)", e.Ref, e.Platform, strings.Join(e.Have, ", "))
}

// oneFile copies the image's one layer's one file, `<short>-stow` at its root, to w, and returns
// its short name.
func oneFile(img v1.Image, w io.Writer) (string, error) {
	layers, err := img.Layers()
	if err != nil {
		return "", err
	}
	if len(layers) != 1 {
		return "", fmt.Errorf("an installer image has one layer, not %d", len(layers))
	}
	rc, err := layers[0].Uncompressed()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	var short string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", fmt.Errorf("its layer: %w", err)
		}
		n := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		if h.Typeflag == tar.TypeDir && (n == "" || n == ".") {
			continue
		}
		s, ok := strings.CutSuffix(n, "-stow")
		if short != "" || h.Typeflag != tar.TypeReg || !ok || s == "" || strings.Contains(n, "/") {
			return "", fmt.Errorf("its layer holds %q: an installer's layer holds one file, <short>-stow, at its root", h.Name)
		}
		if h.Size > MaxInstaller {
			return "", fmt.Errorf("%s is %d bytes, over %d", n, h.Size, MaxInstaller)
		}
		if _, err := io.CopyN(w, tr, h.Size); err != nil {
			return "", fmt.Errorf("reading %s: %w", n, err)
		}
		short = s
	}
	if short == "" {
		return "", errors.New("its layer holds no file: an installer's holds <short>-stow")
	}
	return short, nil
}

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) bytes() []byte {
	if len(t.buf) > t.max {
		return t.buf[len(t.buf)-t.max:]
	}
	return t.buf
}

// fileKeys are PayloadKeys of the file f, read from its end.
func fileKeys(f *os.File) (map[string][]byte, error) {
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	n := min(fi.Size(), int64(maxPayload+trailerLen))
	b := make([]byte, n)
	if _, err := f.ReadAt(b, fi.Size()-n); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return PayloadKeys(b)
}

// FileKeys are the keys the release of the installer at path carries (PayloadKeys).
func FileKeys(path string) (map[string][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return fileKeys(f)
}

// payloadMagic ends an installer carrying a release's payload: <executable> <tar> <8-byte
// big-endian tar length> <magic>.
const payloadMagic = "STOWAGE-PAYLOAD1"

// maxPayload bounds a payload, as Stowage does.
const maxPayload = 16 << 20

// trailerLen is the payload's trailer: its length and the magic.
const trailerLen = 8 + len(payloadMagic)

// PayloadKeys are the public keys bin's payload carries (keys/<name>), by name: the release's
// trusted keys. bin is the file, or its last bytes holding the whole payload and its trailer. A
// file without a payload is no installer.
func PayloadKeys(bin []byte) (map[string][]byte, error) {
	trailer := 8 + len(payloadMagic)
	if len(bin) < trailer || string(bin[len(bin)-len(payloadMagic):]) != payloadMagic {
		return nil, errors.New("the file carries no release (no payload): not an installer")
	}
	n := binary.BigEndian.Uint64(bin[len(bin)-trailer : len(bin)-len(payloadMagic)])
	if n > maxPayload || n > uint64(len(bin)-trailer) {
		return nil, fmt.Errorf("its payload's length %d is not within the file", n)
	}
	start := len(bin) - trailer - int(n)
	return TarKeys(bin[start : len(bin)-trailer])
}

// TarKeys are the public keys a payload's tar carries (keys/<name>), by name. At most maxPayload
// bytes are read.
func TarKeys(payload []byte) (map[string][]byte, error) {
	if len(payload) > maxPayload {
		return nil, fmt.Errorf("its payload's length %d is over %d", len(payload), maxPayload)
	}
	tr := tar.NewReader(bytes.NewReader(payload))
	keys := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("its payload: %w", err)
		}
		base, ok := strings.CutPrefix(h.Name, "keys/")
		if !ok || h.Typeflag != tar.TypeReg || base == "" || strings.Contains(base, "/") {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 1<<20))
		if err != nil {
			return nil, fmt.Errorf("its payload's %s: %w", h.Name, err)
		}
		keys[base] = b
	}
	return keys, nil
}

// remote is the registry options for host: o's login for it, else the machine's Docker
// configuration; o's transport.
func (o Options) remote(ctx context.Context, host string) ([]remote.Option, error) {
	t, err := o.transport()
	if err != nil {
		return nil, err
	}
	opts := []remote.Option{remote.WithContext(ctx), remote.WithTransport(t), remote.WithUserAgent("terraform-provider-stowage")}
	i := slices.IndexFunc(o.Logins, func(l Login) bool { return sameHost(l.Host, host) })
	if i >= 0 {
		opts = append(opts, remote.WithAuth(&authn.Basic{Username: o.Logins[i].Username, Password: o.Logins[i].Password}))
	} else {
		opts = append(opts, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	}
	return opts, nil
}

// sameHost says whether a login's host is the registry's: Docker Hub's names as one.
func sameHost(a, b string) bool {
	hub := func(h string) string {
		switch h = strings.ToLower(h); h {
		case "docker.io", "index.docker.io", "registry-1.docker.io":
			return name.DefaultRegistry
		}
		return h
	}
	return hub(a) == hub(b)
}

// transport is o's, else Go's default trusting o.CA besides the system's; either refuses plain
// HTTP to a registry that is not on this machine's loopback.
func (o Options) transport() (http.RoundTripper, error) {
	if o.Transport != nil {
		return httpsOnly{o.Transport}, nil
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	if o.CA != "" {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM([]byte(o.CA)) {
			return nil, errors.New("the registry CA holds no PEM certificate")
		}
		t.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return httpsOnly{t}, nil
}

// httpsOnly refuses a plain HTTP request to a host that is not a loopback address or localhost:
// the client library falls back to HTTP for a private address, which would send a login in the
// clear.
type httpsOnly struct{ next http.RoundTripper }

func (h httpsOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Scheme == "http" && !loopback(r.URL.Hostname()) {
		return nil, fmt.Errorf("%s: a registry is read over HTTPS (plain HTTP only on this machine's loopback)", r.URL.Host)
	}
	return h.next.RoundTrip(r)
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
