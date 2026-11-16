package fetch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
)

// BundleType is the artifact type of a sigstore bundle referrer, as cosign v3 writes it.
const BundleType = "application/vnd.dev.sigstore.bundle.v0.3+json"

// SignPredicate is the in-toto predicate type of a `cosign sign` signature: an attestation the
// same key made over the digest (an SBOM) does not approve it.
const SignPredicate = "https://sigstore.dev/cosign/sign/v1"

// maxBundles bounds the bundles read for one digest: of more candidates, the newest are read.
const maxBundles = 64

// maxBundle bounds one bundle read: a signature's is a few KiB; a larger one (an SBOM's
// attestation) signs nothing.
const maxBundle = 1 << 20

const (
	predicateAnnotation = "dev.sigstore.bundle.predicateType"
	createdAnnotation   = "org.opencontainers.image.created"
	clockSkew           = time.Hour
)

// ErrUnsigned is the answer "no signature by a trusted key", as opposed to a failure to check.
var ErrUnsigned = errors.New("no signature by a trusted key")

// Verifier checks signatures against a fixed set of public keys.
type Verifier struct {
	keys []*verify.Verifier
}

// NewVerifier trusts the PEM public keys given, by name.
func NewVerifier(pems map[string][]byte) (*Verifier, error) {
	if len(pems) == 0 {
		return nil, errors.New("no public key to verify with")
	}
	names := make([]string, 0, len(pems))
	for n := range pems {
		names = append(names, n)
	}
	sort.Strings(names)
	out := &Verifier{}
	for _, n := range names {
		pub, err := cryptoutils.UnmarshalPEMToPublicKey(pems[n])
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		sv, err := signature.LoadDefaultVerifier(pub)
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		// No validity window: a key is trusted while the release lists it.
		ek := root.NewExpiringKey(sv, time.Time{}, time.Time{})
		tm := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) { return ek, nil })
		v, err := verify.NewVerifier(tm, verify.WithNoObserverTimestamps())
		if err != nil {
			return nil, fmt.Errorf("public key %s: %w", n, err)
		}
		out.keys = append(out.keys, v)
	}
	return out, nil
}

// candidates are the referrers a check tries, in order, at most maxBundles: those annotated as a
// signature, newest first (undated, or dated past now and clockSkew, after the dated), then those
// annotated with no predicate (another tool's), in the registry's order. total counts them all.
func candidates(refs []v1.Descriptor, now time.Time) (read []v1.Descriptor, total int) {
	type ref struct {
		d       v1.Descriptor
		created time.Time
	}
	var annotated []ref
	var bare []v1.Descriptor
	for _, d := range refs {
		switch p := d.Annotations[predicateAnnotation]; {
		case p == "":
			bare = append(bare, d)
		case p == SignPredicate:
			c, err := time.Parse(time.RFC3339, d.Annotations[createdAnnotation])
			if err != nil || c.After(now.Add(clockSkew)) {
				c = time.Time{}
			}
			annotated = append(annotated, ref{d, c})
		}
	}
	sort.SliceStable(annotated, func(i, j int) bool { return annotated[i].created.After(annotated[j].created) })
	for _, r := range annotated {
		read = append(read, r.d)
	}
	read = append(read, bare...)
	total = len(read)
	if total > maxBundles {
		read = read[:maxBundles]
	}
	return read, total
}

// Verify returns nil when d carries a signature by one of the keys over d itself; an error
// wrapping ErrUnsigned when it carries none; another error when it could not tell (a referrer
// listed and not read is a failure to check, never "unsigned").
func (v *Verifier) Verify(ctx context.Context, d name.Digest, opts []remote.Option) error {
	h, err := v1.NewHash(d.DigestStr())
	if err != nil {
		return err
	}
	if h.Algorithm != "sha256" {
		return fmt.Errorf("%s: only sha256 digests are verified", d)
	}
	sum, err := hex.DecodeString(h.Hex)
	if err != nil {
		return fmt.Errorf("%s: %w", d, err)
	}
	// Every referrer is listed: a registry may report another artifact type than the manifest's
	// (its config's), and bundleOf reads the manifest's own.
	idx, err := remote.Referrers(d, opts...)
	if err != nil {
		return fmt.Errorf("%s: cannot check signatures: %w", d, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return fmt.Errorf("%s: cannot check signatures: %w", d, err)
	}
	refs, total := candidates(im.Manifests, time.Now())
	if len(refs) == 0 {
		return fmt.Errorf("%s: no signature: %w", d, ErrUnsigned)
	}
	policy := verify.NewPolicy(verify.WithArtifactDigest("sha256", sum), verify.WithKey())
	var readErr error
	signatures := len(refs)
	for _, ref := range refs {
		data, err := bundleOf(d.Context().Digest(ref.Digest.String()), opts)
		if errors.Is(err, errNotBundle) {
			signatures--
			continue
		}
		if err != nil {
			readErr = err
			continue
		}
		var b bundle.Bundle
		if err := b.UnmarshalJSON(data); err != nil {
			continue // not a bundle this reads: it signs nothing it can trust
		}
		for _, k := range v.keys {
			if res, err := k.Verify(&b, policy); err == nil && res.Statement != nil && res.Statement.PredicateType == SignPredicate {
				return nil
			}
		}
	}
	if readErr != nil {
		return fmt.Errorf("%s: cannot check signatures: %w", d, readErr)
	}
	if total > len(refs) {
		return fmt.Errorf("%s: cannot check signatures: none of the newest %d of %d read is by a trusted key, and the others were not", d, len(refs), total)
	}
	if signatures == 0 {
		return fmt.Errorf("%s: no signature: %w", d, ErrUnsigned)
	}
	return fmt.Errorf("%s: %d signature(s), none by a trusted key: %w", d, signatures, ErrUnsigned)
}

// errNotBundle is a referrer that holds no bundle a signature is: another artifact type, more
// than one layer, or one too large.
var errNotBundle = errors.New("not a signature bundle")

// bundleOf is the bundle a referrer's manifest carries: of the bundle's artifact type, its one
// layer of that type, read whole and checked against its digest.
func bundleOf(ref name.Digest, opts []remote.Option) ([]byte, error) {
	desc, err := remote.Get(ref, opts...)
	if err != nil {
		return nil, err
	}
	var m struct {
		ArtifactType string          `json:"artifactType"`
		Layers       []v1.Descriptor `json:"layers"`
	}
	if err := json.Unmarshal(desc.Manifest, &m); err != nil {
		return nil, errNotBundle
	}
	if m.ArtifactType != BundleType || len(m.Layers) != 1 || string(m.Layers[0].MediaType) != BundleType || m.Layers[0].Size > maxBundle {
		return nil, errNotBundle
	}
	l, err := remote.Layer(ref.Context().Digest(m.Layers[0].Digest.String()), opts...)
	if err != nil {
		return nil, err
	}
	rc, err := l.Compressed() // the blob as stored, checked against its digest as it is read
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxBundle {
		return nil, errNotBundle
	}
	return b, nil
}
