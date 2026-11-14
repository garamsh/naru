// Package orchtest is what orch's tests stand on: a throwaway authority, stand-ins for the signer
// and the identity checks, and a cluster of in-process members on localhost. It holds test keys
// only, generated per run.
package orchtest

import (
	"crypto/ecdsa"
	"crypto/ed25519"
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
	"sync"
	"testing"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

// CA is a throwaway authority.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	mu   sync.Mutex
	next int64
	// revokeKey signs the revocation list, which holds the serials revoked so far.
	revokePub  ed25519.PublicKey
	revokeKey  ed25519.PrivateKey
	revoked    []string
	revokedSeq uint64
}

// NewCA makes an authority valid for a day.
func NewCA(t testing.TB) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &CA{cert: cert, key: key, next: 1, revokePub: pub, revokeKey: priv, revokedSeq: 1}
}

// RevocationKey verifies the lists CRL signs.
func (c *CA) RevocationKey() ed25519.PublicKey { return c.revokePub }

// Revoke puts the identity's certificate on the revocation list.
func (c *CA) Revoke(id Identity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revoked = append(c.revoked, identity.SerialOf(id.Cert))
	c.revokedSeq++
}

// CRL is the signed revocation list.
func (c *CA) CRL() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return identity.SignCRL(c.revokeKey, identity.CRL{Seq: c.revokedSeq, IssuedAt: time.Now(), Serials: c.revoked})
}

// SignCRL signs a list with the revocation key, as a signer replica does.
func (c *CA) SignCRL(crl identity.CRL) ([]byte, error) {
	return identity.SignCRL(c.revokeKey, crl)
}

// Pool is a pool holding the authority.
func (c *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(c.cert)
	return p
}

// PEM is the authority's certificate.
func (c *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.cert.Raw})
}

func (c *CA) sign(subject, role string, pub any, ips []net.IP) (*x509.Certificate, []byte, error) {
	c.mu.Lock()
	c.next++
	serial := c.next
	c.mu.Unlock()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: subject, OrganizationalUnit: []string{role}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, pub, c.key)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	return cert, der, err
}

// Identity is a certificate with its key.
type Identity struct {
	TLS  tls.Certificate
	Cert *x509.Certificate
}

// Issue makes a client identity with the role in its Organization.
func (c *CA) Issue(t testing.TB, subject string, role identity.Role) Identity {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert, der, err := c.sign(subject, string(role), &key.PublicKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Identity{TLS: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}, Cert: cert}
}

// Files is an orch member's pair and the authority, written under dir.
type Files struct{ Cert, Key, CA string }

// IssueFiles makes a certificate for 127.0.0.1 and writes it with its key and the authority.
func (c *CA) IssueFiles(t testing.TB, dir, name string) Files {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, der, err := c.sign(name, string(identity.RoleOrch), &key.PublicKey, []net.IP{net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	f := Files{Cert: filepath.Join(dir, name+".crt"), Key: filepath.Join(dir, name+".key"), CA: filepath.Join(dir, "ca.crt")}
	write := func(path string, mode os.FileMode, b []byte) {
		if err := os.WriteFile(path, b, mode); err != nil {
			t.Fatal(err)
		}
	}
	write(f.Cert, 0o644, append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), c.PEM()...))
	write(f.Key, 0o600, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}))
	write(f.CA, 0o644, c.PEM())
	return f
}

// SignCSR signs a host's certificate request as the stand-in signer does.
func (c *CA) SignCSR(csrDER []byte, subject string, role identity.Role) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	_, der, err := c.sign(subject, string(role), csr.PublicKey, nil)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
