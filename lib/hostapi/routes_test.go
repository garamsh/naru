package hostapi_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

// selfSigned is a pair for name, PEM.
func selfSigned(t *testing.T, name string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestRouteCheck_RefusesARouteAProxyCannotServeAndParsesAnAliassPair(t *testing.T) {
	good := hostapi.Route{Hostname: "0123456789abcdef.m.example.org", Machine: "0123456789abcdef", Address: netip.MustParseAddr("10.20.0.5"), Port: 8080, Protocol: hostapi.RouteHTTP, Running: true}
	cert, err := good.Check()
	require.NoError(t, err, "control: a zone name")
	assert.Nil(t, cert, "the zone's certificate serves it")

	crt, key := selfSigned(t, "shop.example.com")
	shop := good
	shop.Hostname, shop.Certificate, shop.Key = "shop.example.com", crt, key
	cert, err = shop.Check()
	require.NoError(t, err, "control: an alias with its own pair")
	require.NotNil(t, cert)
	assert.Equal(t, "shop.example.com", cert.Leaf.Subject.CommonName)

	for name, change := range map[string]func(*hostapi.Route){
		"an upper-case hostname":      func(r *hostapi.Route) { r.Hostname = "Shop.example.com" },
		"an underscore":               func(r *hostapi.Route) { r.Hostname = "a_b.example.com" },
		"a label over 63":             func(r *hostapi.Route) { r.Hostname = strings.Repeat("a", 64) + ".example.com" },
		"no Machine":                  func(r *hostapi.Route) { r.Machine = "" },
		"IPv6":                        func(r *hostapi.Route) { r.Address = netip.MustParseAddr("fd00::5") },
		"loopback":                    func(r *hostapi.Route) { r.Address = netip.MustParseAddr("127.0.0.1") },
		"unspecified":                 func(r *hostapi.Route) { r.Address = netip.MustParseAddr("0.0.0.0") },
		"link-local":                  func(r *hostapi.Route) { r.Address = netip.MustParseAddr("169.254.169.254") },
		"multicast":                   func(r *hostapi.Route) { r.Address = netip.MustParseAddr("224.0.0.1") },
		"broadcast":                   func(r *hostapi.Route) { r.Address = netip.MustParseAddr("255.255.255.255") },
		"port 0":                      func(r *hostapi.Route) { r.Port = 0 },
		"port 65536":                  func(r *hostapi.Route) { r.Port = 65536 },
		"another protocol":            func(r *hostapi.Route) { r.Protocol = "udp" },
		"PROXY protocol v1":           func(r *hostapi.Route) { r.ProxyProtocol = "v1" },
		"a key that does not parse":   func(r *hostapi.Route) { r.Hostname, r.Certificate, r.Key = "shop.example.com", crt, "junk" },
		"a pair for another name":     func(r *hostapi.Route) { r.Certificate, r.Key = crt, key },
		"a certificate without a key": func(r *hostapi.Route) { r.Hostname, r.Certificate = "shop.example.com", crt },
	} {
		r := good
		change(&r)
		_, err := r.Check()
		assert.Error(t, err, name)
	}
}
