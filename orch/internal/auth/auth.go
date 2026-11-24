// Package auth is what orch asks of an identity: who the caller is, per lib/identity, and whether
// the role may do an action. The revocation list is the signer's; orch caches it.
package auth

import (
	"context"
	"crypto/x509"

	"github.com/garamsh/naru/lib/identity"
)

// Action is a kind of request an API endpoint belongs to.
type Action int

// The actions. Each endpoint belongs to exactly one.
const (
	// Read lists hosts, pending enrollments, groups and events.
	Read Action = iota
	// Operate admits a host, manages enrollment keys, sets labels and applies and deletes groups.
	Operate
	// Administer takes a snapshot.
	Administer
	// Report is a host's heartbeat.
	Report
	// Any is open to every authenticated identity.
	Any
)

// Allowed reports whether who's role may do the action.
func Allowed(who identity.Identity, a Action) bool {
	r := who.Role
	switch a {
	case Any:
		return r.Valid()
	case Read:
		return r == identity.RoleReader || r == identity.RoleOperator || r == identity.RoleAutomation || r == identity.RoleRoot
	case Operate:
		return r == identity.RoleOperator || r == identity.RoleAutomation || r == identity.RoleRoot
	case Administer:
		return r == identity.RoleRoot
	case Report:
		return r == identity.RoleHost
	}
	return false
}

// Authenticator turns the verified chains of a TLS connection into an Identity, refusing a
// revoked one.
type Authenticator interface {
	Authenticate(ctx context.Context, chains [][]*x509.Certificate) (identity.Identity, error)
}

// Revocations is an Authenticator that checks the revocation list kept by lib/identity.
type Revocations struct {
	List *identity.RevocationList
}

var _ Authenticator = Revocations{}

// Authenticate implements Authenticator.
func (r Revocations) Authenticate(ctx context.Context, chains [][]*x509.Certificate) (identity.Identity, error) {
	crl, err := r.List.Current(ctx)
	if err != nil {
		return identity.Identity{}, err
	}
	return identity.Authenticate(chains, crl)
}
