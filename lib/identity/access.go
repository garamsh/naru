package identity

import "crypto/x509"

// Access is what an API lets a client of the signer's do.
type Access int

const (
	// AccessNone lets nothing through: the certificate names no role an API gives access to.
	AccessNone Access = iota
	// AccessRead lets the client read.
	AccessRead
	// AccessWrite lets the client read and change.
	AccessWrite
)

// PersonAccess returns the access the certificate's role gives on an API that trusts the
// operator authority (ADR 0080, ADR 0100): root and operator write, reader and automation read, and
// a certificate with no single known role, or with the role of a host or another service, none. It
// is the one table the host API and the Node API read.
func PersonAccess(cert *x509.Certificate) Access {
	r, err := RoleOf(cert)
	if err != nil {
		return AccessNone
	}
	switch r {
	case RoleRoot, RoleOperator:
		return AccessWrite
	case RoleReader, RoleAutomation:
		return AccessRead
	}
	return AccessNone
}
