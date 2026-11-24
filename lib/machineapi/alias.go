package machineapi

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AliasRequest is the body of PUT .../aliases/{hostname}: the caller's Machine and the service the
// hostname reaches, the default service when Service is empty. An alias outside the zone carries
// a PEM certificate for the hostname and its PEM key; one inside the zone carries neither.
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

// ErrAliasForm is the refusal of a hostname the zone's rules do not admit.
var ErrAliasForm = errors.New("not an alias the zone admits")

// AliasInZone reports whether hostname is an alias inside zone, and refuses with ErrAliasForm one
// that is not a lowercase DNS name or, inside the zone, is not one label, is 16 hexadecimal digits
// (a Machine's id) or holds "--" (a service's name) (ADR 0112 item 3).
func AliasInZone(hostname, zone string) (bool, error) {
	labels := strings.Split(hostname, ".")
	if len(hostname) > 253 {
		return false, fmt.Errorf("%w: %q is longer than 253 characters", ErrAliasForm, hostname)
	}
	for _, l := range labels {
		if CheckName(l) != nil {
			return false, fmt.Errorf("%w: %q is not a lowercase DNS name", ErrAliasForm, hostname)
		}
	}
	if hostname == zone {
		return false, fmt.Errorf("%w: %q is the zone itself", ErrAliasForm, hostname)
	}
	label, ok := strings.CutSuffix(hostname, "."+zone)
	if !ok {
		return false, nil
	}
	switch {
	case strings.Contains(label, "."):
		return true, fmt.Errorf("%w: %q is more than one label under %s", ErrAliasForm, hostname, zone)
	case IsMachineID(label):
		return true, fmt.Errorf("%w: %q is a Machine's stable name", ErrAliasForm, label)
	case strings.Contains(label, "--"):
		return true, fmt.Errorf("%w: %q holds \"--\", which names a service", ErrAliasForm, label)
	}
	return true, nil
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
