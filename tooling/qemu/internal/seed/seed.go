// Package seed edits the user-data of a Node that runs as a microVM, for what naructl writes no
// command for: `naructl gen kubernetes-node` has no spec.api, and the test needs the microVM's Node
// API to answer.
package seed

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

	"go.yaml.in/yaml/v3"
)

// AddNodeAPI adds spec.api to the user-data in file: the Node API authority under caDir (api/ca.pem,
// which `naructl gen api-ca` wrote) as the client CA, and a serving pair from it for ips. The
// authority's key is read to sign and is not carried.
func AddNodeAPI(file, caDir string, ips ...net.IP) error {
	ca, key, err := loadCA(filepath.Join(caDir, "api"))
	if err != nil {
		return err
	}
	certPEM, keyPEM, err := serving(ca, key, ips)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return err
	}
	spec := field(doc.Content[0], "spec")
	if spec == nil {
		return errors.New("the user-data has no spec")
	}
	if field(spec, "api") != nil {
		return errors.New("the user-data already has spec.api")
	}
	var api yaml.Node
	if err := api.Encode(map[string]string{
		"clientCA":    string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Raw})),
		"certificate": string(certPEM),
		"key":         string(keyPEM),
	}); err != nil {
		return err
	}
	spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "api"}, &api)
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o600)
}

func field(m *yaml.Node, key string) *yaml.Node {
	for i := 0; m != nil && i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func loadCA(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return nil, nil, fmt.Errorf("the Node API authority (run gen api-ca): %w", err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, "ca.key"))
	if err != nil {
		return nil, nil, fmt.Errorf("the Node API authority's key: %w", err)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, nil, errors.New("the Node API authority's files are not PEM")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, nil, err
	}
	return cert, key, nil
}

// serving issues a serving certificate for ips, valid for a day.
func serving(ca *x509.Certificate, caKey *ecdsa.PrivateKey, ips []net.IP) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "microvm"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: ips,
	}, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}
