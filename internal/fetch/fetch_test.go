package fetch

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// env is a registry served in-process on the loopback, its requests counted.
type env struct {
	host string
	reqs atomic.Int64
	// auth, when set, is the Basic login every request must carry.
	auth string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	reg := registry.New(registry.WithReferrersSupport(true), registry.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.reqs.Add(1)
		if e.auth != "" {
			if u, p, ok := r.BasicAuth(); !ok || u+":"+p != e.auth {
				w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.host = strings.TrimPrefix(srv.URL, "http://")
	return e
}

// payload is an executable followed by a payload of files, as Stowage appends one.
func payload(exe string, files map[string]string) []byte {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	for n, c := range files {
		_ = tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(c))
	}
	_ = tw.Close()
	out := append([]byte(exe), tb.Bytes()...)
	out = binary.BigEndian.AppendUint64(out, uint64(tb.Len()))
	return append(out, payloadMagic...)
}

// layerOf is a layer holding the files given, in order.
func layerOf(t *testing.T, files ...[2]string) v1.Layer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		if err := tw.WriteHeader(&tar.Header{Name: f[0], Mode: 0o755, Size: int64(len(f[1])), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(f[1]))
	}
	_ = tw.Close()
	data := b.Bytes()
	l, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil })
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func imageOf(t *testing.T, platform string, l v1.Layer) v1.Image {
	t.Helper()
	img, err := mutate.AppendLayers(empty.Image, l)
	if err != nil {
		t.Fatal(err)
	}
	os, arch, _ := strings.Cut(platform, "/")
	cf, _ := img.ConfigFile()
	cf.OS, cf.Architecture = os, arch
	if img, err = mutate.ConfigFile(img, cf); err != nil {
		t.Fatal(err)
	}
	return img
}

// pushIndex pushes an index of one image per platform, each holding file, and returns the
// index's reference by digest and each image's.
func (e *env) pushIndex(t *testing.T, files map[string][]byte) (string, map[string]name.Digest) {
	t.Helper()
	repo := e.host + "/notes/notes-installer"
	var idx v1.ImageIndex = empty.Index
	idx = mutate.IndexMediaType(idx, types.OCIImageIndex)
	images := map[string]name.Digest{}
	for _, p := range []string{"linux/amd64", "linux/arm64", "darwin/arm64"} {
		b, ok := files[p]
		if !ok {
			continue
		}
		img := imageOf(t, p, layerOf(t, [2]string{"notes-stow", string(b)}))
		dg, _ := img.Digest()
		ref, _ := name.NewDigest(repo + "@" + dg.String())
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		images[p] = ref
		os, arch, _ := strings.Cut(p, "/")
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{Add: img, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: os, Architecture: arch}}})
	}
	dg, _ := idx.Digest()
	ref, _ := name.NewDigest(repo + "@" + dg.String())
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	return ref.String(), images
}

// signer signs digests as cosign v3 does with a key pair: an in-toto statement naming the digest
// in a DSSE sigstore bundle, pushed as an OCI referrer of the digest.
type signer struct {
	key *ecdsa.PrivateKey
	pem []byte
}

func newSigner(t *testing.T) *signer {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return &signer{key: k, pem: pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})}
}

func (s *signer) sign(t *testing.T, d name.Digest, predicate string) { s.signAs(t, d, predicate, true) }

