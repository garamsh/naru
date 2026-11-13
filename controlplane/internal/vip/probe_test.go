package vip

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
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issue returns a certificate of the CA (self-signed when parent is nil) and writes it and its key.
func issue(t *testing.T, dir, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, IsCA: isCA, BasicConstraintsValid: true,
	}
	if isCA {
		tpl.KeyUsage |= x509.KeyUsageCertSign
	}
	signer, signerKey := tpl, key
	if parent != nil {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, signer, &key.PublicKey, signerKey)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600))
	return cert, key
}

// liveMember is a Node with an API server port that accepts connections and an etcd /health that
// answers 200 while healthy is set; it requires a client certificate of the CA.
func liveMember(t *testing.T, dir string, healthy *atomic.Bool) (apiPort, etcdPort string) {
	t.Helper()
	ca, caKey := issue(t, dir, "ca", nil, nil, true)
	issue(t, dir, "server", ca, caKey, false)
	issue(t, dir, "client", ca, caKey, false)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key"))
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() && r.URL.Path == "/health" {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	api, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = api.Close() })
	go func() {
		for {
			c, err := api.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, apiPort, _ = net.SplitHostPort(api.Addr().String())
	_, etcdPort, _ = net.SplitHostPort(srv.Listener.Addr().String())
	return apiPort, etcdPort
}

func TestLiveProbe_AMemberWhoseEtcdIsUnhealthyIsNotAliveThoughItsAPIServerAcceptsConnections(t *testing.T) {
	dir := t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	apiPort, etcdPort := liveMember(t, dir, &healthy)
	etcd := Etcd{Port: etcdPort, CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "client.pem"), KeyFile: filepath.Join(dir, "client.key")}
	probe := LiveProbe(apiPort, etcd, time.Second)
	lo := netip.MustParseAddr("127.0.0.1")

	assert.True(t, probe(t.Context(), lo), "the API server accepts and etcd is healthy")
	healthy.Store(false)
	assert.False(t, probe(t.Context(), lo), "etcd answers 503: the API server accepts but its requests fail")
	healthy.Store(true)
	etcd.KeyFile = filepath.Join(dir, "missing.key")
	assert.False(t, LiveProbe(apiPort, etcd, time.Second)(t.Context(), lo), "a probe that cannot present the client pair finds no member alive")
}
