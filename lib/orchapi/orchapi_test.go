package orchapi_test

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
)

func TestProofOfAKey(t *testing.T) {
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	require.NoError(t, err)
	tv, sig, err := orchapi.SignProof(key, "host-a", now)
	require.NoError(t, err)

	require.NoError(t, orchapi.VerifyProof(pub, "host-a", tv, sig, now), "control: the matching proof holds")

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	otherPub, err := x509.MarshalPKIXPublicKey(&other.PublicKey)
	require.NoError(t, err)
	require.ErrorIs(t, orchapi.VerifyProof(otherPub, "host-a", tv, sig, now), orchapi.ErrBadProof, "another key")
	require.ErrorIs(t, orchapi.VerifyProof(pub, "host-b", tv, sig, now), orchapi.ErrBadProof, "another host")
	require.ErrorIs(t, orchapi.VerifyProof(pub, "host-a", tv, sig, now.Add(time.Hour)), orchapi.ErrBadProof, "a stale proof")
}

func TestProofOfAnEd25519Key(t *testing.T) {
	now := time.Now()
	pubKey, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	pub, err := x509.MarshalPKIXPublicKey(pubKey)
	require.NoError(t, err)
	tv, sig, err := orchapi.SignProof(priv, "host-a", now)
	require.NoError(t, err)
	require.NoError(t, orchapi.VerifyProof(pub, "host-a", tv, sig, now))
}

type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newAuthority(t *testing.T) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return authority{cert, key}
}

// server issues a certificate for 127.0.0.1 (or for the other IP when ip is set) and serves with
// the chain leaf, ca.
func (a authority) server(t *testing.T, ip string) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "orch"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP(ip)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der, a.cert.Raw}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func TestPinCATrustsOnlyTheChainOfThePinnedAuthority(t *testing.T) {
	ca := newAuthority(t)
	srv := ca.server(t, "127.0.0.1")

	try := func(pin, host string) error {
		conf, err := orchapi.PinCA(pin, host, nil)
		require.NoError(t, err)
		c, err := orchapi.New(srv.URL, orchapi.HTTPClient(conf, 5*time.Second))
		require.NoError(t, err)
		_, err = c.Hosts(t.Context())
		return err
	}
	require.NoError(t, try(fingerprint(ca.cert), "127.0.0.1"), "control: the pinned authority and the dialled host")
	require.Error(t, try(fingerprint(newAuthority(t).cert), "127.0.0.1"), "another authority's fingerprint")
	require.Error(t, try(fingerprint(ca.cert), "10.9.9.9"), "a certificate that does not name the host")
}

// resigned is a's certificate signed again under its key, with a new serial and a later end, as the
// signer renews an authority.
func (a authority) resigned(t *testing.T) *x509.Certificate {
	t.Helper()
	tmpl := *a.cert
	tmpl.SerialNumber, tmpl.NotAfter = big.NewInt(9), a.cert.NotAfter.Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &a.key.PublicKey, a.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

// serve serves a leaf for 127.0.0.1 that signer signed, with chain after it.
func serve(t *testing.T, signer authority, chain ...*x509.Certificate) *httptest.Server {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "orch"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer.cert, &key.PublicKey, signer.key)
	require.NoError(t, err)
	raw := [][]byte{der}
	for _, c := range chain {
		raw = append(raw, c.Raw)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("[]")) }))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: raw, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server, pin string) error {
	t.Helper()
	conf, err := orchapi.PinCA(pin, "127.0.0.1", nil)
	require.NoError(t, err)
	c, err := orchapi.New(srv.URL, orchapi.HTTPClient(conf, 5*time.Second))
	require.NoError(t, err)
	_, err = c.Hosts(t.Context())
	return err
}

func TestAKeyPinHoldsAcrossTheAuthoritysReSigningAndACertificatePinDoesNot(t *testing.T) {
	ca := newAuthority(t)
	renewed := ca.resigned(t)
	srv := serve(t, ca, renewed)

	require.Equal(t, orchapi.KeyPin(ca.cert), orchapi.KeyPin(renewed), "one key, one pin")
	require.NoError(t, dial(t, srv, orchapi.KeyPin(ca.cert)), "the key pin trusts the re-signed certificate")
	require.NoError(t, dial(t, serve(t, ca, ca.cert), fingerprint(ca.cert)), "control: until v0.1.0-rc.1 a certificate's hash pins its authority")
	require.Error(t, dial(t, srv, fingerprint(ca.cert)), "a certificate's hash names that certificate, which a re-signing replaces")
	require.Error(t, dial(t, srv, orchapi.KeyPin(newAuthority(t).cert)), "another key's pin")
}

func TestACertificateUnderAPinnedNameWithAnotherKeyIsNoRoot(t *testing.T) {
	ca := newAuthority(t)
	forger := newAuthority(t) // the same subject, "ca", under another key
	require.Equal(t, ca.cert.Subject.String(), forger.cert.Subject.String())
	require.NoError(t, dial(t, serve(t, ca, forger.cert, ca.cert), orchapi.KeyPin(ca.cert)),
		"control: the pinned authority's leaf verifies with a stranger in the chain")
	require.Error(t, dial(t, serve(t, forger, forger.cert, ca.cert), orchapi.KeyPin(ca.cert)),
		"a leaf the stranger signed does not, though its certificate carries the pinned authority's name")
}