// signAs is sign, the referrer annotated as cosign v3 annotates it when annotated is set.
func (s *signer) signAs(t *testing.T, d name.Digest, predicate string, annotated bool) {
	t.Helper()
	h, _ := v1.NewHash(d.DigestStr())
	stmt, _ := json.Marshal(map[string]any{
		"_type":         "https://in-toto.io/Statement/v1",
		"subject":       []any{map[string]any{"digest": map[string]string{"sha256": h.Hex}}},
		"predicateType": predicate,
	})
	const pt = "application/vnd.in-toto+json"
	sum := sha256.Sum256([]byte(fmt.Sprintf("DSSEv1 %d %s %d %s", len(pt), pt, len(stmt), stmt)))
	sig, err := ecdsa.SignASN1(rand.Reader, s.key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"mediaType":            BundleType,
		"verificationMaterial": map[string]any{"publicKey": map[string]string{"hint": ""}},
		"dsseEnvelope": map[string]any{
			"payload": base64.StdEncoding.EncodeToString(stmt), "payloadType": pt,
			"signatures": []any{map[string]string{"sig": base64.StdEncoding.EncodeToString(sig)}},
		},
	})
	sh, err := remote.Head(d)
	if err != nil {
		t.Fatal(err)
	}
	cfg, layer := static.NewLayer([]byte("{}"), "application/vnd.oci.empty.v1+json"), static.NewLayer(b, BundleType)
	desc := func(l v1.Layer) v1.Descriptor {
		h, _ := l.Digest()
		n, _ := l.Size()
		mt, _ := l.MediaType()
		return v1.Descriptor{MediaType: mt, Digest: h, Size: n}
	}
	for _, l := range []v1.Layer{cfg, layer} {
		if err := remote.WriteLayer(d.Context(), l); err != nil {
			t.Fatal(err)
		}
	}
	m := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, ArtifactType: BundleType, Config: desc(cfg), Layers: []v1.Descriptor{desc(layer)},
		Subject:     &v1.Descriptor{MediaType: sh.MediaType, Digest: sh.Digest, Size: sh.Size},
		Annotations: map[string]string{createdAnnotation: time.Now().UTC().Format(time.RFC3339), predicateAnnotation: predicate}}
	if !annotated {
		m.Annotations = nil
	}
	raw, _ := json.Marshal(m)
	mh, _, _ := v1.SHA256(bytes.NewReader(raw))
	if err := remote.Put(d.Context().Digest(mh.String()), rawManifest(raw)); err != nil {
		t.Fatal(err)
	}
}

type rawManifest []byte

func (r rawManifest) RawManifest() ([]byte, error)        { return r, nil }
func (r rawManifest) MediaType() (types.MediaType, error) { return types.OCIManifestSchema1, nil }

