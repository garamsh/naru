// Package identity holds what naru's authorities and their callers agree on:
// the roles a certificate carries, the revocation list, the pairing code, the
// admission statement, the passphrase-encrypted person and recovery files and
// the sealed box.
package identity

import (
	"crypto/x509"
	"errors"
	"fmt"
	"regexp"
	"strings"
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
	// RoleExposer is the cloud-controller-manager of one network; its Subject is ExposerPrefix and
	// the network's name (ADR 0085 item 7).
	RoleExposer Role = "exposer"
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
	case RoleRoot, RoleOperator, RoleReader, RoleAutomation, RoleHost, RoleOrch, RoleExposer:
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
	if err := crl.CheckChain(chain); err != nil {
		return Identity{}, err
	}
	leaf := chain[0]
	role, err := RoleOf(leaf)
	if err != nil {
		return Identity{}, err
	}
	return Identity{Subject: leaf.Subject.CommonName, Role: role, Serial: SerialOf(leaf)}, nil
}

// ExposerPrefix starts the Subject of a RoleExposer certificate; the rest is the network it serves.
const ExposerPrefix = "exposer."

// networkName is what a network is called: a DNS label.
var networkName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ExposerSubject returns the Subject of the RoleExposer certificate of network. ok is false when
// network is not a DNS label.
func ExposerSubject(network string) (subject string, ok bool) {
	if !networkName.MatchString(network) {
		return "", false
	}
	return ExposerPrefix + network, true
}

// ExposerNetwork returns the network an exposer identity serves. ok is false for any other role,
// or for a Subject that does not name a network by a DNS label.
func ExposerNetwork(who Identity) (network string, ok bool) {
	n, found := strings.CutPrefix(who.Subject, ExposerPrefix)
	if who.Role != RoleExposer || !found || !networkName.MatchString(n) {
		return "", false
	}
	return n, true
}
