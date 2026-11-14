package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/signer"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// crlTTL is how long a revocation list is used before orch asks the signer for a fresh one.
const crlTTL = 30 * time.Second

// newAuthenticator checks callers against lib/identity's roles and the signer's revocation list,
// which the Ed25519 public key in keyFile (PKIX, PEM) verifies.
func newAuthenticator(sg *signer.Client, keyFile string, log *slog.Logger) (auth.Authenticator, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read the revocation public key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("the revocation public key is not PEM")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the revocation public key: %w", err)
	}
	key, ok := pub.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("the revocation public key is not Ed25519")
	}
	return &auth.Revocations{Source: sg, Key: key, TTL: crlTTL, Now: time.Now, Log: log}, nil
}

// newSealer seals snapshots to the recovery public key in path, the file naructl orch init wrote.
// An empty path gives no sealer, so no snapshot is written.
func newSealer(path string) (snapshot.Sealer, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the recovery public key: %w", err)
	}
	pub, err := identity.ParseRecoveryPublic(data)
	if err != nil {
		return nil, err
	}
	k, err := pub.SealKey()
	if err != nil {
		return nil, err
	}
	return snapshot.SealTo(k), nil
}
