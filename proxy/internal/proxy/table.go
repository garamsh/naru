package proxy

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
)

// table is a routes file the proxy has parsed and forwards on.
type table struct {
	revision int64
	// receivedAt is when hostd took the table, by the host's clock; the table's age counts from it.
	receivedAt time.Time
	// source is the host's mesh address, which connections to Machines leave from. It is the zero
	// Addr on a host that has none, and the kernel then chooses.
	source netip.Addr
	routes map[string]route
}

// route is one name the proxy answers, with an alias's own certificate parsed. cert is nil for a
// name the zone's certificate covers.
type route struct {
	hostapi.Route
	cert *tls.Certificate
}

// hostnameRE is a lowercase DNS name: labels of letters, digits and inner hyphens.
var hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

// parseTable parses a routes file. Any entry that does not parse fails the whole file, so the
// proxy keeps the table it had rather than forward on part of one.
func parseTable(raw []byte) (*table, error) {
	var f hostapi.RoutesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("the routes file: %w", err)
	}
	if f.ReceivedAt.IsZero() {
		return nil, errors.New("the routes file has no receivedAt")
	}
	if f.MeshAddress.IsValid() && !f.MeshAddress.Is4() {
		return nil, fmt.Errorf("the mesh address %s is not IPv4", f.MeshAddress)
	}
	t := &table{revision: f.Revision, receivedAt: f.ReceivedAt, source: f.MeshAddress, routes: make(map[string]route, len(f.Routes))}
	for _, r := range f.Routes {
		parsed, err := parseRoute(r)
		if err != nil {
			return nil, fmt.Errorf("route %q: %w", r.Hostname, err)
		}
		if _, dup := t.routes[r.Hostname]; dup {
			return nil, fmt.Errorf("route %q is named twice", r.Hostname)
		}
		t.routes[r.Hostname] = parsed
	}
	return t, nil
}

func parseRoute(r hostapi.Route) (route, error) {
	switch {
	case len(r.Hostname) > 253 || !hostnameRE.MatchString(r.Hostname):
		return route{}, errors.New("the hostname is not a lowercase DNS name")
	case r.Machine == "":
		return route{}, errors.New("names no Machine")
	case !r.Address.Is4():
		return route{}, errors.New("the address is not IPv4")
	case r.Port < 1 || r.Port > 65535:
		return route{}, fmt.Errorf("the port %d is out of range", r.Port)
	case !r.Protocol.Valid():
		return route{}, fmt.Errorf("the protocol %q is not http or tcp", r.Protocol)
	case r.ProxyProtocol != "" && r.ProxyProtocol != "v2":
		return route{}, fmt.Errorf("the PROXY protocol %q is not v2", r.ProxyProtocol)
	}
	if r.Certificate == "" && r.Key == "" {
		return route{Route: r}, nil
	}
	cert, err := tls.X509KeyPair([]byte(r.Certificate), []byte(r.Key))
	if err != nil {
		return route{}, fmt.Errorf("the alias's certificate and key: %w", err)
	}
	if err := cert.Leaf.VerifyHostname(r.Hostname); err != nil {
		return route{}, fmt.Errorf("the alias's certificate: %w", err)
	}
	return route{Route: r, cert: &cert}, nil
}

// lookup returns the route of a name as a client sent it, in any case and with or without a
// trailing dot. A nil table has no routes.
func (t *table) lookup(name string) (route, bool) {
	if t == nil {
		return route{}, false
	}
	r, ok := t.routes[strings.TrimSuffix(strings.ToLower(name), ".")]
	return r, ok
}

// expired reports whether the table is older than maxAge at now, after which the proxy forwards
// on it no more (ADR 0112 item 5). A nil table has expired.
func (t *table) expired(now time.Time, maxAge time.Duration) bool {
	return t == nil || now.Sub(t.receivedAt) > maxAge
}