func TestGetTakesThePlatformsEntry(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	linux, darwin := payload("linux-exe", nil), payload("darwin-exe", nil)
	ref, images := e.pushIndex(t, map[string][]byte{"linux/amd64": linux, "darwin/arm64": darwin})
	dir := t.TempDir()
	got, err := Get(context.Background(), ref, Options{Platform: "darwin/arm64", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got.Short != "notes" || !bytes.Equal(fileOf(t, got.Path), darwin) || got.Image != images["darwin/arm64"].String() || len(got.Keys) != 0 || filepath.Dir(got.Path) != dir {
		t.Fatalf("got %q %q %s %v", got.Short, got.Path, got.Image, got.Keys)
	}
	if fi, err := os.Stat(got.Path); err != nil || fi.Mode() != 0o700 {
		t.Fatalf("mode %v %v", fi, err)
	}
	_, err = Get(context.Background(), ref, Options{Platform: "windows/amd64", Dir: dir})
	var np *NoPlatformError
	if !errors.As(err, &np) || !strings.Contains(err.Error(), "has no image for windows/amd64 (it has linux/amd64, darwin/arm64)") {
		t.Fatalf("a platform the index lacks: %v", err)
	}
}

// An installer a stowage before 0.40.0 published is one linux/amd64 image: taken for that
// platform alone.
func TestGetTakesAnImageOfItsPlatformAlone(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	bin := payload("exe", nil)
	img := imageOf(t, "linux/amd64", layerOf(t, [2]string{"notes-stow", string(bin)}))
	dg, _ := img.Digest()
	ref, _ := name.NewDigest(e.host + "/notes/notes-installer@" + dg.String())
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	if got, err := Get(context.Background(), ref.String(), Options{Platform: "linux/amd64", Dir: t.TempDir()}); err != nil || !bytes.Equal(fileOf(t, got.Path), bin) {
		t.Fatalf("linux/amd64: %v", err)
	}
	if _, err := Get(context.Background(), ref.String(), Options{Platform: "darwin/arm64"}); err == nil || !strings.Contains(err.Error(), "has no image for darwin/arm64 (it has linux/amd64)") {
		t.Fatalf("darwin/arm64: %v", err)
	}
}

func TestGetRefusesWhatIsNoInstaller(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	for n, c := range map[string]struct {
		layer v1.Layer
		want  string
	}{
		"two files":  {layerOf(t, [2]string{"notes-stow", string(payload("x", nil))}, [2]string{"other", "y"}), "holds one file"},
		"no -stow":   {layerOf(t, [2]string{"notes", string(payload("x", nil))}), "holds one file"},
		"in a dir":   {layerOf(t, [2]string{"bin/notes-stow", string(payload("x", nil))}), "holds one file"},
		"no payload": {layerOf(t, [2]string{"notes-stow", "just an executable"}), "no payload"},
		"no file":    {layerOf(t), "holds no file"},
	} {
		img := imageOf(t, "linux/amd64", c.layer)
		dg, _ := img.Digest()
		ref, _ := name.NewDigest(e.host + "/notes/notes-installer@" + dg.String())
		if err := remote.Write(ref, img); err != nil {
			t.Fatal(err)
		}
		dir := t.TempDir()
		if _, err := Get(context.Background(), ref.String(), Options{Platform: "linux/amd64", Dir: dir}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", n, err)
		}
		if left, _ := os.ReadDir(dir); len(left) != 0 {
			t.Errorf("%s: left %v", n, left)
		}
	}
}

// With a trust, an image is taken only signed by one key of each set, before its layer is read
// (an unsigned one is refused with nothing written); a release carrying no key is refused then.
// Without one, nothing is checked: an image signed by no key is taken, its keys read.
func TestGetChecksTheTrust(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	trusted, other := newSigner(t), newSigner(t)
	bin := payload("exe", map[string]string{"keys/release.pub": string(trusted.pem), "release.json": "{}"})
	ref, images := e.pushIndex(t, map[string][]byte{"linux/amd64": bin})
	dir := t.TempDir()
	if got, err := Get(context.Background(), ref, Options{Platform: "linux/amd64", Dir: dir}); err != nil || string(got.Keys["release.pub"]) != string(trusted.pem) {
		t.Fatalf("no trust: %v", err)
	}
	clean(t, dir)
	o := Options{Platform: "linux/amd64", Dir: dir, Trust: []Trust{{Name: "the provider's public_keys", Keys: map[string][]byte{"k": trusted.pem}}}}
	if _, err := Get(context.Background(), ref, o); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "against the provider's public_keys") {
		t.Fatalf("unsigned: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("an unsigned image written: %v", left)
	}
	other.sign(t, images["linux/amd64"], SignPredicate)
	if _, err := Get(context.Background(), ref, o); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "1 signature(s), none by a trusted key") {
		t.Fatalf("another key's: %v", err)
	}
	trusted.sign(t, images["linux/amd64"], "https://cosign.sigstore.dev/attestation/vuln/v1")
	trusted.signAs(t, images["linux/amd64"], "https://cosign.sigstore.dev/attestation/vuln/v1", false)
	if _, err := Get(context.Background(), ref, o); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("an attestation is no signature, annotated or not: %v", err)
	}
	trusted.sign(t, images["linux/amd64"], SignPredicate)
	if _, err := Get(context.Background(), ref, o); err != nil {
		t.Fatalf("signed: %v", err)
	}
	clean(t, dir)
	// Two sets: one key of each.
	o.Trust = append(o.Trust, Trust{Name: "the keys of the release installed", Keys: map[string][]byte{"o": other.pem}})
	if _, err := Get(context.Background(), ref, o); err != nil {
		t.Fatalf("signed by both: %v", err)
	}
	o.Trust[1].Keys = map[string][]byte{"n": newSigner(t).pem}
	if _, err := Get(context.Background(), ref, o); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), "against the keys of the release installed") {
		t.Fatalf("one set unmet: %v", err)
	}
	// A release carrying no key, signed: refused under a trust, taken without.
	bare := payload("bare-exe", nil)
	ref2, images2 := e.pushIndex(t, map[string][]byte{"linux/amd64": bare})
	trusted.sign(t, images2["linux/amd64"], SignPredicate)
	clean(t, dir)
	o.Trust = o.Trust[:1]
	if _, err := Get(context.Background(), ref2, o); err == nil || !strings.Contains(err.Error(), "carries no public key") {
		t.Fatalf("a release without keys under a trust: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("a refused file left: %v", left)
	}
}

// Check checks every platform's image, and reads the keys from one as it streams.
func TestCheckEveryImage(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := newSigner(t)
	bin := payload("exe", map[string]string{"keys/release.pub": string(s.pem)})
	ref, images := e.pushIndex(t, map[string][]byte{"linux/amd64": bin, "linux/arm64": bin})
	if keys, err := Check(context.Background(), ref, Options{Platform: "darwin/arm64"}); err != nil || string(keys["release.pub"]) != string(s.pem) {
		t.Fatalf("keys of another platform's image: %v %v", keys, err)
	}
	o := Options{Platform: "linux/amd64", Trust: []Trust{{Name: "public_keys", Keys: map[string][]byte{"k": s.pem}}}}
	s.sign(t, images["linux/amd64"], SignPredicate)
	if _, err := Check(context.Background(), ref, o); !errors.Is(err, ErrUnsigned) || !strings.Contains(err.Error(), images["linux/arm64"].DigestStr()) {
		t.Fatalf("one image unsigned: %v", err)
	}
	s.sign(t, images["linux/arm64"], SignPredicate)
	if _, err := Check(context.Background(), ref, o); err != nil {
		t.Fatal(err)
	}
	if err := VerifyImage(context.Background(), images["linux/arm64"].String(), o); err != nil {
		t.Fatal(err)
	}
	o.Trust[0].Keys = map[string][]byte{"n": newSigner(t).pem}
	if err := VerifyImage(context.Background(), images["linux/arm64"].String(), o); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("another key: %v", err)
	}
}

