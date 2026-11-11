package pki_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/pki"
)

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block, "the certificate is PEM")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return cert
}

func pool(t *testing.T, caPEM []byte) *x509.CertPool {
	t.Helper()
	p := x509.NewCertPool()
	require.True(t, p.AppendCertsFromPEM(caPEM))
	return p
}

func TestNewCluster_IssuesWhatGaonsFlagsTake(t *testing.T) {
	server, forward := net.ParseIP("10.78.0.1"), net.ParseIP("127.0.0.1")
	c, err := pki.NewCluster([]string{"node-1", "node-2"}, server, forward)
	require.NoError(t, err)
	roots := pool(t, c.CA)

	t.Run("the serving certificate names both addresses and chains to the CA", func(t *testing.T) {
		cert := parse(t, c.APIServer.CertPEM)
		_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		require.NoError(t, err)
		assert.NoError(t, cert.VerifyHostname("10.78.0.1"))
		assert.NoError(t, cert.VerifyHostname("127.0.0.1"))
		assert.Error(t, cert.VerifyHostname("10.78.0.2"), "no other address is named")
		_, err = tls.X509KeyPair(c.APIServer.CertPEM, c.APIServer.KeyPEM)
		assert.NoError(t, err, "the key is the certificate's")
	})

	t.Run("each agent's CN is its Node's name", func(t *testing.T) {
		for _, name := range []string{"node-1", "node-2"} {
			cert := parse(t, c.Agents[name].CertPEM)
			assert.Equal(t, name, cert.Subject.CommonName)
			_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			assert.NoError(t, err, name)
			_, err = tls.X509KeyPair(c.Agents[name].CertPEM, c.Agents[name].KeyPEM)
			assert.NoError(t, err, "the key is the certificate's")
		}
	})

	t.Run("every other caller has a client certificate of its own", func(t *testing.T) {
		seen := map[string]bool{}
		for _, p := range []pki.Pair{c.Scheduler, c.Controllers, c.Client, c.Agents["node-1"], c.Agents["node-2"]} {
			cert := parse(t, p.CertPEM)
			_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
			assert.NoError(t, err, cert.Subject.CommonName)
			assert.False(t, seen[cert.Subject.CommonName], "%s is issued once", cert.Subject.CommonName)
			seen[cert.Subject.CommonName] = true
		}
	})

	t.Run("a client certificate is not a serving certificate", func(t *testing.T) {
		cert := parse(t, c.Client.CertPEM)
		_, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		assert.Error(t, err)
	})

	t.Run("another cluster's CA does not verify them", func(t *testing.T) {
		other, err := pki.NewCluster([]string{"node-1"}, server)
		require.NoError(t, err)
		_, err = parse(t, c.Client.CertPEM).Verify(x509.VerifyOptions{Roots: pool(t, other.CA), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		assert.Error(t, err)
	})
}

func TestCA_RefusesWhatIsNotACertificate(t *testing.T) {
	ca, err := pki.NewCA()
	require.NoError(t, err)

	_, err = ca.Server("gaon-apiserver")
	assert.Error(t, err, "a serving certificate with no address")
	_, err = ca.Client("")
	assert.Error(t, err, "a client certificate with no CN")
}
