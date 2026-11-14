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
// OrganizationalUnit when that is orch's role.
type Role int

const (
	// None may do nothing: the certificate is trusted but names no role.
	None Role = iota
	// Reader may read.
	Reader
	// Provisioner may read, create and destroy microVMs.
	Provisioner
)

// The Organizations that grant a role. naructl gen host-client writes them.
const (
	ProvisionerOrganization = "naru:host-provisioner"
	ReaderOrganization      = "naru:host-reader"
)

// roleOf returns the highest role among the Organizations of cert. A certificate that carries
// lib/identity's orch role, as naru-signer issues to orch, is a provisioner: orch creates, stops,
// starts and deletes microVMs and sets networks and peers on every host it admitted.
func roleOf(cert *x509.Certificate) Role {
	role := None
	if r, err := identity.RoleOf(cert); err == nil && r == identity.RoleOrch {
		role = Provisioner
	}
	for _, o := range cert.Subject.Organization {
		switch o {
		case ProvisionerOrganization:
			role = max(role, Provisioner)
		case ReaderOrganization:
			role = max(role, Reader)
		}
	}
	return role
}
