package hostapi

import (
	"net/netip"
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
