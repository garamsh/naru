package identity_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// writePair writes cert and key as PEM to the files, stamped at mod so the change is seen.
func writePair(t *testing.T, certFile, keyFile string, cert *x509.Certificate, key *ecdsa.PrivateKey, mod time.Time) {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}), 0o600))
	require.NoError(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600))
	require.NoError(t, os.Chtimes(certFile, mod, mod))
	require.NoError(t, os.Chtimes(keyFile, mod, mod))
}

func TestFilePairPresentsTheFilesAsTheyStandAndKeepsTheLastPairThroughATornWrite(t *testing.T) {
	c := newCA(t)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "orch.crt"), filepath.Join(dir, "orch.key")
	first, firstKey := c.issue(t, "orch-1", 2, now.Add(time.Hour))
	writePair(t, certFile, keyFile, first, firstKey, now)

	p, err := identity.NewFilePair(certFile, keyFile)
	require.NoError(t, err)
	got, err := p.GetCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, first.Raw, got.Certificate[0])

	renewed, renewedKey := c.issue(t, "orch-1", 3, now.Add(2*time.Hour))
	writePair(t, certFile, keyFile, renewed, renewedKey, now.Add(time.Minute))
	got, err = p.GetClientCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, renewed.Raw, got.Certificate[0], "the renewed pair, from the next handshake on")

	require.NoError(t, os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\ntorn"), 0o600))
	_, err = p.Pair()
	require.Error(t, err, "a torn certificate is not a pair")
	got, err = p.GetCertificate(nil)
	require.NoError(t, err)
	assert.Equal(t, renewed.Raw, got.Certificate[0], "a handshake keeps the pair last read")

	_, err = identity.NewFilePair(certFile, keyFile)
	require.Error(t, err, "and before any read there is none")
}
