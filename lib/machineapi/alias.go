package machineapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AliasRequest is the body of PUT .../aliases/{hostname}: the caller's Machine, the service the
// hostname reaches, the default service when Service is empty, and a PEM certificate for the
// hostname with its PEM key.
type AliasRequest struct {
	Machine     string `json:"machine"`
	Service     string `json:"service,omitempty"`
	Certificate string `json:"certificate,omitempty"`
	Key         string `json:"key,omitempty"`
}

// Alias is an alias as a read answers it. The key is never returned: Key is Redacted when the
// alias has one.
type Alias struct {
	Hostname    string            `json:"hostname"`
	Machine     string            `json:"machine"`
	Service     string            `json:"service"`
	Certificate *AliasCertificate `json:"certificate,omitempty"`
	Key         string            `json:"key,omitempty"`
}

// AliasCertificate is what a read shows of an alias's certificate: the names it covers and when
// it expires.
type AliasCertificate struct {
	Names    []string  `json:"names"`
	NotAfter time.Time `json:"notAfter"`
}

// ErrAliasForm is the refusal of a hostname naru does not take as an alias.
var ErrAliasForm = errors.New("not a hostname naru takes as an alias")

// CheckAliasHostname refuses with ErrAliasForm a hostname that is not a lowercase DNS name or is in
// zone. In the zone naru answers only Machines' stable names: the zone is shared, and a policy for
// other names in it is the zone's, not naru's (ADR 0120).
func CheckAliasHostname(hostname, zone string) error {
	if len(hostname) > 253 {
		return fmt.Errorf("%w: %q is longer than 253 characters", ErrAliasForm, hostname)
	}
	for _, l := range strings.Split(hostname, ".") {
		if CheckName(l) != nil {
			return fmt.Errorf("%w: %q is not a lowercase DNS name", ErrAliasForm, hostname)
		}
	}
	if InDomain(hostname, zone) {
		return fmt.Errorf("%w: %q is in the zone %s, where naru takes no alias", ErrAliasForm, hostname, zone)
	}
	return nil
}

// InDomain reports whether hostname is domain or a name under it.
func InDomain(hostname, domain string) bool {
	return hostname == domain || strings.HasSuffix(hostname, "."+domain)
}

// ErrAliasCertificate is the refusal of an alias's certificate or key.
var ErrAliasCertificate = errors.New("the alias's certificate is refused")

// CheckAliasCertificate parses the PEM certificate and key of an alias outside the zone and
// refuses, with ErrAliasCertificate, a pair that does not match, a certificate that does not cover
// hostname, or one not valid at now. It returns what a read shows of the certificate.
func CheckAliasCertificate(hostname, certPEM, keyPEM string, now time.Time) (AliasCertificate, error) {
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	if err != nil {
		return AliasCertificate{}, fmt.Errorf("%w: %v", ErrAliasCertificate, err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return AliasCertificate{}, fmt.Errorf("%w: %v", ErrAliasCertificate, err)
	}
	if err := leaf.VerifyHostname(hostname); err != nil {
		return AliasCertificate{}, fmt.Errorf("%w: %v", ErrAliasCertificate, err)
	}
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return AliasCertificate{}, fmt.Errorf("%w: it is valid from %s to %s", ErrAliasCertificate,
			leaf.NotBefore.Format(time.RFC3339), leaf.NotAfter.Format(time.RFC3339))
	}
	return AliasCertificate{Names: leaf.DNSNames, NotAfter: leaf.NotAfter.UTC()}, nil
}
