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

// sameSizePEMs are two PEM certificates of one size, under c, and their keys.
func sameSizePEMs(t *testing.T, c ca) ([2][]byte, [2]*ecdsa.PrivateKey) {
	t.Helper()
	for range 100 {
		a, ak := c.issue(t, "orch-1", 2, now.Add(time.Hour))
		b, bk := c.issue(t, "orch-1", 2, now.Add(time.Hour))
		pa := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.Raw})
		pb := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.Raw})
		if len(pa) == len(pb) {
			return [2][]byte{pa, pb}, [2]*ecdsa.PrivateKey{ak, bk}
		}
	}
	t.Fatal("no two certificates of one size")
	return [2][]byte{}, [2]*ecdsa.PrivateKey{}
}

// renameOver puts b at path by a rename, stamped with mod, as a renewal writes it.
func renameOver(t *testing.T, path string, b []byte, mod time.Time) {
	t.Helper()
	require.NoError(t, os.WriteFile(path+".new", b, 0o600))
	require.NoError(t, os.Chtimes(path+".new", mod, mod))
	require.NoError(t, os.Rename(path+".new", path))
}

func TestAFileRenamedOverWithinOneClockTickIsReadAgain(t *testing.T) {
	c := newCA(t)
	dir := t.TempDir()
	certs, keys := sameSizePEMs(t, c)
	keyPEM := func(k *ecdsa.PrivateKey) []byte {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}

	certFile, keyFile := filepath.Join(dir, "orch.crt"), filepath.Join(dir, "orch.key")
	renameOver(t, certFile, certs[0], now)
	renameOver(t, keyFile, keyPEM(keys[0]), now)
	p, err := identity.NewFilePair(certFile, keyFile)
	require.NoError(t, err)
	renameOver(t, keyFile, keyPEM(keys[1]), now)
	renameOver(t, certFile, certs[1], now)
	got, err := p.Pair()
	require.NoError(t, err)
	block, _ := pem.Decode(certs[1])
	assert.Equal(t, block.Bytes, got.Certificate[0], "a pair: the same size and time, and another file")

	base, store := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca-store.pem")
	require.NoError(t, os.WriteFile(base, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw}), 0o644))
	renameOver(t, store, certs[0], now)
	pool, err := identity.NewFilePool(base, store)
	require.NoError(t, err)
	before := pool.Current()
	renameOver(t, store, certs[1], now)
	assert.NotSame(t, before, pool.Current(), "a pool: the store replaced at the same size and time is read again")
}
