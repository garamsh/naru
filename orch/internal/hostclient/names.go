package hostclient

import (
	"crypto/x509"
	"net"
	"net/http"
	"sync"

	"github.com/garamsh/naru/lib/identity"
)

// named gives each host an HTTP client that accepts the host's certificate when it chains to the
// authority of the base client and names either the address orch dials or the host's ID. A host
// that joined from a seed has the ID in its certificate and no address, because a DHCP lease can
// change within the year the certificate lives; a control host has its address. A certificate that
// chains but names another host is refused: every host certificate carries the server EKU, so the
// chain alone would let one host answer for another.
//
// Every certificate is also checked against revocations, when it is not nil, so a revoked host is
// not called. A base client that skips verification, or whose transport is not an *http.Transport,
// is returned as it is: that is the caller's own choice of what to trust, and only tests make it.
type named struct {
	base        *http.Client
	revocations *identity.RevocationList
	mu          sync.Mutex
	by          map[string]namedClient
}

type namedClient struct {
	addr   string
	client *http.Client
}

// newNamed takes a copy of base without its Timeout: a call's deadline comes from its context
// alone, because the client's Timeout would end a Create that the engine allows minutes at 30 s,
// and the engine would then delete the orphan and create the group's member again.
func newNamed(base *http.Client, revocations *identity.RevocationList) *named {
	b := *base
	b.Timeout = 0
	return &named{base: &b, revocations: revocations, by: map[string]namedClient{}}
}

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
	conf.InsecureSkipVerify = true
	dialled, _, err := net.SplitHostPort(addr)
	if err != nil {
		dialled = addr
	}
	conf.VerifyConnection = identity.PeerCheck{Roots: roots, Usage: x509.ExtKeyUsageServerAuth, Names: []string{dialled, id}, Revocations: n.revocations}.VerifyConnection
	c := &http.Client{Transport: clone}
	n.by[id] = namedClient{addr: addr, client: c}
	return c
}
