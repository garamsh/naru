package vip

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	golog "log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// logBuffer is the log of a test server, which a probe's connections may write to.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// tlsServer serves handler over TLS with the pair of the CA, writing the server's own log to log.
// With clientAuth it requires a client certificate of the CA.
func tlsServer(t *testing.T, dir, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, clientAuth bool, log *logBuffer, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	issue(t, dir, name, ca, caKey, false)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key"))
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ErrorLog = golog.New(log, "", 0)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: pool}
	if clientAuth {
		srv.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func portOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	require.NoError(t, err)
	return port
}

// liveMember is a Node whose API server answers 401 to every request, as one does that asks who
// the client is, and whose etcd /health answers 200 while healthy is set and requires a client
// certificate of the CA. apiLog is what the API server logged of the connections it was given.
func liveMember(t *testing.T, dir string, healthy *atomic.Bool) (api API, etcd Etcd, apiLog *logBuffer) {
	t.Helper()
	ca, caKey := issue(t, dir, "ca", nil, nil, true)
	issue(t, dir, "client", ca, caKey, false)
	apiLog = &logBuffer{}
	apiSrv := tlsServer(t, dir, "api", ca, caKey, false, apiLog, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	etcdSrv := tlsServer(t, dir, "etcd", ca, caKey, true, &logBuffer{}, func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() && r.URL.Path == "/health" {
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	caFile := filepath.Join(dir, "ca.pem")
	return API{Port: portOf(t, apiSrv), Name: "127.0.0.1", CAFile: caFile},
		Etcd{Port: portOf(t, etcdSrv), CAFile: caFile, CertFile: filepath.Join(dir, "client.pem"), KeyFile: filepath.Join(dir, "client.key")},
		apiLog
}

func TestLiveProbe_AMemberWhoseEtcdIsUnhealthyIsNotAliveThoughItsAPIServerAnswers(t *testing.T) {
	dir := t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	api, etcd, _ := liveMember(t, dir, &healthy)
	probe := LiveProbe(api, etcd, time.Second)
	lo := netip.MustParseAddr("127.0.0.1")

	assert.True(t, probe(t.Context(), lo), "the API server answers, though with 401, and etcd is healthy")
	healthy.Store(false)
	assert.False(t, probe(t.Context(), lo), "etcd answers 503: the API server answers but its requests fail")
	healthy.Store(true)
	etcd.KeyFile = filepath.Join(dir, "missing.key")
	assert.False(t, LiveProbe(api, etcd, time.Second)(t.Context(), lo), "a probe that cannot present the client pair finds no member alive")
}

func TestLiveProbe_AnAPIServerThatDoesNotAnswerOverTLSIsNotAlive(t *testing.T) {
	dir := t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	api, etcd, _ := liveMember(t, dir, &healthy)
	lo := netip.MustParseAddr("127.0.0.1")
	require.True(t, LiveProbe(api, etcd, time.Second)(t.Context(), lo), "control: the same member is alive")

	other := t.TempDir()
	foreign, foreignKey := issue(t, other, "ca", nil, nil, true)
	foreignSrv := tlsServer(t, other, "api", foreign, foreignKey, false, &logBuffer{}, func(http.ResponseWriter, *http.Request) {})
	untrusted := api
	untrusted.Port = portOf(t, foreignSrv)
	assert.False(t, LiveProbe(untrusted, etcd, time.Second)(t.Context(), lo), "a certificate of another CA is not an API server of this cluster")

	named := api
	named.Name = "192.0.2.1"
	assert.False(t, LiveProbe(named, etcd, time.Second)(t.Context(), lo), "a certificate that does not name the address is refused")

	plain, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = plain.Close() })
	go func() {
		for {
			c, err := plain.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(plain.Addr().String())
	accepting := api
	accepting.Port = port
	assert.False(t, LiveProbe(accepting, etcd, time.Second)(t.Context(), lo), "a port that accepts a connection and closes it is not an API server")
}

func TestLiveProbe_ProbingAnAPIServerLeavesItNothingToLog(t *testing.T) {
	dir := t.TempDir()
	var healthy atomic.Bool
	healthy.Store(true)
	api, etcd, apiLog := liveMember(t, dir, &healthy)
	probe := LiveProbe(api, etcd, time.Second)
	lo := netip.MustParseAddr("127.0.0.1")

	for range 20 {
		require.True(t, probe(t.Context(), lo))
	}
	// A closed connection that never handshook is what the server logs; shown to be logged, so that
	// an empty log above means no such connection was made.
	c, err := net.Dial("tcp", "127.0.0.1:"+api.Port)
	require.NoError(t, err)
	require.NoError(t, c.Close())
	assert.Eventually(t, func() bool { return strings.Contains(apiLog.String(), "TLS handshake error") }, 5*time.Second, 10*time.Millisecond, "control: a dial that closes is logged")
	assert.Equal(t, 1, strings.Count(apiLog.String(), "TLS handshake error"), "only the control's dial was logged, none of the 20 probes")
}
