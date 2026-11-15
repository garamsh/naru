package hostclient_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
)

// authority is a throwaway host authority.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "host authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return authority{cert: cert, key: key}
}

// leaf issues a server certificate naming dns and ips, as the signer issues a host's.
func (a authority) leaf(t *testing.T, dns []string, ips ...net.IP) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "host"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: dns, IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// hostAnswering is a host API server presenting cert, and the address it is at.
func hostAnswering(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/host":
			_, _ = w.Write([]byte(`{"free":{"cpu":2,"memory":2147483648},"mvmd":{"kernels":[]}}`))
		default:
			_, _ = w.Write([]byte(`{"microvms":[]}`))
		}
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

// readyThrough is whether the client, trusting ca, reaches the host id at addr.
func readyThrough(t *testing.T, ca authority, id, addr string) bool {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	hc := orchapi.HTTPClient(orchapi.TrustCA(pool, nil), 10*time.Second)
	hosts, err := hostclient.New(inventory{{ID: id, Address: addr}}, hc).List(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 1)
	return hosts[0].Ready
}

func TestHostCertificates_AHostIsReachedByTheNameItsCertificateCarries(t *testing.T) {
	ca := newAuthority(t)
	loopback := net.IPv4(127, 0, 0, 1)

	t.Run("the host ID of a host that joined from a seed", func(t *testing.T) {
		addr := hostAnswering(t, ca.leaf(t, []string{"h-0123456789abcdef"}))
		require.True(t, readyThrough(t, ca, "h-0123456789abcdef", addr))
	})
	t.Run("the address of a control host", func(t *testing.T) {
		addr := hostAnswering(t, ca.leaf(t, nil, loopback))
		require.True(t, readyThrough(t, ca, "naru-node-1", addr))
	})
	t.Run("another host's ID is refused", func(t *testing.T) {
		addr := hostAnswering(t, ca.leaf(t, []string{"h-ffffffffffffffff"}))
		require.False(t, readyThrough(t, ca, "h-0123456789abcdef", addr))
	})
	t.Run("a certificate that names neither is refused", func(t *testing.T) {
		addr := hostAnswering(t, ca.leaf(t, nil, net.IPv4(10, 0, 0, 9)))
		require.False(t, readyThrough(t, ca, "h-0123456789abcdef", addr))
	})
	t.Run("the right name under another authority is refused", func(t *testing.T) {
		addr := hostAnswering(t, newAuthority(t).leaf(t, []string{"h-0123456789abcdef"}, loopback))
		require.False(t, readyThrough(t, ca, "h-0123456789abcdef", addr))
	})
}
