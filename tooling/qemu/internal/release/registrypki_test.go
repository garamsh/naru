package release

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryCerts_AVersionAndAnEpochFixTheAuthorityAndTheLeaf(t *testing.T) {
	a, err := registryCerts("v1", testEpoch)
	require.NoError(t, err)
	b, err := registryCerts("v1", testEpoch)
	require.NoError(t, err)
	other, err := registryCerts("v2", testEpoch)
	require.NoError(t, err)

	assert.Equal(t, a, b, "one version and epoch, one set of bytes")
	assert.NotEqual(t, a.CA, other.CA)
	assert.NotEqual(t, a.Cert, other.Cert)
	assert.NotEqual(t, a.Key, other.Key)
}

func TestRegistryCerts_TheLeafIsValidFor127001UnderTheAuthorityFromTheEpoch(t *testing.T) {
	c, err := registryCerts("v1", testEpoch)
	require.NoError(t, err)
	parse := func(b []byte) *x509.Certificate {
		block, _ := pem.Decode(b)
		cert, err := x509.ParseCertificate(block.Bytes)
		require.NoError(t, err)
		return cert
	}
	ca, leaf := parse(c.CA), parse(c.Cert)

	assert.Equal(t, registryCAName, ca.Subject.CommonName)
	assert.Equal(t, time.Unix(testEpoch, 0).UTC().Add(-time.Hour), ca.NotBefore)
	assert.Equal(t, ca.NotBefore, leaf.NotBefore)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "", CurrentTime: time.Unix(testEpoch, 0)})
	require.NoError(t, err)
	require.NoError(t, leaf.VerifyHostname(net.IPv4(127, 0, 0, 1).String()))
	assert.Error(t, leaf.VerifyHostname("10.0.0.1"))
}

func TestRegistryCerts_TheEpochIsRequired(t *testing.T) {
	_, err := registryCerts("v1", 0)
	assert.Error(t, err)
}

func TestWriteRegistryCerts_ReplacesWhatStageWrote(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"ca.pem", "cert.pem", "key.pem"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte("random"), 0o644))
	}
	require.NoError(t, writeRegistryCerts(dir, "v1", testEpoch))
	want, err := registryCerts("v1", testEpoch)
	require.NoError(t, err)

	assert.Equal(t, string(want.CA), read(t, dir, "ca.pem"))
	assert.Equal(t, string(want.Cert), read(t, dir, "cert.pem"))
	assert.Equal(t, string(want.Key), read(t, dir, "key.pem"))
}
