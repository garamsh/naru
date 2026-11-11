// Package pki issues the certificates of a test cluster's gaon services: a throwaway CA, the API
// server's serving certificate, and one client certificate per caller. It is test tooling: naru
// issues no certificate at boot and carries the files the tooling issues (ADR 0012).
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
	"time"
)

// validity is how long an issued certificate is valid, from an hour before it is issued, which
// leaves room for a clock a little behind the host's. A test cluster lives for minutes.
const validity = 48 * time.Hour

// Pair is a certificate and its private key, both PEM.
type Pair struct {
	CertPEM, KeyPEM []byte
}

// CA is a certificate authority that signs the certificates of one cluster.
type CA struct {
	// PEM is the CA's certificate.
	PEM  []byte
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// NewCA returns a new self-signed CA.
func NewCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "naru-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), cert: cert, key: key}, nil
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
	tmpl.NotBefore = now.Add(-time.Hour)
	tmpl.NotAfter = now.Add(validity)
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

// Cluster is every certificate a gaon test cluster uses, issued by one CA.
type Cluster struct {
	// CA is the CA's certificate, PEM: the file every service names as the CA it trusts.
	CA []byte
	// APIServer is the API server's serving certificate.
	APIServer Pair
	// Agents holds one client certificate per Node, its CN the Node's name.
	Agents map[string]Pair
	// Scheduler and Controllers are the client certificates of those services.
	Scheduler, Controllers Pair
	// Client is the test host's client certificate.
	Client Pair
}

// NewCluster issues the certificates of a cluster of the named Nodes whose API server answers at
// serverIPs: an agent certificate for each Node, one for the scheduler and one for the controllers,
// one for the host that tests the cluster, and the serving certificate.
func NewCluster(nodes []string, serverIPs ...net.IP) (*Cluster, error) {
	ca, err := NewCA()
	if err != nil {
		return nil, err
	}
	c := &Cluster{CA: ca.PEM, Agents: map[string]Pair{}}
	if c.APIServer, err = ca.Server("gaon-apiserver", serverIPs...); err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if c.Agents[n], err = ca.Client(n); err != nil {
			return nil, err
		}
	}
	if c.Scheduler, err = ca.Client("gaon-scheduler"); err != nil {
		return nil, err
	}
	if c.Controllers, err = ca.Client("gaon-controllers"); err != nil {
		return nil, err
	}
	if c.Client, err = ca.Client("naru-test-host"); err != nil {
		return nil, err
	}
	return c, nil
}
