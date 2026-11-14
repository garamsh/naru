package keyprobe_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/keyprobe"
)

func keyPair(t *testing.T) (pubPEM, keyPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	require.NoError(t, err)
	der, err := x509.MarshalPKCS8PrivateKey(k)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func TestScan_FindsAFileThatHoldsAWantedPrivateKeyAndSkipsTheNamedDirectoryAndAnotherKey(t *testing.T) {
	dir := t.TempDir()
	wantPub, wantKey := keyPair(t)
	_, otherKey := keyPair(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "signer"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "run"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "signer", "key"), wantKey, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "run", "other"), otherKey, 0o600))
	want, err := keyprobe.Parse(wantPub)
	require.NoError(t, err)
	skip := []string{filepath.Join(dir, "signer")}

	n, found := keyprobe.Scan([]string{dir}, skip, want)
	assert.Equal(t, 1, n, "the skipped directory is not read")
	assert.Empty(t, found, "another key is not the wanted one")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "run", "leak"), wantKey, 0o600))
	_, found = keyprobe.Scan([]string{dir}, skip, want)
	assert.Equal(t, []string{filepath.Join(dir, "run", "leak")}, found)
	_, found = keyprobe.Scan([]string{dir}, nil, want)
	assert.Len(t, found, 2, "without the skip, the signer's own copy is found too")
}

func TestParse_RefusesInputWithNoKey(t *testing.T) {
	_, err := keyprobe.Parse([]byte("nothing"))
	assert.Error(t, err)
}
