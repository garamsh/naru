package hostclient

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// named gives each host an HTTP client that accepts the host's certificate when it chains to the
// authority of the base client and names either the address orch dials or the host's ID. A host
// that joined from a seed has the ID in its certificate and no address, because a DHCP lease can
// change within the year the certificate lives; a control host has its address. A certificate that
// chains but names another host is refused: every host certificate carries the server EKU, so the
// chain alone would let one host answer for another.
//
// A base client that skips verification, or whose transport is not an *http.Transport, is
// returned as it is: that is the caller's own choice of what to trust.
type named struct {
	base *http.Client
	mu   sync.Mutex
	by   map[string]namedClient
}

type namedClient struct {
	addr   string
	client *http.Client
}

func newNamed(base *http.Client) *named { return &named{base: base, by: map[string]namedClient{}} }

// client is the client for the host id at addr.
func (n *named) client(id, addr string) *http.Client {
	tr, ok := n.base.Transport.(*http.Transport)
	if !ok || tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		return n.base
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if c, ok := n.by[id]; ok {
		if c.addr == addr {
			return c.client
		}
		c.client.CloseIdleConnections()
	}
	clone := tr.Clone()
	conf := clone.TLSClientConfig
	roots := conf.RootCAs
	conf.InsecureSkipVerify = true //nolint:gosec // VerifyConnection below verifies the chain and the names
	conf.VerifyConnection = func(cs tls.ConnectionState) error { return verifyHost(cs, roots, id, addr) }
	c := &http.Client{Transport: clone, Timeout: n.base.Timeout}
	n.by[id] = namedClient{addr: addr, client: c}
	return c
}

// verifyHost checks that the chain the host presented verifies to roots for server use and that
// its leaf names the address dialled or the host id.
func verifyHost(cs tls.ConnectionState, roots *x509.CertPool, id, addr string) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("the host presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: inter, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, CurrentTime: time.Now()}); err != nil {
		return fmt.Errorf("the host's certificate: %w", err)
	}
	dialled, _, err := net.SplitHostPort(addr)
	if err != nil {
		dialled = addr
	}
	if leaf.VerifyHostname(dialled) == nil || leaf.VerifyHostname(id) == nil {
		return nil
	}
	return fmt.Errorf("the host's certificate names neither %s, the address dialled, nor %s, the host orch means to reach", dialled, id)
}
