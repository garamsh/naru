// Package client is the Node API's client: the operator's client file and the calls of every
// endpoint init serves (docs/architecture/adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).
package client

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"

	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/pki"
)

// DefaultPort is the port init's Node API listens on unless spec.api.listen says another.
const DefaultPort = "50000"

// Roles a client certificate can carry; the server takes the role from the Organization.
const (
	Admin  = "admin"
	Reader = "reader"
)

var organizations = map[string]string{Admin: "naru:admin", Reader: "naru:reader"}

// File is the operator's client file: where to reach Nodes, the Node API authority's certificate
// and the client's own pair, all PEM.
type File struct {
	Endpoints   []string `yaml:"endpoints"`
	CA          string   `yaml:"ca"`
	Certificate string   `yaml:"certificate"`
	Key         string   `yaml:"key"`
}

// Issue returns a client file whose certificate, issued by authority with CN cn, carries the
// Organization of role. Each endpoint is an IP address, with a port or without.
func Issue(authority *pki.CA, role, cn string, endpoints []string) (File, error) {
	org, ok := organizations[role]
	if !ok {
		return File{}, fmt.Errorf("the role %q is not %s or %s", role, Admin, Reader)
	}
	if len(endpoints) == 0 {
		return File{}, errors.New("a client file needs at least one endpoint")
	}
	f := File{CA: string(authority.CertPEM)}
	for _, e := range endpoints {
		hostPort, err := Address(e)
		if err != nil {
			return File{}, err
		}
		f.Endpoints = append(f.Endpoints, hostPort)
	}
	pair, err := authority.Client(cn, org)
	if err != nil {
		return File{}, err
	}
	f.Certificate, f.Key = string(pair.CertPEM), string(pair.KeyPEM)
	return f, nil
}

// Address returns a Node's address as host:port: an IP address, with DefaultPort when it has no
// port. The Node's serving certificate names IP addresses, so a name is refused.
func Address(s string) (string, error) { return AddressPort(s, DefaultPort) }

// AddressPort is Address with port in place of DefaultPort.
func AddressPort(s, port string) (string, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.String(), nil
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return net.JoinHostPort(a.String(), port), nil
	}
	return "", fmt.Errorf("%q is not an IP address, with or without a port", s)
}

// Write writes the file to path, mode 0600: it carries a key.
func (f File) Write(path string) error {
	b, err := yaml.Marshal(f)
	if err != nil {
		return err
	}
	return pki.WriteFile(path, b, 0o600)
}

// LoadFile reads a client file. A malformed file is reported without its text: the file carries
// a key, and a parser's message can quote the value it refused.
func LoadFile(path string) (File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("%s is not a client file: endpoints, ca, certificate and key are its fields", path)
	}
	if len(f.Endpoints) == 0 || f.CA == "" || f.Certificate == "" || f.Key == "" {
		return File{}, fmt.Errorf("%s is not a client file: endpoints, ca, certificate and key are all required", path)
	}
	return f, nil
}
