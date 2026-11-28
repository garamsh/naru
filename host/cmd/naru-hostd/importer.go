package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"strconv"

	"github.com/garamsh/naru/host/internal/api"
	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/host/internal/network"
)

// newImporter is how this host fetches another host's export: with the pair it reports to orch
// with, read at each fetch so a rotation is taken, to the peer's mesh address at the port this
// host's API listens on, every host listening on the same one, accepting a serving certificate
// under the client CA pool, which holds the authorities every host's serving pair chains to, as
// it stands at each fetch.
func newImporter(hostKey string, pool *api.ClientPool, listen string, peers func() []network.Peer) (*api.Importer, error) {
	_, p, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %w", listen, err)
	}
	port, err := strconv.ParseUint(p, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("--listen %q names no port", listen)
	}
	return &api.Importer{
		Pair: func() (*tls.Certificate, error) {
			pair, err := tls.LoadX509KeyPair(enroll.CertPathFor(hostKey), hostKey)
			return &pair, err
		},
		Roots: func() *x509.CertPool {
			roots, _ := pool.Pool()
			return roots
		},
		Address: func(id string) (string, error) {
			for _, peer := range peers() {
				if peer.Name == id && peer.Mesh.IsValid() {
					return netip.AddrPortFrom(peer.Mesh, uint16(port)).String(), nil
				}
			}
			return "", fmt.Errorf("the host %s is not a peer with a mesh address", id)
		},
	}, nil
}
