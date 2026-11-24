package machineapifake

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/machineapi"
)

// Server is a Fake served over mutual TLS 1.3 for a test, with an authority of its own that issues
// the clients' certificates.
type Server struct {
	*Fake
	URL  string
	t    testing.TB
	srv  *httptest.Server
	ca   *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

// Start serves a Fake of o until the test ends.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	s := &Server{Fake: New(o), t: t}
	s.ca, s.key = s.issue(&x509.Certificate{
		Subject: pkix.Name{CommonName: "machineapifake CA"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign,
	}, nil, nil)
	s.pool = x509.NewCertPool()
	s.pool.AddCert(s.ca)
	s.srv = httptest.NewUnstartedServer(s.Fake)
	s.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: s.pool}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// Client returns a client of the Fake holding a caller certificate named caller.
func (s *Server) Client(caller string) *machineapi.Client {
	return s.ClientOfRole(caller, CallerRole)
}

// ClientOfRole returns a client of the Fake holding a certificate named name of the role role,
// which the Fake refuses unless role is CallerRole.
func (s *Server) ClientOfRole(name, role string) *machineapi.Client {
	s.t.Helper()
	leaf, key := s.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name, OrganizationalUnit: []string{role}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}, s.ca, s.key)
	pair := tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}
	roots := x509.NewCertPool()
	roots.AddCert(s.srv.Certificate())
	hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair},
	}}}
	c, err := machineapi.New(s.URL, hc)
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	return c
}

// issue signs tmpl with parent's key, or itself when parent is nil.
func (s *Server) issue(tmpl, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey) {
	s.t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	tmpl.SerialNumber = serial
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	return cert, key
}
