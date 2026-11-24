package orchtest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/signer"
)

// Signer stands in for naru-signer. It does what the signer does with a statement: verifies it
// with lib/identity against the authority and the revocation list, and signs the certificate
// request only for the key the operator named.
type Signer struct {
	CA *CA
	mu sync.Mutex
	// calls counts IssueHost calls, so a test can tell a refusal at orch from one at the signer.
	calls int
	// fail, when set, is what IssueHost answers instead of signing.
	fail error
	// tokens are the callers' registration tokens not yet spent, by token, each the caller's name.
	tokens map[string]string
}

// Fail makes IssueHost answer err from now on; nil makes it sign again.
func (s *Signer) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
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
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
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
// PushCRL and merges them the way the signer does, serials and reissue nonces both, and Revoke adds
// a serial to its own.
type CRLReplica struct {
	ca     *CA
	mu     sync.Mutex
	seq    uint64
	set    map[string]struct{}
	nonces map[string]time.Time
	cur    []byte
}

// NewCRLReplica starts a replica holding the empty list at sequence 1.
func NewCRLReplica(t testing.TB, ca *CA) *CRLReplica {
	r := &CRLReplica{ca: ca, seq: 1, set: map[string]struct{}{}, nonces: map[string]time.Time{}}
	if err := r.sign(); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *CRLReplica) sign() error {
	crl := identity.CRL{Seq: r.seq, IssuedAt: time.Now(), Nonces: maps.Clone(r.nonces)}
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

// UseNonce records a reissue nonce, live until expiresAt, and signs the next sequence.
func (r *CRLReplica) UseNonce(nonce string, expiresAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nonces[nonce] = expiresAt
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
// is newer and holds everything held, and otherwise the union signed at the higher sequence plus one.
func (r *CRLReplica) PushCRL(_ context.Context, data []byte) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	in, err := identity.VerifyCRL(r.ca.RevocationKey(), data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", inventory.ErrSignerRefused, err)
	}
	now := time.Now()
	ownSerials, ownNonces := len(r.set), len(r.nonces)
	for _, s := range in.Serials {
		r.set[s] = struct{}{}
	}
	for n, exp := range in.Nonces {
		if exp.After(now) {
			r.nonces[n] = exp
		}
	}
	if len(r.set) == ownSerials && len(r.nonces) == ownNonces {
		return r.cur, nil
	}
	if len(r.set) == len(in.Serials) && len(r.nonces) == len(in.Nonces) && in.Seq > r.seq {
		r.seq, r.cur = in.Seq, data
		return r.cur, nil
	}
	r.seq = max(r.seq, in.Seq) + 1
	return r.cur, r.sign()
}

// HasNonce reports whether the replica holds the reissue nonce.
func (r *CRLReplica) HasNonce(nonce string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.nonces[nonce]
	return ok
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

// CallerToken is a registration token for the caller name, as root asks naru-signer for one. It is
// taken once.
func (s *Signer) CallerToken(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = map[string]string{}
	}
	token := "naru-ct-" + rand.Text()
	s.tokens[token] = name
	return token
}

// RegisterCaller spends a token CallerToken gave for a caller certificate of 90 days, as
// naru-signer's POST /v1/caller does.
func (s *Signer) RegisterCaller(_ context.Context, token string, csr []byte) (identity.Issued, error) {
	s.mu.Lock()
	name, ok := s.tokens[token]
	delete(s.tokens, token)
	s.mu.Unlock()
	if !ok {
		return identity.Issued{}, fmt.Errorf("%w: the registration token is not valid, or was spent or has expired", inventory.ErrSignerRefused)
	}
	return s.issueCaller(name, csr)
}

// RenewCaller issues a further caller certificate for a renewal signed in the last 30 days of the
// signing certificate, as naru-signer's POST /v1/caller-renew does.
func (s *Signer) RenewCaller(_ context.Context, signed []byte) (identity.Issued, error) {
	data, err := s.CA.CRL()
	if err != nil {
		return identity.Issued{}, err
	}
	crl, err := identity.VerifyCRL(s.CA.RevocationKey(), data)
	if err != nil {
		return identity.Issued{}, err
	}
	who, raw, err := identity.VerifyRequest(signed, identity.ActionRenewCaller, s.CA.Pool(), crl, time.Now())
	if err != nil || who.Role != identity.RoleCaller {
		return identity.Issued{}, fmt.Errorf("%w: not a caller's renewal: %v", inventory.ErrSignerRefused, err)
	}
	if time.Now().Before(who.NotAfter.Add(-identity.CallerRenewWindow)) {
		return identity.Issued{}, identity.ErrCallerNotDue
	}
	var req identity.CallerRenewal
	if err := json.Unmarshal(raw, &req); err != nil {
		return identity.Issued{}, err
	}
	return s.issueCaller(who.Subject, req.CSR)
}

func (s *Signer) issueCaller(name string, csrDER []byte) (identity.Issued, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return identity.Issued{}, fmt.Errorf("%w: %v", signer.ErrInvalid, err)
	}
	cert, der, err := s.CA.signUntil(name, string(identity.RoleCaller), csr.PublicKey, nil, time.Now().AddDate(0, 0, identity.MaxCallerDays))
	if err != nil {
		return identity.Issued{}, err
	}
	return identity.Issued{Certificate: der, Serial: identity.SerialOf(cert), NotAfter: cert.NotAfter}, nil
}
