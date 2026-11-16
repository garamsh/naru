package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

type listSource []byte

func (s listSource) CRL(context.Context) ([]byte, error) { return s, nil }

// certFile writes a self-signed certificate of serial and returns its path.
func certFile(t *testing.T, dir, name string, serial int64) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "h-1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}, &x509.Certificate{}, &key.PublicKey, key)
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	return path
}

func listRevoking(t *testing.T, serials ...string) *identity.RevocationList {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signed, err := identity.SignCRL(priv, identity.CRL{Seq: 1, IssuedAt: time.Now(), Serials: serials})
	require.NoError(t, err)
	return &identity.RevocationList{Source: listSource(signed), Key: pub, TTL: time.Minute, MaxAge: time.Hour, Now: time.Now}
}

func TestCheckOwnCertificates_RefusesARevokedKeyAndSaysWhy(t *testing.T) {
	dir := t.TempDir()
	revoked := certFile(t, dir, "tls.crt", 0x2a)
	fine := certFile(t, dir, "host.crt", 0x2b)
	list := listRevoking(t, "2a")

	err := checkOwnCertificates(t.Context(), list, fine, filepath.Join(dir, "absent.crt"))
	require.NoError(t, err, "control: a certificate that is not revoked, and a file that is not there, pass")

	err = checkOwnCertificates(t.Context(), list, fine, revoked)
	require.ErrorIs(t, err, identity.ErrRevoked)
	assert.Contains(t, err.Error(), revoked)
	assert.Contains(t, err.Error(), "serial 2a")

	require.NoError(t, checkOwnCertificates(t.Context(), nil, revoked), "no list revokes nothing")
}

func TestRefuseRevokedKey_FailsWhenARunningHostsKeyIsRevokedLater(t *testing.T) {
	dir := t.TempDir()
	cert := certFile(t, dir, "tls.crt", 0x2a)
	list := listRevoking(t, "2a")
	failed := make(chan error, 1)
	go refuseRevokedKey(t.Context(), list, 10*time.Millisecond, nil, func(err error) { failed <- err }, cert)
	select {
	case err := <-failed:
		assert.ErrorIs(t, err, identity.ErrRevoked)
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked key was not refused")
	}
}
