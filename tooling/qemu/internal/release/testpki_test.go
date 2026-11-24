package release

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testEpoch = 1794355200

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	return string(b)
}

func TestTestPKI_ASignatureIsVerifiableAndRepeatable(t *testing.T) {
	key, err := derivedKeyOf("v1", "registry-ca-key")
	require.NoError(t, err)
	digest := sha256.Sum256([]byte("a certificate"))

	first, err := key.Sign(nil, digest[:], crypto.SHA256)
	require.NoError(t, err)
	second, err := key.Sign(nil, digest[:], crypto.SHA256)
	require.NoError(t, err)

	assert.Equal(t, first, second)
	assert.True(t, ecdsa.VerifyASN1(&key.PublicKey, digest[:], first))
	assert.Equal(t, elliptic.P256(), key.Curve)
}
