// Package signedrequest is the form of a request signed with the key of an X.509 certificate,
// which naru's signer takes and a Machine API caller's renewal carries. It needs only the standard
// library, so that a module of the Machine API's callers requires no other module of naru;
// lib/identity re-exports it.
package signedrequest

import (
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrAction is a request signed for another action.
	ErrAction = errors.New("request was signed for another action")
	// ErrExpired is a request past its expiry.
	ErrExpired = errors.New("request has expired")
	// ErrSignature is a request whose signature does not verify.
	ErrSignature = errors.New("request signature does not verify")
)

// ActionRenewCaller is signed by a caller's current certificate to renew it, with a CallerRenewal.
const ActionRenewCaller = "renew-caller"

// CallerRenewal is the body of ActionRenewCaller: the certificate request of the caller's new key.
type CallerRenewal struct {
	CSR []byte `json:"csr"`
}

type request struct {
	Action    string          `json:"action"`
	ExpiresAt time.Time       `json:"expiresAt"`
	Body      json.RawMessage `json:"body"`
}

type signed struct {
	Request     json.RawMessage `json:"request"`
	Signature   []byte          `json:"signature"`
	Certificate []byte          `json:"certificate"`
}

// Sign signs body for action with the key of cert and attaches cert. The signature is over the
// request JSON, which binds the action, the expiry and the body.
func Sign(action string, body any, expiresAt time.Time, key crypto.Signer, cert *x509.Certificate) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}
	req, err := json.Marshal(request{Action: action, ExpiresAt: expiresAt, Body: raw})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	sum := sha256.Sum256(req)
	sig, err := key.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return nil, fmt.Errorf("sign request: %w", err)
	}
	return json.Marshal(signed{Request: req, Signature: sig, Certificate: cert.Raw})
}

// Open checks that the attached certificate chains to roots for client authentication at now and
// signed a request for action that has not expired at now. It returns the verified chains, whose
// first certificate is the signer's, and the body. Whether the certificate is revoked, and what
// its role is, are the caller's to check.
func Open(data []byte, action string, roots *x509.CertPool, now time.Time) ([][]*x509.Certificate, json.RawMessage, error) {
	var s signed
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, nil, fmt.Errorf("parse signed request: %w", err)
	}
	cert, err := x509.ParseCertificate(s.Certificate)
	if err != nil {
		return nil, nil, fmt.Errorf("parse request certificate: %w", err)
	}
	chains, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return nil, nil, fmt.Errorf("verify request certificate: %w", err)
	}
	if cert.CheckSignature(x509.ECDSAWithSHA256, s.Request, s.Signature) != nil {
		return nil, nil, ErrSignature
	}
	var req request
	if err := json.Unmarshal(s.Request, &req); err != nil {
		return nil, nil, fmt.Errorf("parse request: %w", err)
	}
	if req.Action != action {
		return nil, nil, fmt.Errorf("%w: %q, not %q", ErrAction, req.Action, action)
	}
	if !now.Before(req.ExpiresAt) {
		return nil, nil, ErrExpired
	}
	return chains, req.Body, nil
}

// Read returns the action and the body of a signed request without checking its certificate or
// its signature, for a party that only forwards it to the one that checks them: what it reads
// tells it whether the request is the one it means to forward, never whether it is authorised.
func Read(data []byte) (action string, body json.RawMessage, err error) {
	var s signed
	if err := json.Unmarshal(data, &s); err != nil {
		return "", nil, fmt.Errorf("parse signed request: %w", err)
	}
	var req request
	if err := json.Unmarshal(s.Request, &req); err != nil {
		return "", nil, fmt.Errorf("parse request: %w", err)
	}
	return req.Action, req.Body, nil
}

// Certificate returns the certificate attached to a signed request without checking it, for a
// party that only forwards the request: it tells that party whose request it holds, never that the
// request is authentic.
func Certificate(data []byte) (*x509.Certificate, error) {
	var s signed
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse signed request: %w", err)
	}
	cert, err := x509.ParseCertificate(s.Certificate)
	if err != nil {
		return nil, fmt.Errorf("parse request certificate: %w", err)
	}
	return cert, nil
}
