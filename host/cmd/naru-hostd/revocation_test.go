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
	"io"
	"log/slog"
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
	go refuseRevokedKey(t.Context(), list, 10*time.Millisecond, nil, func(err error) { failed <- err }, func() []string { return []string{cert} })
	select {
	case err := <-failed:
		assert.ErrorIs(t, err, identity.ErrRevoked)
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked key was not refused")
	}
}

func TestChooseServing_ServesTheStoredPairThenTheSeedAndRefusesWhenNeitherIsUsable(t *testing.T) {
	dir := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	seedCrt, seedKey := pairFiles(t, dir, "seed", 0x2a)
	storedPath := filepath.Join(dir, "serving.pem")

	_, err := chooseServing(t.Context(), listRevoking(t), log, seedCrt, seedKey, storedPath)
	require.NoError(t, err, "control: the seed pair serves when nothing is stored and nothing is revoked")

	// a stored pair, 0x2b, that is not revoked wins over a seed pair that is
	storedCrt, storedKey := pairFiles(t, dir, "stored", 0x2b)
	both := append(mustRead(t, storedCrt), mustRead(t, storedKey)...)
	require.NoError(t, os.WriteFile(storedPath, both, 0o600))
	s, err := chooseServing(t.Context(), listRevoking(t, "2a"), log, seedCrt, seedKey, storedPath)
	require.NoError(t, err)
	assert.Equal(t, storedPath, s.File())

	// the stored pair revoked falls back to the seed pair, unless that is revoked too
	s, err = chooseServing(t.Context(), listRevoking(t, "2b"), log, seedCrt, seedKey, storedPath)
	require.NoError(t, err)
	assert.Equal(t, seedCrt, s.File())
	_, err = chooseServing(t.Context(), listRevoking(t, "2a", "2b"), log, seedCrt, seedKey, storedPath)
	assert.ErrorIs(t, err, identity.ErrRevoked, "a host whose only pairs are revoked refuses to serve")

	// no stored pair and a revoked seed pair
	require.NoError(t, os.Remove(storedPath))
	_, err = chooseServing(t.Context(), listRevoking(t, "2a"), log, seedCrt, seedKey, storedPath)
	assert.ErrorIs(t, err, identity.ErrRevoked)
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	return b
}

// pairFiles writes a self-signed certificate of serial and its key, and returns their paths.
func pairFiles(t *testing.T, dir, name string, serial int64) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "h-1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}, &x509.Certificate{}, &key.PublicKey, key)
	require.NoError(t, err)
	kder, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	c, k := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	require.NoError(t, os.WriteFile(c, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(k, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}), 0o600))
	return c, k
}
