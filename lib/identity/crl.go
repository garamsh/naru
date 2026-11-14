package identity

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

var (
	// ErrCRLSignature is returned when a revocation list is not signed by the revocation key.
	ErrCRLSignature = errors.New("revocation list signature does not verify")
	// ErrCRLStale is returned when a revocation list is older than one already seen.
	ErrCRLStale = errors.New("revocation list is older than the last one seen")
)

// CRL lists the serials, in lowercase hexadecimal, that are no longer trusted, and the nonces of
// the once-only requests the signers took, each with the expiry of its request.
// Seq grows with every list the signer signs.
type CRL struct {
	Seq      uint64               `json:"seq"`
	IssuedAt time.Time            `json:"issuedAt"`
	Serials  []string             `json:"serials"`
	Nonces   map[string]time.Time `json:"nonces,omitempty"`
}

// Used reports whether nonce was taken and its request has not yet expired at now. A nil list has
// taken none.
func (c *CRL) Used(nonce string, now time.Time) bool {
	if c == nil {
		return false
	}
	expiresAt, ok := c.Nonces[nonce]
	return ok && now.Before(expiresAt)
}

// Revokes reports whether serial is on the list. A nil list revokes nothing.
func (c *CRL) Revokes(serial string) bool {
	return c != nil && slices.Contains(c.Serials, serial)
}

// CheckSeq refuses a list whose Seq is lower than lastSeen, so an attacker
// cannot replay an older list that omits a revocation.
func (c *CRL) CheckSeq(lastSeen uint64) error {
	if c.Seq < lastSeen {
		return fmt.Errorf("%w: seq %d, last seen %d", ErrCRLStale, c.Seq, lastSeen)
	}
	return nil
}

type signedCRL struct {
	CRL       json.RawMessage `json:"crl"`
	Signature []byte          `json:"signature"`
}

// SignCRL returns the list as signed JSON, signed by the revocation key.
func SignCRL(key ed25519.PrivateKey, crl CRL) ([]byte, error) {
	if crl.Serials == nil {
		crl.Serials = []string{}
	}
	body, err := json.Marshal(crl)
	if err != nil {
		return nil, fmt.Errorf("marshal revocation list: %w", err)
	}
	return json.Marshal(signedCRL{CRL: body, Signature: ed25519.Sign(key, body)})
}

// VerifyCRL checks the signature against the revocation key and returns the list.
func VerifyCRL(pub ed25519.PublicKey, data []byte) (*CRL, error) {
	var s signedCRL
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse revocation list: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, s.CRL, s.Signature) {
		return nil, ErrCRLSignature
	}
	var crl CRL
	if err := json.Unmarshal(s.CRL, &crl); err != nil {
		return nil, fmt.Errorf("parse revocation list: %w", err)
	}
	return &crl, nil
}
