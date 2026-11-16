package client

import (
	"errors"
	"fmt"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/layout/pki"
	"github.com/garamsh/naru/lib/wire"
)

// Roles of a host API client certificate.
const (
	Provisioner = "provisioner"
	HostReader  = "reader"
)

var hostOrganizations = map[string]string{Provisioner: "naru:host-provisioner", HostReader: "naru:host-reader"}

// IssueHost returns a client file for host APIs: its certificate, issued by the host authority with
// CN cn, carries the Organization of role. Each endpoint is an IP address, with a port or without.
func IssueHost(authority *pki.CA, role, cn string, endpoints []string) (File, error) {
	org, ok := hostOrganizations[role]
	if !ok {
		return File{}, fmt.Errorf("the role %q is not %s or %s", role, Provisioner, HostReader)
	}
	if len(endpoints) == 0 {
		return File{}, errors.New("a client file needs at least one endpoint")
	}
	f := File{CA: string(authority.CertPEM)}
	for _, e := range endpoints {
		hostPort, err := AddressPort(e, wire.HostAPIPort)
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

// DialHost returns the host API's client for the host at node, an IP address with or without a
// port; when node is empty it is the file's first endpoint. It speaks TLS 1.3 only, trusts the
// file's authority and presents the file's pair. Nothing is sent until a call. A host that joined
// from the install seed has a certificate that names only its host ID, so serverName is that name
// and the certificate is verified against it instead of the address; empty verifies the address.
func DialHost(f File, node, serverName string) (*hostapi.Client, error) {
	hc, addr, err := mtls(f, node, wire.HostAPIPort, serverName)
	if err != nil {
		return nil, err
	}
	return hostapi.NewClient("https://"+addr, hc), nil
}
