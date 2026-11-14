package signer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// Files are where orch keeps its own identity: its key, its certificate with the authority's after
// it, and the authority.
type Files struct{ Cert, Key, CA string }

// EnsureIdentity leaves orch's identity at f. When f.Cert and f.Key exist it changes nothing;
// otherwise it generates a key, asks the signer to certify it as name for the given hosts (DNS
// names and IP addresses) and writes the key 0600 and the certificates.
func (c *Client) EnsureIdentity(ctx context.Context, f Files, name string, hosts []string) error {
	if _, err := os.Stat(f.Cert); err == nil {
		if _, err := os.Stat(f.Key); err == nil {
			return nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return err
	}
	got, err := c.IssueOrch(ctx, csr)
	if err != nil {
		return fmt.Errorf("the signer did not certify orch: %w", err)
	}
	if len(got.Certificate) == 0 || len(got.CA) == 0 {
		return errors.New("the signer's answer holds no certificate or no authority")
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.Key), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(f.Key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(f.CA, got.CA, 0o644); err != nil {
		return err
	}
	chain := append(append([]byte{}, got.Certificate...), got.CA...)
	return os.WriteFile(f.Cert, chain, 0o644)
}
