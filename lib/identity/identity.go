// Package identity holds what naru's authorities and their callers agree on:
// the roles a certificate carries, the revocation list, the pairing code, the
// admission statement, the passphrase-encrypted person and recovery files and
// the sealed box.
package identity

import (
	"crypto/x509"
	"errors"
	"fmt"
)

// Role is what a certificate lets its holder do. It is carried in the
// certificate's Subject OrganizationalUnit, exactly one.
type Role string

// The roles. Root, Operator, Reader and Automation are held by people and
// their tools; Host and Orch by the services.
const (
	RoleRoot       Role = "root"
	RoleOperator   Role = "operator"
	RoleReader     Role = "reader"
	RoleAutomation Role = "automation"
	RoleHost       Role = "host"
	RoleOrch       Role = "orch"
)

var (
	// ErrRole is returned when a certificate does not carry exactly one known role.
	ErrRole = errors.New("certificate does not carry exactly one known role")
	// ErrRevoked is returned when a serial is on the revocation list.
	ErrRevoked = errors.New("certificate is revoked")
	// ErrNoChain is returned when there is no verified chain to read.
	ErrNoChain = errors.New("no verified certificate chain")
)

// Identity is who a caller is. Authorisation code reads only this.
type Identity struct {
	Subject string
	Role    Role
	// Serial is the certificate's serial number in lowercase hexadecimal.
	Serial string
}

// Valid reports whether r is one of the roles.
func (r Role) Valid() bool {
	switch r {
	case RoleRoot, RoleOperator, RoleReader, RoleAutomation, RoleHost, RoleOrch:
		return true
	}
	return false
}

// RoleOf returns the role in the certificate's OrganizationalUnit.
func RoleOf(cert *x509.Certificate) (Role, error) {
	ou := cert.Subject.OrganizationalUnit
	if len(ou) != 1 {
		return "", fmt.Errorf("%w: %d organizational units", ErrRole, len(ou))
	}
	r := Role(ou[0])
	if !r.Valid() {
		return "", fmt.Errorf("%w: %q", ErrRole, ou[0])
	}
	return r, nil
}

// SerialOf returns the certificate's serial number in lowercase hexadecimal.
func SerialOf(cert *x509.Certificate) string {
	return cert.SerialNumber.Text(16)
}

// Authenticate turns the verified chains of a TLS connection into an Identity.
// The leaf of the first chain names the caller; the call is refused when any
// certificate of that chain is on crl. A nil crl revokes nothing.
func Authenticate(chains [][]*x509.Certificate, crl *CRL) (Identity, error) {
	if len(chains) == 0 || len(chains[0]) == 0 {
		return Identity{}, ErrNoChain
	}
	chain := chains[0]
	for _, c := range chain {
		if crl.Revokes(SerialOf(c)) {
			return Identity{}, fmt.Errorf("%w: serial %s", ErrRevoked, SerialOf(c))
		}
	}
	leaf := chain[0]
	role, err := RoleOf(leaf)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Subject: leaf.Subject.CommonName, Role: role, Serial: SerialOf(leaf)}, nil
}
