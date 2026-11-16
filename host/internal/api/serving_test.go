package api

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writePair(t *testing.T, dir, name string, crt, key []byte) (string, string) {
	t.Helper()
	c, k := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	require.NoError(t, os.WriteFile(c, crt, 0o600))
	require.NoError(t, os.WriteFile(k, key, 0o600))
	return c, k
}

func TestServing_ReplaceServesTheNewPairAndStoresItPrivately(t *testing.T) {
	dir := t.TempDir()
	ca := newAuthority(t, "servers")
	_, oldCrt, oldKey := ca.issue(t, nil, true)
	newPair, newCrt, newKey := ca.issue(t, nil, true)
	c, k := writePair(t, dir, "old", oldCrt, oldKey)
	stored := filepath.Join(dir, "sub", "serving.pem")
	s, err := NewServing(c, k, stored)
	require.NoError(t, err)
	before, _ := s.GetCertificate(nil)
	require.NotEqual(t, newPair.Certificate[0], before.Certificate[0], "control: the old pair is served first")
	assert.Equal(t, c, s.File())

	require.Error(t, s.Replace(newCrt, oldKey), "a mismatched pair is refused")
	_, err = os.Stat(stored)
	require.True(t, os.IsNotExist(err), "and nothing is stored")

	require.NoError(t, s.Replace(newCrt, newKey))
	got, _ := s.GetCertificate(nil)
	assert.Equal(t, newPair.Certificate[0], got.Certificate[0])
	assert.Equal(t, stored, s.File())
	fi, err := os.Stat(stored)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	reloaded, err := NewServing(stored, stored, stored)
	require.NoError(t, err, "a restart loads what was stored")
	again, _ := reloaded.GetCertificate(nil)
	assert.Equal(t, newPair.Certificate[0], again.Certificate[0])
}

func TestIdentity_PutAlsoReplacesTheServingPair(t *testing.T) {
	dir := t.TempDir()
	ca := newAuthority(t, "servers")
	_, crt, key := ca.issue(t, nil, true)
	c, k := writePair(t, dir, "seed", crt, key)
	serving, err := NewServing(c, k, filepath.Join(dir, "serving.pem"))
	require.NoError(t, err)
	stub := &pairStub{}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.Identity = stub; d.Serving = serving })
	_, nCrt, nKey := ca.issue(t, nil, true)
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/identity", map[string]string{"certificate": string(nCrt), "key": string(nKey)})
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, serving.stored, serving.File())
}
