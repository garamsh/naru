package release

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEpoch = 1794355200

func writeTestPKI(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, testPKI(dir, version, testEpoch, []string{"naru-node-1", "naru-node-2"}))
	return dir
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}

func TestTestPKI_AVersionFixesTheAuthorityAndTheTokens(t *testing.T) {
	a, b, other := writeTestPKI(t, "v1"), writeTestPKI(t, "v1"), writeTestPKI(t, "v2")

	for _, f := range []string{"ca.pem", "ca.key", "tokens/naru-node-1", "tokens/naru-node-2"} {
		assert.Equal(t, read(t, a, f), read(t, b, f), "%s: one version, one file", f)
		assert.NotEqual(t, read(t, a, f), read(t, other, f), "%s: another version, another file", f)
	}
	assert.NotEqual(t, read(t, a, "tokens/naru-node-1"), read(t, a, "tokens/naru-node-2"))
}

func TestTestPKI_TheAuthorityIsOneNaructlAndInitAccept(t *testing.T) {
	dir := writeTestPKI(t, "v1")

	block, _ := pem.Decode([]byte(read(t, dir, "ca.pem")))
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	assert.True(t, cert.IsCA)
	require.NoError(t, cert.CheckSignatureFrom(cert), "the self-signature verifies")
	assert.Equal(t, time.Unix(testEpoch, 0).UTC().Add(-time.Hour), cert.NotBefore)
	assert.Equal(t, 10*365*24*time.Hour+0, cert.NotAfter.Sub(cert.NotBefore))
	kb, _ := pem.Decode([]byte(read(t, dir, "ca.key")))
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	require.NoError(t, err)
	assert.True(t, key.PublicKey.Equal(cert.PublicKey))
	for _, n := range []string{"naru-node-1", "naru-node-2"} {
		assert.Regexp(t, regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}\n$`), read(t, dir, "tokens/"+n))
	}
	info, err := os.Stat(filepath.Join(dir, "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestDeterministicSigner_SignsVerifiablyAndRepeatably(t *testing.T) {
	key, err := derivedKey("v1")
	require.NoError(t, err)
	digest := sha256.Sum256([]byte("a certificate"))
	s := deterministicSigner{key}

	first, err := s.Sign(nil, digest[:], nil)
	require.NoError(t, err)
	second, err := s.Sign(nil, digest[:], nil)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.True(t, ecdsa.VerifyASN1(&key.PublicKey, digest[:], first))
	assert.Equal(t, elliptic.P256(), key.Curve)
}
