package config

import (
	"fmt"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// DefaultRevocationMaxAge is how old a revocation list that cannot be refreshed may be before a
// service refuses every caller. It is lib/identity's DefaultMaxAge, which orch's flag defaults to.
const DefaultRevocationMaxAge = identity.DefaultMaxAge

// The files init writes for naru-hostd's revocation list.
const (
	// HostdRevocationKeyFile holds spec.host.revocationPublicKey.
	HostdRevocationKeyFile = "/run/naru-hostd/revocation.pub"
	// HostdRevocationSeedFile holds spec.host.revocationList.
	HostdRevocationSeedFile = "/run/naru-hostd/crl-seed.json"
	// APIRevocationFile is where the Node API keeps the last list it verified, on the data disk.
	APIRevocationFile = DataMount + "/api/crl.json"
)

// RevocationFrom is where a service of the Node reads the signed revocation list: the socket of the
// signer on this Node, or else orch's GET /v1/crl at OrchURL, whose authority has the fingerprint
// OrchCASHA256.
type RevocationFrom struct {
	Socket       string
	OrchURL      string
	OrchCASHA256 string
}

// RevocationFrom returns the source of the list for this Node, and false when it has none: it runs
// no signer and enrolls with no orch.
func (s Spec) RevocationFrom() (RevocationFrom, bool) {
	switch {
	case s.Signer != nil:
		return RevocationFrom{Socket: SignerSocket}, true
	case s.Host != nil && s.Host.Orch != nil:
		return RevocationFrom{OrchURL: s.Host.Orch.URL, OrchCASHA256: s.Host.Orch.CASHA256}, true
	}
	return RevocationFrom{}, false
}

// RevocationMaxAgeOf parses a revocationMaxAge, which is DefaultRevocationMaxAge when empty.
func RevocationMaxAgeOf(s string) (time.Duration, error) {
	if s == "" {
		return DefaultRevocationMaxAge, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute || d > 24*time.Hour {
		return 0, fmt.Errorf("must be a duration from 1m to 24h, got %q", s)
	}
	return d, nil
}

// revocation checks the fields of spec.host and spec.api that name the revocation list.
func (v *validation) revocation(spec Spec) {
	check := func(section, key, maxAge string) {
		if key == "" {
			if maxAge != "" {
				v.fail(section+".revocationMaxAge", "is only used with %s.revocationPublicKey", section)
			}
			return
		}
		if _, err := identity.ParseRevocationPublic([]byte(key)); err != nil {
			v.fail(section+".revocationPublicKey", "must be an Ed25519 public key in PKIX PEM: %v", err)
		}
		if _, err := RevocationMaxAgeOf(maxAge); err != nil {
			v.fail(section+".revocationMaxAge", "%v", err)
		}
		if _, ok := spec.RevocationFrom(); !ok {
			v.fail(section+".revocationPublicKey", "needs spec.signer, to read the list from, or spec.host.orch, to fetch it from")
		}
	}
	if h := spec.Host; h != nil {
		check("spec.host", h.RevocationPublicKey, h.RevocationMaxAge)
		if h.RevocationList != "" && h.RevocationPublicKey == "" {
			v.fail("spec.host.revocationList", "is only used with spec.host.revocationPublicKey")
		}
		if h.RevocationList != "" && h.RevocationPublicKey != "" {
			if key, err := identity.ParseRevocationPublic([]byte(h.RevocationPublicKey)); err == nil {
				if _, err := identity.VerifyCRL(key, []byte(h.RevocationList)); err != nil {
					v.fail("spec.host.revocationList", "is not a list signed by spec.host.revocationPublicKey: %v", err)
				}
			}
		}
	}
	if a := spec.API; a != nil {
		check("spec.api", a.RevocationPublicKey, a.RevocationMaxAge)
	}
}
