package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	require.NotNil(t, b, "not PEM")
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c
}

func TestIssue_ALeafDoesNotOutliveTheAuthority(t *testing.T) {
	ca, err := New("test")
	require.NoError(t, err)
	// An authority with an hour left: a leaf of LeafValidity would outlive it.
	ca.cert.NotAfter = time.Now().Add(time.Hour)

	p, err := ca.Client("alice")
	require.NoError(t, err)

	assert.False(t, parse(t, p.CertPEM).NotAfter.After(ca.cert.NotAfter), "the leaf expires with the authority at the latest")
}

func TestLoad_RefusesAKeyThatIsNotTheCertificates(t *testing.T) {
	a, err := New("a")
	require.NoError(t, err)
	b, err := New("b")
	require.NoError(t, err)

	_, err = load(a.CertPEM, b.KeyPEM)

	assert.ErrorContains(t, err, "not the certificate's")
}

func TestLoad_RefusesAnAuthorityThatHasExpired(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: serial(), Subject: pkix.Name{CommonName: "old"},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: time.Now().Add(-time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, CertFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, KeyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))

	_, err = Load(dir)

	assert.ErrorContains(t, err, "expired")
}

func TestWriteFile_GivesAnExistingFileTheModeAndReplacesItsContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.key")
	require.NoError(t, os.WriteFile(path, []byte("old and long content"), 0o644)) //nolint:gosec // the case under test

	require.NoError(t, WriteFile(path, []byte("new"), 0o600))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "a key written over a 0644 file is private")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(got))
}
