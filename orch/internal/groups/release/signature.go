package release

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
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
	issuer   string
	// identity is the signer, or a prefix ending in "*" (see checkIdentity); empty means the
	// release workflow at the tag of the version checked.
	identity string
	// parse reads a bundle; a test replaces it.
	parse func(raw []byte) (verify.SignedEntity, error)
}

// defaultIdentity is the release workflow of this repository at the tag of version.
func defaultIdentity(version string) string {
	return "https://github.com/garamsh/naru/.github/workflows/release.yml@refs/tags/" + version
}

// githubRepoPrefix matches the start of a prefix identity that names one GitHub repository: the
// issuer is shared by every repository, so a shorter prefix accepts workflows of others.
var githubRepoPrefix = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/`)

// checkIdentity refuses an identity that is a prefix (ends in "*") shorter than
// https://github.com/<owner>/<repo>/, or that has a "*" elsewhere.
func checkIdentity(identity string) error {
	p, isPrefix := strings.CutSuffix(identity, "*")
	if strings.Contains(p, "*") {
		return errors.New("boot set identity may have a * only at its end")
	}
	if isPrefix && !githubRepoPrefix.MatchString(p) {
		return errors.New("boot set identity prefix must start with https://github.com/<owner>/<repo>/ (the trailing slash is required)")
	}
	return nil
}

// newSignature is the check against the trusted root at rootPath, or the embedded one when
// rootPath is empty, for the certificates issued by issuer to identity, or to the release
// workflow at the tag of the version when identity is empty.
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
	if issuer == "" {
		return signature{}, errors.New("boot set store needs the issuer its releases are signed under")
	}
	if err := checkIdentity(identity); err != nil {
		return signature{}, err
	}
	v, err := verify.NewVerifier(tm, opts...)
	if err != nil {
		return signature{}, fmt.Errorf("boot set trust root: %w", err)
	}
	return signature{verifier: v, issuer: issuer, identity: identity, parse: parseBundle}, nil
}

// certificateIdentity is what a certificate must name to have signed the release of version.
func (s signature) certificateIdentity(version string) (verify.CertificateIdentity, error) {
	identity := s.identity
	if identity == "" {
		identity = defaultIdentity(version)
	}
	san, sanRegex := identity, ""
	if p, ok := strings.CutSuffix(identity, "*"); ok {
		san, sanRegex = "", "^"+regexp.QuoteMeta(p)+".*$"
	}
	return verify.NewShortCertificateIdentity(s.issuer, "", san, sanRegex)
}

func parseBundle(raw []byte) (verify.SignedEntity, error) {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return &b, nil
}

// check is nil only when raw is a bundle that signs sums, with an inclusion proof, by the
// identity for version. It needs no network.
func (s signature) check(raw, sums []byte, version string) error {
	id, err := s.certificateIdentity(version)
	if err != nil {
		return fmt.Errorf("%w: boot set identity: %v", ErrIdentity, err)
	}
	e, err := s.parse(raw)
	if err != nil {
		return fmt.Errorf("%w: unreadable bundle: %v", ErrSignature, err)
	}
	if !e.HasInclusionProof() {
		return fmt.Errorf("%w: the bundle carries no inclusion proof", ErrSignature)
	}
	_, err = s.verifier.Verify(e, verify.NewPolicy(verify.WithArtifact(bytes.NewReader(sums)),
		verify.WithCertificateIdentity(id)))
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
