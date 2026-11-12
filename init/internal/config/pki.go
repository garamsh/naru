package config

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// DefaultValidity is how long a certificate is valid when spec.pki gives none.
const DefaultValidity = 720 * time.Hour

// Usage is what a certificate is issued for.
type Usage string

const (
	// Server is a certificate that serves TLS.
	Server Usage = "server"
	// Client is a certificate that authenticates a TLS client.
	Client Usage = "client"
)

// PKI is the certificate authorities a Node carries and the certificates init
// issues from them at boot.
type PKI struct {
	Authorities  []Authority   `yaml:"authorities"`
	Certificates []Certificate `yaml:"certificates"`
}

// Authority is a certificate authority. Its Key is optional: an authority
// without one is written out and signs nothing.
type Authority struct {
	Name string `yaml:"name"`
	// Certificate is the PEM of the authority's certificate.
	Certificate string `yaml:"certificate"`
	// Key is the PEM of the authority's private key.
	Key string `yaml:"key"`
	// CertificatePath is where init writes the authority's certificate.
	CertificatePath string `yaml:"certificatePath"`
	// KeyPath is where init writes the authority's key, mode 0600, for a
	// service that signs with it. It needs a Key and is optional.
	KeyPath string `yaml:"keyPath"`
}

// Certificate is a certificate and key pair init issues at boot.
type Certificate struct {
	Name string `yaml:"name"`
	// Authority names the authority that signs the certificate. It must carry a key.
	Authority       string `yaml:"authority"`
	CertificatePath string `yaml:"certificatePath"`
	KeyPath         string `yaml:"keyPath"`
	CommonName      string `yaml:"commonName"`
	Organization    string `yaml:"organization"`
	// Usage is "server" or "client".
	Usage       Usage    `yaml:"usage"`
	DNSNames    []string `yaml:"dnsNames"`
	IPAddresses []string `yaml:"ipAddresses"`
	// NodeAddresses adds the Node's interface addresses to the certificate's
	// names, and 127.0.0.1 to a server certificate's.
	NodeAddresses bool `yaml:"nodeAddresses"`
	// Validity is a duration such as "720h". It is DefaultValidity when empty.
	Validity string `yaml:"validity"`
	// Restart names the services init restarts when it renews the certificate.
	Restart []string `yaml:"restart"`
}

// ValidFor returns how long the certificate is valid. It is only meaningful
// once Parse has accepted the certificate.
func (c Certificate) ValidFor() time.Duration {
	if c.Validity == "" {
		return DefaultValidity
	}
	d, _ := time.ParseDuration(c.Validity)
	return d
}

// Signer is an authority's certificate with the key that matches it.
type Signer struct {
	Certificate *x509.Certificate
	Key         crypto.Signer
}

// Signer parses the authority's certificate and key. It refuses an authority
// without a key, a certificate that is not a CA, and a key that does not match.
func (a Authority) Signer() (Signer, error) {
	cert, err := parseCertificate(a.Certificate)
	if err != nil {
		return Signer{}, fmt.Errorf("certificate: %w", err)
	}
	if !cert.IsCA {
		return Signer{}, errors.New("certificate is not a certificate authority")
	}
	if a.Key == "" {
		return Signer{}, errors.New("carries no key")
	}
	key, err := parseKey(a.Key)
	if err != nil {
		return Signer{}, fmt.Errorf("key: %w", err)
	}
	if !publicKeysEqual(key.Public(), cert.PublicKey) {
		return Signer{}, errors.New("key does not match the certificate")
	}
	return Signer{Certificate: cert, Key: key}, nil
}

func parseCertificate(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(s string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("is not PEM")
	}
	var parsed any
	var err error
	switch block.Type {
	case "EC PRIVATE KEY":
		parsed, err = x509.ParseECPrivateKey(block.Bytes)
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("has the unsupported PEM type %q", block.Type)
	}
	if err != nil {
		return nil, err
	}
	signer, ok := parsed.(crypto.Signer)
	if !ok {
		return nil, errors.New("is not a signing key")
	}
	return signer, nil
}

func publicKeysEqual(a, b any) bool {
	switch a := a.(type) {
	case *ecdsa.PublicKey:
		b, ok := b.(*ecdsa.PublicKey)
		return ok && a.Equal(b)
	case *rsa.PublicKey:
		b, ok := b.(*rsa.PublicKey)
		return ok && a.Equal(b)
	case ed25519.PublicKey:
		b, ok := b.(ed25519.PublicKey)
		return ok && a.Equal(b)
	}
	return false
}

