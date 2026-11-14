// Package auth is what orch asks of an identity: who the caller is, per lib/identity, and whether
// the role may do an action. The revocation list is the signer's; orch caches it.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// Action is a kind of request an API endpoint belongs to.
type Action int

// The actions. Each endpoint belongs to exactly one.
const (
	// Read lists hosts, pending enrollments and events.
	Read Action = iota
	// Operate admits a host, manages enrollment keys and sets labels.
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

// ErrNoRevocationList is returned when no signed revocation list could be had yet, so nobody can
// be told apart from a revoked identity.
var ErrNoRevocationList = errors.New("no revocation list is available")

// Authenticator turns the verified chains of a TLS connection into an Identity, refusing a
// revoked one.
type Authenticator interface {
	Authenticate(ctx context.Context, chains [][]*x509.Certificate) (identity.Identity, error)
}

// CRLSource returns the revocation list as the signer signed it.
type CRLSource interface {
	CRL(ctx context.Context) ([]byte, error)
}

// Revocations is an Authenticator that checks lib/identity's revocation list, kept at most TTL
// old. A list that fails to refresh is kept; with none at all every call fails.
type Revocations struct {
	Source CRLSource
	// Key verifies the list's signature.
	Key ed25519.PublicKey
	TTL time.Duration
	Now func() time.Time
	Log *slog.Logger

	mu      sync.Mutex
	crl     *identity.CRL
	fetched time.Time
}

var _ Authenticator = (*Revocations)(nil)

// Authenticate implements Authenticator.
func (r *Revocations) Authenticate(ctx context.Context, chains [][]*x509.Certificate) (identity.Identity, error) {
	crl, err := r.list(ctx)
	if err != nil {
		return identity.Identity{}, err
	}
	return identity.Authenticate(chains, crl)
}

func (r *Revocations) list(ctx context.Context) (*identity.CRL, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.Now()
	if r.crl != nil && now.Sub(r.fetched) < r.TTL {
		return r.crl, nil
	}
	crl, err := r.fetch(ctx)
	if err != nil {
		if r.crl == nil {
			return nil, fmt.Errorf("%w: %v", ErrNoRevocationList, err)
		}
		r.Log.Warn("revocation list not refreshed, keeping the last one", "err", err.Error())
		return r.crl, nil
	}
	r.crl, r.fetched = crl, now
	return crl, nil
}

func (r *Revocations) fetch(ctx context.Context) (*identity.CRL, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := r.Source.CRL(ctx)
	if err != nil {
		return nil, err
	}
	crl, err := identity.VerifyCRL(r.Key, data)
	if err != nil {
		return nil, err
	}
	if r.crl != nil {
		if err := crl.CheckSeq(r.crl.Seq); err != nil {
			return nil, err
		}
	}
	return crl, nil
}
