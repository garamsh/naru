// Package pki makes a cluster's certificate authority and the certificates it signs: the pairs a
// Node carries as files and the operator's client pair. Every key is ECDSA P-256 and PEM encoded.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// CAValidity is how long the authority's certificate is valid.
	CAValidity = 10 * 365 * 24 * time.Hour
	// LeafValidity is how long a certificate the CA signs here is valid. A certificate that init
	// issues from spec.pki has its own validity and is renewed.
	LeafValidity = 365 * 24 * time.Hour
	// skew is how far before the moment of issue a certificate is valid, so that a clock a little
	// behind the issuer's accepts it.
	skew = time.Hour

	// CertFile and KeyFile are the names of the authority's files in the directory of gen ca.
	CertFile = "ca.pem"
	KeyFile  = "ca.key"
)

// Pair is a certificate and its private key, both PEM.
type Pair struct {
	CertPEM, KeyPEM []byte
}

// CA is a certificate authority.
type CA struct {
	// CertPEM is the authority's certificate and KeyPEM its private key.
	CertPEM, KeyPEM []byte
	cert            *x509.Certificate
	key             *ecdsa.PrivateKey
}

// New returns a new self-signed authority named cn.
func New(cn string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-skew),
		NotAfter:              now.Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return load(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// Load reads the authority's files, CertFile and KeyFile, from dir.
func Load(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, KeyFile))
	if err != nil {
		return nil, err
	}
	return load(certPEM, keyPEM)
}

func load(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil || cb.Type != "CERTIFICATE" {
		return nil, errors.New("the authority's certificate is not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the authority's certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, errors.New("the certificate is not a certificate authority's")
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("the authority's key is not PEM")
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse the authority's key: %w", err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, errors.New("the key is not the certificate's")
	}
	return &CA{CertPEM: certPEM, KeyPEM: keyPEM, cert: cert, key: key}, nil
}

// Write writes the authority into dir, which it creates: the certificate 0644 and the key 0600.
func (c *CA) Write(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // holds a certificate
		return err
	}
	return writePair(dir, "ca", c.CertPEM, c.KeyPEM)
}

// WritePair writes p into dir, which it creates, as <name>.pem (0644) and <name>.key (0600).
func WritePair(dir, name string, p Pair) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // holds a certificate
		return err
	}
	return writePair(dir, name, p.CertPEM, p.KeyPEM)
}

func writePair(dir, name string, certPEM, keyPEM []byte) error {
	if err := os.WriteFile(filepath.Join(dir, name+".pem"), certPEM, 0o644); err != nil { //nolint:gosec // a certificate
		return err
	}
	return os.WriteFile(filepath.Join(dir, name+".key"), keyPEM, 0o600)
}

// Server issues a serving certificate with cn as its subject and each of ips as an IP SAN.
func (c *CA) Server(cn string, ips ...net.IP) (Pair, error) {
	if len(ips) == 0 {
		return Pair{}, errors.New("a serving certificate needs at least one IP address")
	}
	return c.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
	})
}

// Client issues a client certificate whose subject CN, the caller's identity to gaon, is cn.
func (c *CA) Client(cn string) (Pair, error) {
	if cn == "" {
		return Pair{}, errors.New("a client certificate needs a CN")
	}
	return c.issue(&x509.Certificate{
		Subject:     pkix.Name{CommonName: cn},
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
}

func (c *CA) issue(tmpl *x509.Certificate) (Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return Pair{}, err
	}
	now := time.Now()
	tmpl.SerialNumber = serial()
	tmpl.NotBefore = now.Add(-skew)
	tmpl.NotAfter = now.Add(LeafValidity)
	if tmpl.NotAfter.After(c.cert.NotAfter) {
		tmpl.NotAfter = c.cert.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return Pair{}, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return Pair{}, err
	}
	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		// crypto/rand does not fail on a working system; an issuer cannot go on without it.
		panic(fmt.Sprintf("pki: read random bytes: %v", err))
	}
	return n
}
