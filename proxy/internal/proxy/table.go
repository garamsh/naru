package proxy

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
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

// dropped is a route of the file the proxy does not serve, and why.
type dropped struct {
	hostname string
	err      error
}

// parseTable parses a routes file. A file that does not parse, or has no receivedAt or a mesh
// address that is not IPv4, fails whole, and the proxy keeps the table it had. A route that fails
// hostapi.Route.Check, or a name given twice, is dropped alone and returned with why: dropping a
// name never sends a client to another address, so one tenant's bad alias does not stop the
// proxy's routing of every other name.
func parseTable(raw []byte) (*table, []dropped, error) {
	var f hostapi.RoutesFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, nil, fmt.Errorf("the routes file: %w", err)
	}
	if f.ReceivedAt.IsZero() {
		return nil, nil, errors.New("the routes file has no receivedAt")
	}
	if f.MeshAddress.IsValid() && !f.MeshAddress.Is4() {
		return nil, nil, fmt.Errorf("the mesh address %s is not IPv4", f.MeshAddress)
	}
	t := &table{revision: f.Revision, receivedAt: f.ReceivedAt, source: f.MeshAddress, routes: make(map[string]route, len(f.Routes))}
	var out []dropped
	twice := map[string]bool{}
	for _, r := range f.Routes {
		cert, err := r.Check()
		if err != nil {
			out = append(out, dropped{r.Hostname, err})
			continue
		}
		if _, dup := t.routes[r.Hostname]; dup || twice[r.Hostname] {
			delete(t.routes, r.Hostname)
			twice[r.Hostname] = true
			out = append(out, dropped{r.Hostname, errors.New("the name is given twice")})
			continue
		}
		t.routes[r.Hostname] = route{Route: r, cert: cert}
	}
	return t, out, nil
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
