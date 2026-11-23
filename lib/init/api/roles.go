package api

import (
	"crypto/x509"
	"net/http"

	"github.com/garamsh/naru/lib/identity"
)

// clientCertificate returns the verified certificate of the request's client. The listener
// requires one, so a request without it has no role.
func clientCertificate(r *http.Request) *x509.Certificate {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
		return r.TLS.VerifiedChains[0][0]
	}
	return &x509.Certificate{}
}

// Role is what a client may do, taken from its certificate's Organization, or from its
// OrganizationalUnit when the signer issued it (ADR 0100).
type Role int

const (
	// None may do nothing: the certificate is trusted but names no role.
	None Role = iota
	// Reader may read.
	Reader
	// Admin may read and apply.
	Admin
)

// The Organizations that grant a role.
const (
	adminOrganization  = "naru:admin"
	readerOrganization = "naru:reader"
)

// roleOf returns the highest role among the Organizations of cert and the role of the signer's it
// carries, which the table shared with the host API gives (identity.PersonAccess).
func roleOf(cert *x509.Certificate) Role {
	role := None
	switch identity.PersonAccess(cert) {
	case identity.AccessWrite:
		role = Admin
	case identity.AccessRead:
		role = Reader
	}
	for _, o := range cert.Subject.Organization {
		switch o {
		case adminOrganization:
			role = max(role, Admin)
		case readerOrganization:
			role = max(role, Reader)
		}
	}
	return role
}