// pkiPath is one path spec.files or spec.pki writes, with the field that names it.
type pkiPath struct {
	field string
	path  string
}

func (v *validation) pki(p PKI, files []File, services []Service) {
	running := map[string]bool{}
	for _, s := range services {
		running[s.Name] = true
	}
	var taken []pkiPath
	for i, f := range files {
		taken = append(taken, pkiPath{fmt.Sprintf("spec.files[%d].path", i), f.Path})
	}
	claim := func(field, p string) {
		if !isCleanAbs(p) || !strings.HasPrefix(p, FilesRoot+"/") {
			v.fail(field, "must be a clean absolute path under %s, got %q", FilesRoot, p)
			return
		}
		if p == ReservedDir || strings.HasPrefix(p, ReservedDir+"/") {
			v.fail(field, "%s is init's own directory, got %q", ReservedDir, p)
			return
		}
		for _, t := range taken {
			if p == t.path || strings.HasPrefix(p, t.path+"/") || strings.HasPrefix(t.path, p+"/") {
				v.fail(field, "%q conflicts with %s, %q", p, t.field, t.path)
			}
		}
		taken = append(taken, pkiPath{field, p})
	}

	authorities := map[string]Authority{}
	for i, a := range p.Authorities {
		field := fmt.Sprintf("spec.pki.authorities[%d]", i)
		if !nameRE.MatchString(a.Name) {
			v.fail(field+".name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", a.Name)
		} else if _, dup := authorities[a.Name]; dup {
			v.fail(field+".name", "%q is used by an earlier authority", a.Name)
		}
		authorities[a.Name] = a
		cert, err := parseCertificate(a.Certificate)
		switch {
		case err != nil:
			v.fail(field+".certificate", "%v", err)
		case !cert.IsCA:
			v.fail(field+".certificate", "is not a certificate authority")
		}
		if a.Key != "" && err == nil {
			if _, err := a.Signer(); err != nil {
				v.fail(field+".key", "%v", err)
			}
		}
		claim(field+".certificatePath", a.CertificatePath)
		if a.KeyPath != "" {
			if a.Key == "" {
				v.fail(field+".keyPath", "needs the authority's key, which it does not carry")
			}
			claim(field+".keyPath", a.KeyPath)
		}
	}

	names := map[string]bool{}
	for i, c := range p.Certificates {
		field := fmt.Sprintf("spec.pki.certificates[%d]", i)
		if !nameRE.MatchString(c.Name) {
			v.fail(field+".name", "must be lowercase letters, digits and '-', at most 63 characters, got %q", c.Name)
		} else if names[c.Name] {
			v.fail(field+".name", "%q is used by an earlier certificate", c.Name)
		}
		names[c.Name] = true
		if a, ok := authorities[c.Authority]; !ok {
			v.fail(field+".authority", "names no authority in spec.pki.authorities, got %q", c.Authority)
		} else if a.Key == "" {
			v.fail(field+".authority", "authority %q carries no key and cannot sign", c.Authority)
		}
		claim(field+".certificatePath", c.CertificatePath)
		claim(field+".keyPath", c.KeyPath)
		if c.CommonName == "" {
			v.fail(field+".commonName", "must not be empty")
		}
		if c.Usage != Server && c.Usage != Client {
			v.fail(field+".usage", "must be %q or %q, got %q", Server, Client, c.Usage)
		}
		for j, ip := range c.IPAddresses {
			if _, err := netip.ParseAddr(ip); err != nil {
				v.fail(fmt.Sprintf("%s.ipAddresses[%d]", field, j), "must be an IP address, got %q", ip)
			}
		}
		if c.Usage == Server && len(c.DNSNames) == 0 && len(c.IPAddresses) == 0 && !c.NodeAddresses {
			v.fail(field, "a server certificate needs dnsNames, ipAddresses or nodeAddresses")
		}
		if c.Validity != "" {
			if d, err := time.ParseDuration(c.Validity); err != nil || d <= 0 {
				v.fail(field+".validity", "must be a positive duration such as 720h, got %q", c.Validity)
			}
		}
		for j, name := range c.Restart {
			if !running[name] {
				v.fail(fmt.Sprintf("%s.restart[%d]", field, j), "names no service in spec.services, got %q", name)
			}
		}
	}
}

// Authority returns the authority named name.
func (p PKI) Authority(name string) (Authority, bool) {
	for _, a := range p.Authorities {
		if a.Name == name {
			return a, true
		}
	}
	return Authority{}, false
}
