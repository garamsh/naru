package orchtest

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Signer stands in for naru-signer. It does what the signer does with a statement: verifies it
// with lib/identity against the authority and the revocation list, and signs the certificate
// request only for the key the operator named.
type Signer struct {
	CA *CA
	mu sync.Mutex
	// calls counts IssueHost calls, so a test can tell a refusal at orch from one at the signer.
	calls int
}

var _ inventory.Signer = (*Signer)(nil)

// Statement is the admission statement operator signs for the host holding the public key pub.
func Statement(t testing.TB, operator Identity, hostID string, pub []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(pub)
	stmt, err := identity.SignAdmission(identity.Admission{
		HostID: hostID, HostKeySHA256: hex.EncodeToString(sum[:]), Region: "kr-seoul", ExpiresAt: time.Now().Add(time.Hour),
	}, operator.TLS.PrivateKey.(crypto.Signer), operator.Cert)
	if err != nil {
		t.Fatal(err)
	}
	return stmt
}

// Calls reports how many times IssueHost was called.
func (s *Signer) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// IssueHost implements inventory.Signer.
func (s *Signer) IssueHost(_ context.Context, statement, csr []byte) ([]byte, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	data, err := s.CA.CRL()
	if err != nil {
		return nil, err
	}
	crl, err := identity.VerifyCRL(s.CA.RevocationKey(), data)
	if err != nil {
		return nil, err
	}
	_, adm, err := identity.VerifyAdmission(statement, s.CA.Pool(), crl, time.Now())
	if err != nil {
		return nil, fmt.Errorf("%w: %v", inventory.ErrSignerRefused, err)
	}
	c, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(c.PublicKey)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(der)
	if hex.EncodeToString(sum[:]) != adm.HostKeySHA256 {
		return nil, fmt.Errorf("%w: the statement names another key", inventory.ErrSignerRefused)
	}
	return s.CA.SignCSR(csr, adm.HostID, identity.RoleHost)
}

// CRL implements inventory.Signer.
func (s *Signer) CRL(context.Context) ([]byte, error) { return s.CA.CRL() }

// CRLReplica stands in for one naru-signer replica's revocation list: it takes lists with
// PushCRL and merges them the way the signer does, and Revoke adds a serial to its own.
type CRLReplica struct {
	ca  *CA
	mu  sync.Mutex
	seq uint64
	set map[string]struct{}
	cur []byte
}

// NewCRLReplica starts a replica holding the empty list at sequence 1.
func NewCRLReplica(t testing.TB, ca *CA) *CRLReplica {
	r := &CRLReplica{ca: ca, seq: 1, set: map[string]struct{}{}}
	if err := r.sign(); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *CRLReplica) sign() error {
	crl := identity.CRL{Seq: r.seq, IssuedAt: time.Now()}
	for s := range r.set {
		crl.Serials = append(crl.Serials, s)
	}
	sort.Strings(crl.Serials)
	b, err := r.ca.SignCRL(crl)
	r.cur = b
	return err
}

// Revoke adds the identity's serial and signs the next sequence.
func (r *CRLReplica) Revoke(id Identity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.set[identity.SerialOf(id.Cert)] = struct{}{}
	r.seq++
	return r.sign()
}

// CRL returns the list the replica holds.
func (r *CRLReplica) CRL(context.Context) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur, nil
}

// PushCRL takes a list and returns the one held: the same with nothing new, the pushed one when it
// is newer and a superset, and otherwise the union signed at the higher sequence plus one.
func (r *CRLReplica) PushCRL(_ context.Context, data []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, err := identity.VerifyCRL(r.ca.RevocationKey(), data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", inventory.ErrSignerRefused, err)
	}
	added, inHasAll := false, true
	for _, s := range in.Serials {
		if _, ok := r.set[s]; !ok {
			added = true
			r.set[s] = struct{}{}
		}
	}
	if !added {
		return r.cur, nil
	}
	if len(r.set) != len(in.Serials) {
		inHasAll = false
	}
	if inHasAll && in.Seq > r.seq {
		r.seq, r.cur = in.Seq, data
		return r.cur, nil
	}
	r.seq = max(r.seq, in.Seq) + 1
	return r.cur, r.sign()
}

// Has reports whether the replica holds the serial.
func (r *CRLReplica) Has(id Identity) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.set[identity.SerialOf(id.Cert)]
	return ok
}

// Clock is a clock a test moves.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts at the current time.
func NewClock() *Clock { return &Clock{now: time.Now()} }

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
