package signer

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// A caller registers once with a token root asked for and a key it made, and renews its own
// certificate (ADR 0116).
const (
	// callerTokenPrefix marks a caller registration token, so that a scanner finds one that leaked.
	callerTokenPrefix = "naru-ct-"
	// callerTokenDomain is signed before a token's claims, so that no other thing the operator
	// authority signs reads as a token.
	callerTokenDomain = "naru caller registration token v1\n"
)

// ErrBadToken is the one answer to a registration token that is malformed, unknown, spent or
// expired.
var ErrBadToken = errors.New("the registration token is not valid, or was spent or has expired")

// ErrNotDue is returned for a renewal before the last identity.CallerRenewWindow of the
// certificate's life.
var ErrNotDue = errors.New("the certificate is not yet due for renewal")

// callerClaims is what a registration token carries. The nonce is what spends it.
type callerClaims struct {
	Name      string    `json:"name"`
	Nonce     string    `json:"nonce"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// IssueCallerToken answers a registration token for a caller, for a request root signed. The token
// is signed by the operator authority, which every replica holds, so that any signer takes it, and
// the signer keeps no copy of it.
func (s *Signer) IssueCallerToken(signed []byte) (identity.CallerToken, error) {
	var req identity.CallerTokenRequest
	who, a, err := s.authorise(signed, identity.ActionIssueCallerToken, &req, identity.RoleRoot)
	if err != nil {
		return identity.CallerToken{}, err
	}
	if !identity.IsCallerName(req.Name) {
		return identity.CallerToken{}, fmt.Errorf("%w: caller name %q is not a DNS label", ErrBadRequest, req.Name)
	}
	ttl := time.Duration(req.TTLSeconds) * time.Second
	if req.TTLSeconds == 0 {
		ttl = identity.DefaultCallerTokenTTL
	}
	if ttl <= 0 || ttl > identity.MaxCallerTokenTTL {
		return identity.CallerToken{}, fmt.Errorf("%w: a registration token lives 1 s to %s", ErrBadRequest, identity.MaxCallerTokenTTL)
	}
	nonce, err := identity.NewNonce()
	if err != nil {
		return identity.CallerToken{}, err
	}
	claims := callerClaims{Name: req.Name, Nonce: nonce, ExpiresAt: s.now().Add(ttl)}
	token, err := signCallerToken(a.operator.key, claims)
	if err != nil {
		return identity.CallerToken{}, err
	}
	s.log.Info("issued a caller registration token", "subject", req.Name, "token_nonce", nonce,
		"expires_at", claims.ExpiresAt, "authorised_by", who.Subject, "authorised_by_serial", who.Serial)
	return identity.CallerToken{Token: token, ExpiresAt: claims.ExpiresAt}, nil
}

// RegisterCaller issues a caller certificate for a registration token and the certificate request
// of the caller's own key. The token is spent before the request is read, so it is taken once
// whatever the outcome.
func (s *Signer) RegisterCaller(req identity.CallerRegistration) (identity.Issued, error) {
	a, err := s.ready()
	if err != nil {
		return identity.Issued{}, err
	}
	now := s.now()
	claims, err := verifyCallerToken(&a.operator.key.PublicKey, req.Token, now)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := s.spend(claims.Nonce, claims.ExpiresAt, now); err != nil {
		return identity.Issued{}, err
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	issued, err := s.issueCaller(a, now, identity.MaxCallerDays, claims.Name, csr)
	if err != nil {
		return identity.Issued{}, err
	}
	s.log.Info("registered a caller", "subject", claims.Name, "serial", issued.Serial, "token_nonce", claims.Nonce)
	return issued, nil
}

// RenewCaller issues a caller a further certificate, for a request signed with its current one in
// the last identity.CallerRenewWindow of its life, for the key in the request. The current one stays
// valid until it expires, so the caller moves to the new pair when it holds it.
func (s *Signer) RenewCaller(signed []byte) (identity.Issued, error) {
	var req identity.CallerRenewal
	who, a, err := s.authorise(signed, identity.ActionRenewCaller, &req, identity.RoleCaller)
	if err != nil {
		return identity.Issued{}, err
	}
	name, ok := identity.CallerName(who)
	if !ok {
		return identity.Issued{}, fmt.Errorf("%w: %q is not a caller's name", ErrForbidden, who.Subject)
	}
	now := s.now()
	if due := who.NotAfter.Add(-identity.CallerRenewWindow); now.Before(due) {
		return identity.Issued{}, fmt.Errorf("%w: renew from %s", ErrNotDue, due.Format(time.RFC3339))
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	issued, err := s.issueCaller(a, now, identity.MaxCallerDays, name, csr)
	if err != nil {
		return identity.Issued{}, err
	}
	s.log.Info("renewed a caller", "subject", name, "serial", issued.Serial, "previous_serial", who.Serial)
	return issued, nil
}

// issueCaller issues and records a caller certificate: OU caller, the CN name, client
// authentication only, valid days under the operator authority (ADR 0108 item 4).
func (s *Signer) issueCaller(a *authorities, now time.Time, days int, name string, csr *x509.CertificateRequest) (identity.Issued, error) {
	issued, err := issueLeafUntil(a.operator, now, now.AddDate(0, 0, days), name, identity.RoleCaller, csr.PublicKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := s.record(issued.Serial, identity.RoleCaller); err != nil {
		return identity.Issued{}, err
	}
	return issued, nil
}

// spend signs nonce into the revocation list until expiresAt, so that what carries it is taken once
// here and, once orch's sync carries the list, on every other signer (ADR 0052). A nonce already on
// the list is ErrBadToken.
func (s *Signer) spend(nonce string, expiresAt, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	if err := s.checkSynced(now); err != nil {
		return err
	}
	if cur.crl.Used(nonce, now) {
		return ErrBadToken
	}
	crl := cur.successor(now)
	crl.Nonces[nonce] = expiresAt
	next := *cur
	if err := next.signCRL(crl); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(s.dir, crlFile), next.crlSigned); err != nil {
		return err
	}
	s.auth = &next
	return nil
}

func signCallerToken(key *ecdsa.PrivateKey, claims callerClaims) (string, error) {
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(callerTokenDomain), payload...))
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return callerTokenPrefix + enc.EncodeToString(payload) + "." + enc.EncodeToString(sig), nil
}

// verifyCallerToken returns the claims of a token pub signed that has not expired at now. Every
// refusal is ErrBadToken, so the answer tells nothing of why.
func verifyCallerToken(pub *ecdsa.PublicKey, token string, now time.Time) (callerClaims, error) {
	body, ok := strings.CutPrefix(token, callerTokenPrefix)
	if !ok {
		return callerClaims{}, ErrBadToken
	}
	p, sg, ok := strings.Cut(body, ".")
	if !ok {
		return callerClaims{}, ErrBadToken
	}
	enc := base64.RawURLEncoding
	payload, err := enc.DecodeString(p)
	if err != nil {
		return callerClaims{}, ErrBadToken
	}
	sig, err := enc.DecodeString(sg)
	if err != nil {
		return callerClaims{}, ErrBadToken
	}
	sum := sha256.Sum256(append([]byte(callerTokenDomain), payload...))
	if !ecdsa.VerifyASN1(pub, sum[:], sig) {
		return callerClaims{}, ErrBadToken
	}
	var c callerClaims
	if json.Unmarshal(payload, &c) != nil || !identity.IsCallerName(c.Name) || len(c.Nonce) < minNonceLen || len(c.Nonce) > maxNonceLen || !now.Before(c.ExpiresAt) {
		return callerClaims{}, ErrBadToken
	}
	return c, nil
}
