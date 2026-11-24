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
	s.issuer = &issuer{roots: s.pool, sign: s.signCaller}
	s.srv = httptest.NewUnstartedServer(s.Fake)
	s.srv.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: s.pool}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	s.URL = s.srv.URL
	return s
}

// Client returns a client of the Fake holding a caller certificate named caller.
func (s *Server) Client(caller string) *machineapi.Client {
	c, _, _ := s.ClientWithPair(caller, CallerRole)
	return c
}

// ClientOfRole returns a client of the Fake holding a certificate named name of the role role,
// which the Fake refuses unless role is CallerRole.
func (s *Server) ClientOfRole(name, role string) *machineapi.Client {
	c, _, _ := s.ClientWithPair(name, role)
	return c
}

// ClientWithPair is ClientOfRole, with the key and the certificate the client presents. The
// certificate lasts 24 hours, so a caller's is due for renewal.
func (s *Server) ClientWithPair(name, role string) (*machineapi.Client, *ecdsa.PrivateKey, *x509.Certificate) {
	s.t.Helper()
	leaf, key := s.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: name, OrganizationalUnit: []string{role}},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}, s.ca, s.key)
	return s.client(&tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key, Leaf: leaf}), key, leaf
}

// AnonymousClient returns a client of the Fake that presents no certificate: the one a caller
// registers with.
func (s *Server) AnonymousClient() *machineapi.Client { return s.client(nil) }

func (s *Server) client(pair *tls.Certificate) *machineapi.Client {
	s.t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(s.srv.Certificate())
	conf := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}
	if pair != nil {
		conf.Certificates = []tls.Certificate{*pair}
	}
	c, err := machineapi.New(s.URL, &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: conf}})
	if err != nil {
		s.t.Fatalf("machineapifake: %v", err)
	}
	return c
}

// signCaller signs a caller certificate named name for pub under the Server's authority.
func (s *Server) signCaller(name string, pub any, notAfter time.Time) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name, OrganizationalUnit: []string{CallerRole}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, KeyUsage: x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, s.ca, pub, s.key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
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
