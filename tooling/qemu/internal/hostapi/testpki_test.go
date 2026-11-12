package hostapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// testCluster is a CA, a serving certificate for 127.0.0.1 and the test host's client pair in files,
// as naructl writes them.
type testCluster struct {
	server tls.Certificate
	roots  *x509.CertPool
	client naructl.Client
}

func issue(t *testing.T, tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func cluster(t *testing.T) testCluster {
	t.Helper()
	ca, caKey, caPEM, _ := issue(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "test-ca"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}, nil, nil)
	_, _, serverCert, serverKey := issue(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "gaon-apiserver"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}, ca, caKey)
	_, _, clientCert, clientKey := issue(t, &x509.Certificate{
		Subject: pkix.Name{CommonName: "naru-test-host"}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, ca, caKey)
	server, err := tls.X509KeyPair(serverCert, serverKey)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(caPEM))
	dir := t.TempDir()
	c := naructl.Client{CA: filepath.Join(dir, "ca.pem"), Cert: filepath.Join(dir, "client.pem"), Key: filepath.Join(dir, "client.key")}
	for path, b := range map[string][]byte{c.CA: caPEM, c.Cert: clientCert, c.Key: clientKey} {
		require.NoError(t, os.WriteFile(path, b, 0o600))
	}
	return testCluster{server: server, roots: roots, client: c}
}