func fileOf(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func clean(t *testing.T, dir string) {
	t.Helper()
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

// tail keeps the last bytes written, whatever the writes' sizes.
func TestTail(t *testing.T) {
	t.Parallel()
	tl := &tail{max: 5}
	for _, w := range []string{"ab", "cdefghij", "k", "lmnopqrstuvwxyz"} {
		_, _ = tl.Write([]byte(w))
	}
	if string(tl.bytes()) != "vwxyz" {
		t.Fatalf("%q", tl.bytes())
	}
	if tl := (&tail{max: 5}); string(append(tl.bytes(), "x"...)) != "x" {
		t.Fatal("empty")
	}
}

func TestPayloadKeys(t *testing.T) {
	t.Parallel()
	keys, err := PayloadKeys(payload("exe", map[string]string{"keys/a.pub": "A", "keys/b.pub": "B", "keys/": "", "spec.json": "{}"}))
	if err != nil || len(keys) != 2 || string(keys["a.pub"]) != "A" || string(keys["b.pub"]) != "B" {
		t.Fatalf("%v %v", keys, err)
	}
	bad := payload("exe", nil)
	binary.BigEndian.PutUint64(bad[len(bad)-len(payloadMagic)-8:], 1<<40)
	if _, err := PayloadKeys(bad); err == nil || !strings.Contains(err.Error(), "not within the file") {
		t.Fatalf("a length past the file: %v", err)
	}
	if _, err := PayloadKeys([]byte("short")); err == nil {
		t.Fatal("no payload taken")
	}
}

// A login is sent to its own host alone; the machine's configuration reads any other.
func TestGetLogsInToItsHost(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir()) // the machine's own configuration holds nothing
	e := newEnv(t)
	ref, _ := e.pushIndex(t, map[string][]byte{"linux/amd64": payload("exe", nil)})
	e.auth = "ops:s3cret"
	if _, err := Get(context.Background(), ref, Options{Platform: "linux/amd64", Dir: t.TempDir(), Logins: []Login{{Host: "other.example", Username: "ops", Password: "s3cret"}}}); err == nil {
		t.Fatal("another host's login sent")
	}
	if _, err := Get(context.Background(), ref, Options{Platform: "linux/amd64", Dir: t.TempDir(), Logins: []Login{{Host: e.host, Username: "ops", Password: "s3cret"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestPlainHTTPOnlyOnLoopback(t *testing.T) {
	t.Parallel()
	called := false
	rt := httpsOnly{roundTrip(func(*http.Request) (*http.Response, error) { called = true; return nil, errors.New("sent") })}
	for _, u := range []string{"http://192.168.1.5:5000/v2/", "http://registry.example/v2/"} {
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, err := rt.RoundTrip(r); err == nil || !strings.Contains(err.Error(), "over HTTPS") || called {
			t.Fatalf("%s: %v", u, err)
		}
	}
	for _, u := range []string{"http://127.0.0.1:5000/v2/", "http://localhost:5000/v2/", "http://[::1]:5000/v2/", "https://192.168.1.5/v2/"} {
		called = false
		r, _ := http.NewRequest(http.MethodGet, u, nil)
		if _, _ = rt.RoundTrip(r); !called {
			t.Fatalf("%s refused", u)
		}
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCandidatesOrder(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d := func(n string, pred string, at time.Time) v1.Descriptor {
		ann := map[string]string{}
		if pred != "" {
			ann[predicateAnnotation] = pred
			ann[createdAnnotation] = at.UTC().Format(time.RFC3339)
		}
		h, _ := v1.NewHash("sha256:" + strings.Repeat(n, 64))
		return v1.Descriptor{Digest: h, Annotations: ann}
	}
	got, total := candidates([]v1.Descriptor{
		d("a", "", now), d("b", SignPredicate, now.Add(-time.Hour)), d("c", "https://other/v1", now), d("d", SignPredicate, now), d("e", SignPredicate, now.Add(48*time.Hour)),
	}, now)
	var order []string
	for _, g := range got {
		order = append(order, g.Digest.Hex[:1])
	}
	if strings.Join(order, "") != "dbea" || total != 4 {
		t.Fatalf("%v %d", order, total)
	}
}
