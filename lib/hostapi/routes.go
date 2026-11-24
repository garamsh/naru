package hostapi

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"
)

// RouteProtocol is how a proxy carries a service to a Machine: "http" (HTTP/1.1, HTTP/2 and
// WebSocket, routed by each request's Host) or "tcp" (TLS from the client, routed by the SNI,
// plain TCP to the Machine).
type RouteProtocol string

// The protocols of a route.
const (
	RouteHTTP RouteProtocol = "http"
	RouteTCP  RouteProtocol = "tcp"
)

// Valid reports whether p is RouteHTTP or RouteTCP.
func (p RouteProtocol) Valid() bool { return p == RouteHTTP || p == RouteTCP }

// Route is one name a proxy answers: a Machine's stable name for one of its services, or an alias
// of one (ADR 0112 items 1 to 3). Machine is the Machine's id, Address its private address and Port
// the service's. ProxyProtocol is "v2" or empty; with v2 the proxy writes a PROXY protocol v2
// header with the client's address before the first byte to the Machine. A route that is not
// Running is answered 503 without a connection to the Machine. Certificate and Key are the PEM of
// an alias's own pair, for an alias outside the zone, and empty for every other name, which the
// zone's certificate covers.
type Route struct {
	Hostname      string        `json:"hostname"`
	Machine       string        `json:"machine"`
	Address       netip.Addr    `json:"address"`
	Port          int           `json:"port"`
	Protocol      RouteProtocol `json:"protocol"`
	ProxyProtocol string        `json:"proxyProtocol,omitempty"`
	Running       bool          `json:"running"`
	Certificate   string        `json:"certificate,omitempty"`
	Key           string        `json:"key,omitempty"`
}

// hostnameRE is a lowercase DNS name: labels of letters, digits and inner hyphens.
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// Check refuses a route a proxy cannot serve, and returns an alias's own pair parsed, nil for a
// name the zone's certificate covers. It is the one definition of a route a proxy takes: orch
// refuses an alias, and hostd a table, by it before either reaches a proxy, which drops a route
// that fails it.
//
// The address is a Machine's: IPv4, and not loopback, unspecified, link-local, multicast or the
// broadcast address, so that no table points a client at the proxy's own host.
func (r Route) Check() (*tls.Certificate, error) {
	switch {
	case len(r.Hostname) > 253 || !hostnameRE.MatchString(r.Hostname):
		return nil, errors.New("the hostname is not a lowercase DNS name")
	case r.Machine == "":
		return nil, errors.New("names no Machine")
	case !r.Address.Is4():
		return nil, errors.New("the address is not IPv4")
	case r.Address.IsLoopback(), r.Address.IsUnspecified(), r.Address.IsLinkLocalUnicast(), r.Address.IsMulticast(),
		r.Address == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return nil, fmt.Errorf("the address %s is not a Machine's", r.Address)
	case r.Port < 1 || r.Port > 65535:
		return nil, fmt.Errorf("the port %d is out of range", r.Port)
	case !r.Protocol.Valid():
		return nil, fmt.Errorf("the protocol %q is not http or tcp", r.Protocol)
	case r.ProxyProtocol != "" && r.ProxyProtocol != "v2":
		return nil, fmt.Errorf("the PROXY protocol %q is not v2", r.ProxyProtocol)
	}
	if r.Certificate == "" && r.Key == "" {
		return nil, nil
	}
	cert, err := tls.X509KeyPair([]byte(r.Certificate), []byte(r.Key))
	if err != nil {
		return nil, fmt.Errorf("the alias's certificate and key: %w", err)
	}
	if err := cert.Leaf.VerifyHostname(r.Hostname); err != nil {
		return nil, fmt.Errorf("the alias's certificate: %w", err)
	}
	return &cert, nil
}

// RoutesRequest is the routing table orch's leader puts to each proxy host: the revision of orch's
// store it was derived at, orch's time when it was sent, the mesh addresses of the hosts that run a
// proxy, and every route.
type RoutesRequest struct {
	Revision int64        `json:"revision"`
	SentAt   time.Time    `json:"sentAt"`
	Proxies  []netip.Addr `json:"proxies"`
	Routes   []Route      `json:"routes"`
}

// RoutesFile is what naru-hostd writes to wire.ProxyRoutesFile for naru-proxy: the table it took,
// the time it took it by the host's clock, from which the proxy counts the table's age, and the
// host's mesh address, which the proxy connects to Machines from. MeshAddress is absent on a host
// that has none.
type RoutesFile struct {
	RoutesRequest
	ReceivedAt  time.Time  `json:"receivedAt"`
	MeshAddress netip.Addr `json:"meshAddress,omitzero"`
}
