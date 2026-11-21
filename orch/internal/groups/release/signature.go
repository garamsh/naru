package release

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// trustedRootJSON is the public-good Sigstore trusted root, as the TUF client of sigstore-go
// v1.3.0 served it on 2026-11-21. docs/architecture/orch.md names how it is refreshed.
//
//go:embed trusted_root.json
var trustedRootJSON []byte

// signature checks the cosign keyless bundle over a SHA256SUMS against one identity.
type signature struct {
	verifier *verify.Verifier
	identity verify.CertificateIdentity
	// parse reads a bundle; a test replaces it.
	parse func(raw []byte) (verify.SignedEntity, error)
}

// newSignature is the check against the trusted root at rootPath, or the embedded one when
// rootPath is empty, for the certificates issued to identity by issuer. An identity that ends
// in "*" is a prefix.
func newSignature(rootPath, issuer, identity string) (signature, error) {
	var tm root.TrustedMaterial
	var err error
	if rootPath == "" {
		tm, err = root.NewTrustedRootFromJSON(trustedRootJSON)
	} else {
		tm, err = root.NewTrustedRootFromPath(rootPath)
	}
	if err != nil {
		return signature{}, fmt.Errorf("boot set trust root: %w", err)
	}
	return newSignatureWith(tm, issuer, identity,
		verify.WithTransparencyLog(1), verify.WithObserverTimestamps(1), verify.WithSignedCertificateTimestamps(1))
}

// newSignatureWith is the check against tm with the given requirements of the verifier.
func newSignatureWith(tm root.TrustedMaterial, issuer, identity string, opts ...verify.VerifierOption) (signature, error) {
	if issuer == "" || identity == "" {
		return signature{}, errors.New("boot set store needs both the issuer and the identity its releases are signed by")
	}
	san, sanRegex := identity, ""
	if p, ok := strings.CutSuffix(identity, "*"); ok {
		san, sanRegex = "", "^"+regexpQuote(p)+".*$"
	}
	id, err := verify.NewShortCertificateIdentity(issuer, "", san, sanRegex)
	if err != nil {
		return signature{}, fmt.Errorf("boot set identity: %w", err)
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return signature{}, fmt.Errorf("boot set trust root: %w", err)
	}
	return signature{verifier: v, identity: id, parse: parseBundle}, nil
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func parseBundle(raw []byte) (verify.SignedEntity, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &b, nil
}

// check is nil only when raw is a bundle that signs sums, with an inclusion proof, by the
// identity. It needs no network.
func (s signature) check(raw, sums []byte) error {
	e, err := s.parse(raw)
	if err != nil {
		return fmt.Errorf("%w: unreadable bundle: %v", ErrSignature, err)
	}
	if !e.HasInclusionProof() {
		return fmt.Errorf("%w: the bundle carries no inclusion proof", ErrSignature)
	}
	_, err = s.verifier.Verify(e, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(sums)),
		verify.WithCertificateIdentity(s.identity)))
	var wrong *verify.ErrNoMatchingCertificateIdentity
	switch {
	case err == nil:
		return nil
	case errors.As(err, &wrong):
		return fmt.Errorf("%w: %v", ErrIdentity, err)
	default:
		return fmt.Errorf("%w: %v", ErrSignature, err)
	}
}
