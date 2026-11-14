package signer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

const (
	// noncesFile holds the nonces of the reissue requests the signer took, with their expiry. It
	// is not part of the bundle: it guards this signer against a replay, and a replica or a
	// restored signer is guarded by the requests' own expiry only.
	noncesFile = "reissue-nonces.json"
	// maxReissueTTL bounds how far ahead a reissue request may expire, so that remembering its
	// nonce until then is bounded and a captured request goes stale.
	maxReissueTTL = 10 * time.Minute
	minNonceLen   = 2 * identity.NonceBytes
	maxNonceLen   = 128
)

// ErrReplay is returned when a reissue request's nonce was already used.
var ErrReplay = errors.New("request was already used")

// ReissueRoot issues a new root certificate for the certificate request of a request the recovery
// key signed, and revokes every serial the signer recorded as root's, so the old root is no longer
// admitted. It is for the holder of the recovery key who has lost root's person file. The request
// is once-only: its nonce is written to the directory before anything changes.
func (s *Signer) ReissueRoot(signed []byte) (identity.Issued, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.auth
	if cur == nil {
		return identity.Issued{}, ErrNotInitialised
	}
	now := s.now()
	raw, expiresAt, err := identity.VerifyRecoveryRequest(signed, identity.ActionReissueRoot, ed25519.PublicKey(cur.recovery.Ed25519), now)
	if err != nil {
		return identity.Issued{}, err
	}
	if expiresAt.After(now.Add(maxReissueTTL)) {
		return identity.Issued{}, fmt.Errorf("%w: the request expires in more than %s", ErrBadRequest, maxReissueTTL)
	}
	var req identity.ReissueRootRequest
	if err := decode(raw, &req); err != nil {
		return identity.Issued{}, err
	}
	if len(req.Nonce) < minNonceLen || len(req.Nonce) > maxNonceLen {
		return identity.Issued{}, fmt.Errorf("%w: the nonce is not %d to %d characters", ErrBadRequest, minNonceLen, maxNonceLen)
	}
	csr, err := parseCSR(req.CSR)
	if err != nil {
		return identity.Issued{}, err
	}
	if err := s.useNonce(req.Nonce, expiresAt, now); err != nil {
		return identity.Issued{}, err
	}

	old := slices.Sorted(func(yield func(string) bool) {
		for serial, role := range cur.issued {
			if role == string(identity.RoleRoot) && !yield(serial) {
				return
			}
		}
	})
	next := cur
	if revoked := slices.DeleteFunc(slices.Clone(old), cur.crl.Revokes); len(revoked) > 0 {
		crl := identity.CRL{Seq: cur.crl.Seq + 1, IssuedAt: now, Serials: slices.Sorted(slices.Values(append(slices.Clone(cur.crl.Serials), revoked...)))}
		withCRL := *cur
		if err := withCRL.signCRL(crl); err != nil {
			return identity.Issued{}, err
		}
		if err := writeFile(filepath.Join(s.dir, crlFile), withCRL.crlSigned); err != nil {
			return identity.Issued{}, err
		}
		s.auth, next = &withCRL, &withCRL
	}
	issued, err := issueLeaf(next.operator, now, "root", identity.RoleRoot, csr.PublicKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil)
	if err != nil {
		return identity.Issued{}, err
	}
	withRoot := next.withIssued(issued.Serial, identity.RoleRoot)
	if err := writeFile(filepath.Join(s.dir, issuedFile), withRoot.marshalIssued()); err != nil {
		return identity.Issued{}, err
	}
	s.auth = withRoot
	keySum := sha256.Sum256(csr.RawSubjectPublicKeyInfo)
	recoverySum := sha256.Sum256(cur.recovery.Ed25519)
	s.log.Warn("reissued root from the recovery key", "serial", issued.Serial, "key_sha256", hex.EncodeToString(keySum[:]),
		"recovery_key_sha256", hex.EncodeToString(recoverySum[:]), "nonce", req.Nonce, "revoked_root_serials", old)
	return issued, nil
}

// useNonce records nonce as used until expiresAt and refuses one already recorded. Nonces past
// their expiry are dropped: a request with one fails on its expiry.
func (s *Signer) useNonce(nonce string, expiresAt, now time.Time) error {
	path := filepath.Join(s.dir, noncesFile)
	used := map[string]time.Time{}
	data, err := os.ReadFile(path) //nolint:gosec // a file of the signer's own directory
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &used); err != nil {
			return fmt.Errorf("%w: %s: %w", errCorrupt, noncesFile, err)
		}
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("read %s: %w", noncesFile, err)
	}
	for n, exp := range used {
		if !now.Before(exp) {
			delete(used, n)
		}
	}
	if _, ok := used[nonce]; ok {
		return fmt.Errorf("%w: its nonce was seen", ErrReplay)
	}
	used[nonce] = expiresAt
	out, err := json.Marshal(used)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", noncesFile, err)
	}
	return writeFile(path, out)
}
