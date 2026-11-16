package provider

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/norvexlabs/terraform-provider-stowage/internal/fetch"
)

// publicKeyPEM is a fresh public key's PEM.
func publicKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func strList(vs ...string) tftypes.Value {
	var l []tftypes.Value
	for _, v := range vs {
		l = append(l, str(v))
	}
	return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, l)
}

// configureDiags configures the provider with config, and returns its diagnostics.
func configureDiags(t *testing.T, config map[string]tftypes.Value) []*tfprotov6.Diagnostic {
	t.Helper()
	f := newFake()
	srv, err := providerserver.NewProtocol6WithError(newIn("test", f.factory, time.Millisecond, t.TempDir(), t.TempDir(), f)())()
	if err != nil {
		t.Fatal(err)
	}
	s, err := srv.GetProviderSchema(context.Background(), &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	typ := s.Provider.ValueType().(tftypes.Object)
	v, err := tfprotov6.NewDynamicValue(typ, fill(typ, config))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.ConfigureProvider(context.Background(), &tfprotov6.ConfigureProviderRequest{Config: &v})
	if err != nil {
		t.Fatal(err)
	}
	return resp.Diagnostics
}

// public_keys holds PEM public keys alone, each refused by its index.
func TestPublicKeysArePEMPublicKeys(t *testing.T) {
	t.Parallel()
	if d := configureDiags(t, map[string]tftypes.Value{"public_keys": strList(publicKeyPEM(t))}); len(d) != 0 {
		t.Fatalf("a public key refused: %s", diagText(d))
	}
	d := configureDiags(t, map[string]tftypes.Value{"public_keys": strList(publicKeyPEM(t), "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----\n")})
	if got := errorsOf(d); !strings.Contains(got, "public_keys[1] is not a PEM public key") {
		t.Fatalf("got %q", got)
	}
}

// A first install's plan warns that the installer is trusted by its digest alone, unless
// public_keys is given; with it, the installer is checked before the target fetches it, and an
// installer it does not check is never fetched.
func TestDockerCreateChecksThePublicKeys(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	if w := warningsOf(h.plan(nil, h.dockerConfig(nil)).Diagnostics); !strings.Contains(w, "trusted by its digest alone") ||
		!strings.Contains(w, "checked against the keys the release installed carries") ||
		!strings.Contains(w, "a release that carries none, its project not signing, leaves every upgrade over it checked by the digest installer names alone as well") ||
		strings.Contains(w, "whatever public_keys holds") {
		t.Fatalf("no warning without public_keys: %q", w)
	}
	key := publicKeyPEM(t)
	h = newHarness(t, map[string]tftypes.Value{"public_keys": strList(key)})
	h.fake.keys = map[string]map[string][]byte{image: {"release.pub": []byte(key)}}
	if w := warningsOf(h.plan(nil, h.dockerConfig(nil)).Diagnostics); strings.Contains(w, "digest alone") {
		t.Fatalf("a warning with public_keys: %q", w)
	}
	h.create(h.dockerConfig(nil))
	if !slices.Equal(h.fake.checks, []string{image + " against the provider's public_keys"}) {
		t.Fatalf("checks %q", h.fake.checks)
	}

	h = newHarness(t, map[string]tftypes.Value{"public_keys": strList(key)})
	h.fake.checkErr = map[string]error{image: errors.New("no signature by a trusted key")}
	config := h.dockerConfig(nil)
	a := h.apply(nil, h.plan(nil, config).PlannedState, config)
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "Checking the installer's signature") || len(h.fake.callsOf("fetch")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
}

// An upgrade's new installer is checked against the keys the release installed carries (its
// installer by the digest status --json records, read from the registry, else from the installer
// the install keeps); a release installed that carries none checks nothing but public_keys, said
// in a warning; keys that cannot be read refuse the upgrade before anything is fetched, without
// public_keys, and check the new installer against public_keys alone with them.
func TestDockerUpgradeChecksTheInstalledKeys(t *testing.T) {
	t.Parallel()
	key := []byte(publicKeyPEM(t))
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	_ = tw.WriteHeader(&tar.Header{Name: "keys/r.pub", Mode: 0o644, Size: int64(len(key)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(key)
	_ = tw.Close()
	var empty bytes.Buffer
	_ = tar.NewWriter(&empty).Close()
	unreachable := errors.New("dial tcp: registry.example.com: no route to host")
	for name, c := range map[string]struct {
		installedKeys map[string][]byte
		keysErr       error
		kept          reply // the kept installer's payload, read on the target
		checkErr      error
		publicKeys    bool
		checks        []string
		want, warn    string
		upgraded      bool // the upgrade ran
	}{
		"signed":         {installedKeys: map[string][]byte{"r.pub": key}, checks: []string{image2 + " against the keys of the release installed (" + image + ")"}},
		"not signed":     {installedKeys: map[string][]byte{"r.pub": key}, checkErr: errors.New("no signature by a trusted key"), checks: []string{image2 + " against the keys of the release installed (" + image + ")"}, want: "Checking the installer's signature"},
		"installed none": {warn: "The release installed carries no key (read from its installer image, " + image + "): the upgrade is checked by its digest alone (installer)."},
		"installed none, public_keys": {publicKeys: true, checks: []string{image + " against the provider's public_keys", image2 + " against the provider's public_keys"},
			warn: "The release installed carries no key (read from its installer image, " + image + "): the upgrade is checked by the provider's public_keys alone."},
		"kept carries none": {keysErr: unreachable, kept: reply{stdout: empty.String()},
			warn: "The release installed carries no key (read from the installer the install keeps, /srv/notes/notes-stow, as its installer image (" + image + ") could not be read from its registry: dial tcp: registry.example.com: no route to host): the upgrade is checked by its digest alone (installer)."},
		// The registry cannot give them: the kept installer's are read on the target.
		"read on the target": {keysErr: unreachable, kept: reply{stdout: tb.String()}, checks: []string{image2 + " against the keys of the release installed (" + image + ")"},
			warn: "could not be read from its registry (dial tcp: registry.example.com: no route to host): the new release is checked against the keys the installer the install keeps carries (/srv/notes/notes-stow)"},
		"neither": {keysErr: unreachable, kept: reply{exit: 1, stderr: "./notes/notes-stow: no installer kept there"},
			want: "neither could be read: the installer image the install records (" + image + ") from its registry: dial tcp: registry.example.com: no route to host; the installer the install keeps on the target (/srv/notes/notes-stow, its payload's keys/): reading the installer the install keeps"},
		// With public_keys, the upgrade goes on under them alone.
		"neither, public_keys": {keysErr: unreachable, kept: reply{exit: 1, stderr: "./notes/notes-stow: no installer kept there"}, publicKeys: true,
			checks: []string{image + " against the provider's public_keys", image2 + " against the provider's public_keys"}, upgraded: true,
			warn: "the installer the install keeps on the target (/srv/notes/notes-stow, its payload's keys/): reading the installer the install keeps failed (exit 1)\n./notes/notes-stow: no installer kept there): the new release is checked against the provider's public_keys alone."},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var config map[string]tftypes.Value
			if c.publicKeys {
				config = map[string]tftypes.Value{"public_keys": strList(string(key))}
			}
			h := newHarness(t, config)
			h.fake.keys = map[string]map[string][]byte{image: {"r.pub": key}}
			state := h.create(h.dockerConfig(nil))
			h.fake.reset()
			h.fake.keys = map[string]map[string][]byte{image: c.installedKeys, image2: {"r.pub": key}}
			h.fake.keysErr = map[string]error{image: c.keysErr}
			h.fake.set("kept-payload", c.kept)
			h.fake.checkErr = map[string]error{image2: c.checkErr}
			h.fake.set("describe", reply{stdout: describe2})
			h.fake.then("status", reply{stdout: h.fake.replies["status"].stdout})
			h.fake.set("status", reply{stdout: status2})
			a := h.update(state, h.dockerConfig(map[string]tftypes.Value{"installer": str(image2)}))
			if got := errorsOf(a.Diagnostics); c.want == "" && got != "" || !strings.Contains(got, c.want) {
				t.Fatalf("got %q", got)
			}
			if !slices.Equal(h.fake.checks, c.checks) || !slices.Equal(h.fake.keyReads, []string{image}) {
				t.Fatalf("checks %q, keys read %q", h.fake.checks, h.fake.keyReads)
			}
			if w := warningsOf(a.Diagnostics); c.warn == "" && (strings.Contains(w, "read on the target") || strings.Contains(w, "carries no key")) || !strings.Contains(w, c.warn) {
				t.Fatalf("warnings %q", w)
			}
			if read := len(h.fake.callsOf("kept-payload")); (c.keysErr != nil) != (read > 0) {
				t.Fatalf("the kept installer read %d times", read)
			}
			if c.want != "" && (len(h.fake.callsOf("fetch")) != 0 || len(h.fake.callsOf("upgrade")) != 0) {
				t.Fatalf("fetched or upgraded: %q", h.fake.ops())
			}
			if c.upgraded && len(h.fake.callsOf("upgrade")) != 1 {
				t.Fatalf("not upgraded: %q", h.fake.ops())
			}
			if strings.Contains(c.want, "neither") && (!strings.Contains(c.want+errorsOf(a.Diagnostics), "Nothing was fetched or run") || !strings.Contains(errorsOf(a.Diagnostics), "set the provider's public_keys")) {
				t.Fatalf("the refusal's fixes: %q", errorsOf(a.Diagnostics))
			}
		})
	}
}

// On Kubernetes, the new release's installer is fetched against the keys of the running one's.
func TestK8sUpgradeChecksTheInstalledKeys(t *testing.T) {
	t.Parallel()
	key := []byte(publicKeyPEM(t))
	h := k8sHarness(t, nil)
	h.fake.then("k8s-status", reply{exit: 3, stdout: noReleaseJSON, stderr: noRelease})
	state := h.create(h.k8sConfig(nil))
	h.fake.reset()
	h.fake.keys = map[string]map[string][]byte{image: {"r.pub": key}, image2: {"r.pub": key}}
	h.fake.checkErr = map[string]error{image2: errors.New("no signature by a trusted key")}
	h.fake.then("k8s-describe", reply{stdout: describeK8s}, reply{stdout: describeK8s2})
	h.fake.then("k8s-status", reply{stdout: statusK8s})
	h.fake.set("k8s-status", reply{stdout: statusK8s2})
	a := h.update(state, h.k8sConfig(map[string]tftypes.Value{"installer": str(image2)}))
	if got := errorsOf(a.Diagnostics); !strings.Contains(got, "no signature by a trusted key") || len(h.fake.callsOf("k8s-upgrade")) != 0 {
		t.Fatalf("got %q, calls %q", got, h.fake.ops())
	}
	if !slices.Equal(h.fake.checks, []string{image2 + " against the keys of the release installed (" + image + ")"}) {
		t.Fatalf("checks %q", h.fake.checks)
	}
}

// The pull account goes to the registry of an installer the configuration names, or of the
// provider's registry, alone; the mirror's own login to the mirror.
func TestFetchLoginHosts(t *testing.T) {
	t.Parallel()
	d := &Data{Username: "pull", Password: "p"}
	if l := d.fetchLogin("registry.example.com", image); l == nil || l.Username != "pull" || l.Host != "registry.example.com" {
		t.Fatalf("the configured installer's host: %v", l)
	}
	for _, host := range []string{"evil.example", "registry.example.com.evil.example"} {
		if l := d.fetchLogin(host, image); l != nil {
			t.Fatalf("%s got the account", host)
		}
	}
	if l := d.fetchLogin("registry.example.com"); l != nil {
		t.Fatal("no installer configured, yet the account sent")
	}
	if l := d.fetchLogin("index.docker.io", "docker.io/notes/notes-installer@"+digest); l == nil {
		t.Fatal("Docker Hub's names are one host")
	}
	d = &Data{Username: "pull", Password: "p", Registry: "mirror.lan/notes"}
	if l := d.fetchLogin("mirror.lan"); l == nil || l.Username != "pull" {
		t.Fatalf("the provider's registry: %v", l)
	}
	d.RegistryUsername, d.RegistryPassword = "mirror", "m"
	if l := d.fetchLogin("mirror.lan", image); l == nil || l.Username != "mirror" {
		t.Fatalf("the mirror's own login: %v", l)
	}
	if l := d.fetchLogin("evil.example", image); l != nil {
		t.Fatalf("another host: %v", l)
	}
}

// A program the cache keeps is checked again against the trust of the run that takes it: its
// image's signature, and keys it must carry.
func TestCachedProgramIsCheckedAgainstTheTrust(t *testing.T) {
	t.Parallel()
	key := publicKeyPEM(t)
	for name, c := range map[string]struct {
		keys      [][2]string
		verifyErr error
		want      string
	}{
		"verified":       {[][2]string{{"keys/r.pub", key}}, nil, ""},
		"not verified":   {[][2]string{{"keys/r.pub", key}}, errors.New("no signature by a trusted key"), "no signature by a trusted key"},
		"carries no key": {nil, nil, "carries no public key"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gets := 0
			var verified []string
			src := nativeSource{
				get: func(_ context.Context, ref string, o fetch.Options) (*fetch.Installer, error) {
					gets++
					f, err := os.CreateTemp(o.Dir, ".x-*")
					if err != nil {
						return nil, err
					}
					_, _ = f.Write(withPayload("exe", c.keys...))
					_ = f.Close()
					return &fetch.Installer{Short: "notes", Path: f.Name(), Image: "registry.example.com/notes/notes-installer@sha256:" + strings.Repeat("e", 64)}, nil
				},
				verify: func(_ context.Context, image string, o fetch.Options) error {
					verified = append(verified, image)
					return c.verifyErr
				},
			}
			d := &Data{CacheDir: t.TempDir()}
			if _, err := src.Program(context.Background(), d, Want{Ref: image}, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			d.PublicKeys = map[string][]byte{"public_keys[0]": []byte(key)}
			p, err := src.Program(context.Background(), d, Want{Ref: image}, t.TempDir())
			if gets != 1 {
				t.Fatalf("%d reads: the cache was not taken", gets)
			}
			if c.want == "" {
				if err != nil || filepath.Dir(filepath.Dir(p.Kept)) != filepath.Join(d.CacheDir, strings.TrimPrefix(digest, "sha256:")) || len(verified) != 1 || !strings.HasSuffix(verified[0], strings.Repeat("e", 64)) {
					t.Fatalf("%v %v %q", p, err, verified)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// Keys reads the keys of a release once per digest: kept beside the cache's entries.
func TestKeysAreKept(t *testing.T) {
	t.Parallel()
	reads := 0
	src := nativeSource{check: func(_ context.Context, ref string, o fetch.Options) (map[string][]byte, error) {
		reads++
		if len(o.Trust) != 0 {
			t.Errorf("keys read under a trust")
		}
		return map[string][]byte{"r.pub": []byte("K")}, nil
	}}
	d := &Data{CacheDir: t.TempDir()}
	for range 2 {
		keys, err := src.Keys(context.Background(), d, Want{Ref: image})
		if err != nil || string(keys["r.pub"]) != "K" {
			t.Fatalf("%v %v", keys, err)
		}
	}
	if reads != 1 {
		t.Fatalf("%d reads", reads)
	}
}
