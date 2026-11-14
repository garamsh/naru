package main

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/signer"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// crlTTL is how long a revocation list is used before orch asks the signer for a fresh one.
const crlTTL = 30 * time.Second

// readRevocationKey parses the Ed25519 public key in keyFile (PKIX, PEM) that verifies the
// revocation list.
func readRevocationKey(keyFile string) (ed25519.PublicKey, error) {
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
	return key, nil
}

// newAuthenticator checks callers against lib/identity's roles and the signer's revocation list,
// which key verifies. A list that cannot be refreshed is used for maxAge and then refused.
func newAuthenticator(sg *signer.Client, key ed25519.PublicKey, maxAge time.Duration, log *slog.Logger) auth.Authenticator {
	return &auth.Revocations{Source: sg, Key: key, TTL: crlTTL, MaxAge: maxAge, Now: time.Now, Log: log}
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

// newHostHTTP is the HTTP client the groups engine drives the hosts' APIs with. It presents the
// pair in certFile and keyFile, orch's own identity unless the operator names another, and trusts
// the authority in caFile.
func newHostHTTP(certFile, keyFile, caFile string) (*http.Client, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load the host API client pair: %w", err)
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read the host API authority: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("%s holds no certificate", caFile)
	}
	return orchapi.HTTPClient(orchapi.TrustCA(pool, &pair), 30*time.Second), nil
}
