package command

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	must "github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout/pki"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	must.NoError(t, err)
	return b
}

func TestServiceAccountKey_ARotationThatEndsBetweenTheFilesLeavesAPairThatVerifiesAndCanBeRetried(t *testing.T) {
	dir := t.TempDir()
	must.NoError(t, serviceAccountKey(dir, saKeyOptions{alg: pki.RS256}))
	oldKey := readFile(t, filepath.Join(dir, "sa.key"))
	oldPub, err := pki.SigningPublicKey(oldKey)
	must.NoError(t, err)
	kid, err := pki.SigningKeyID(oldPub)
	must.NoError(t, err)
	real := writeKeyFile
	t.Cleanup(func() { writeKeyFile = real })
	writeKeyFile = func(path string, data []byte, perm os.FileMode) error {
		if filepath.Base(path) == "sa.key" {
			return errors.New("the run ends here")
		}
		return real(path, data, perm)
	}

	err = serviceAccountKey(dir, saKeyOptions{rotateKid: kid})

	must.Error(t, err)
	assert.Equal(t, oldKey, readFile(t, filepath.Join(dir, "sa.key")), "the key that signs is the old one")
	keys, err := pki.PublicKeys(readFile(t, filepath.Join(dir, "sa.pub")))
	must.NoError(t, err)
	must.Len(t, keys, 2, "sa.pub already holds the new key and the old one")
	oldID, _ := pki.KeyID(keys[1])
	assert.Equal(t, kid, oldID, "the old key's public key verifies the tokens it signed")

	writeKeyFile = real
	must.NoError(t, serviceAccountKey(dir, saKeyOptions{rotateKid: kid}), "the retry with the same key id is not refused")
	keys, err = pki.PublicKeys(readFile(t, filepath.Join(dir, "sa.pub")))
	must.NoError(t, err)
	assert.Len(t, keys, 2)
	oldID, _ = pki.KeyID(keys[1])
	assert.Equal(t, kid, oldID, "the retry keeps the key that signed the tokens in use")
}

func TestIssuerDir_IsUnderOutDirAndRefusesAPathThatLeavesIt(t *testing.T) {
	out := t.TempDir()

	dir, err := issuerDir(out, "https://oidc.example.com/a/b")
	assert.NoError(t, err)
	assert.Equal(t, filepath.Join(out, "a", "b"), dir)
	dir, err = issuerDir(out, "https://oidc.example.com")
	assert.NoError(t, err)
	assert.Equal(t, out, dir, "an issuer with no path is served at the root")

	for _, bad := range []string{"https://oidc.example.com/..", "https://oidc.example.com/../x", "https://oidc.example.com/a/../../x"} {
		_, err := issuerDir(out, bad)
		assert.ErrorContains(t, err, "outside --out-dir", bad)
	}
}

func TestServiceAccountKey_AFirstRunThatEndedAfterTheKeyIsRepairedByTheNextAndOneThatListsAnotherKeyFirstIsRefused(t *testing.T) {
	clean := t.TempDir()
	must.NoError(t, serviceAccountKey(clean, saKeyOptions{alg: pki.RS256}))
	cleanPub := readFile(t, filepath.Join(clean, "sa.pub"))

	dir := t.TempDir()
	real := writeKeyFile
	t.Cleanup(func() { writeKeyFile = real })
	writeKeyFile = func(path string, data []byte, perm os.FileMode) error {
		if filepath.Base(path) == "sa.pub" {
			return errors.New("the run ends here")
		}
		return real(path, data, perm)
	}
	must.Error(t, serviceAccountKey(dir, saKeyOptions{alg: pki.RS256}))
	_, err := os.Stat(filepath.Join(dir, "sa.pub"))
	must.ErrorIs(t, err, os.ErrNotExist)
	writeKeyFile = real

	must.NoError(t, serviceAccountKey(dir, saKeyOptions{alg: pki.RS256}))

	signer, err := pki.SigningPublicKey(readFile(t, filepath.Join(dir, "sa.key")))
	must.NoError(t, err)
	assert.Equal(t, signer, readFile(t, filepath.Join(dir, "sa.pub")), "the retry writes sa.pub from the key sa.key holds")
	assert.Equal(t, len(cleanPub), len(signer), "control: a clean first run leaves the same shape of pair")
	must.NoError(t, serviceAccountKey(dir, saKeyOptions{alg: pki.RS256}), "a complete pair is left as it is")

	_, otherPub, err := pki.ServiceAccountKey(pki.RS256)
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(filepath.Join(dir, "sa.pub"), otherPub, 0o644))
	err = serviceAccountKey(dir, saKeyOptions{alg: pki.RS256})
	must.Error(t, err)
	assert.ErrorContains(t, err, "lists the key")
	assert.Equal(t, otherPub, readFile(t, filepath.Join(dir, "sa.pub")), "a refused run changes nothing")
}
