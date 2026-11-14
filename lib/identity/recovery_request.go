package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrRecoverySignature is returned when a request is not signed by the recovery key.
var ErrRecoverySignature = errors.New("request is not signed by the recovery key")

// ActionReissueRoot is the action of a request the recovery key signs for a new root certificate.
const ActionReissueRoot = "reissue-root"

// NonceBytes is the size of the random nonce a recovery request carries.
const NonceBytes = 16

// ReissueRootRequest is the body of ActionReissueRoot: the certificate request, DER, that root's
// new certificate is issued for, and a nonce that makes the request once-only.
type ReissueRootRequest struct {
	Nonce string `json:"nonce"`
	CSR   []byte `json:"csr"`
}

// NewNonce returns a random nonce, lowercase hexadecimal, for a recovery request.
func NewNonce() (string, error) {
	b := make([]byte, NonceBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

type signedRecoveryRequest struct {
	Request   json.RawMessage `json:"request"`
	Signature []byte          `json:"signature"`
}

// SignRecoveryRequest signs body for action with the recovery key's Ed25519 half. It carries no
// certificate: the signer knows the recovery key's public half, and the signature is over the
// request JSON, which binds the action, the expiry and the body.
func SignRecoveryRequest(action string, body any, expiresAt time.Time, key ed25519.PrivateKey) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := json.Marshal(request{Action: action, ExpiresAt: expiresAt, Body: raw})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	return json.Marshal(signedRecoveryRequest{Request: req, Signature: ed25519.Sign(key, req)})
}

// VerifyRecoveryRequest checks that pub, the recovery key's Ed25519 public half, signed a request
// for action that has not expired at now. It returns the body and the expiry.
func VerifyRecoveryRequest(data []byte, action string, pub ed25519.PublicKey, now time.Time) (json.RawMessage, time.Time, error) {
	var s signedRecoveryRequest
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: parse signed request: %w", ErrFormat, err)
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, s.Request, s.Signature) {
		return nil, time.Time{}, ErrRecoverySignature
	}
	var req request
	if err := json.Unmarshal(s.Request, &req); err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: parse request: %w", ErrFormat, err)
	}
	if req.Action != action {
		return nil, time.Time{}, fmt.Errorf("%w: %q, not %q", ErrRequestAction, req.Action, action)
	}
	if !now.Before(req.ExpiresAt) {
		return nil, time.Time{}, ErrRequestExpired
	}
	return req.Body, req.ExpiresAt, nil
}
