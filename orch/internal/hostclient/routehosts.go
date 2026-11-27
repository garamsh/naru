package hostclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/routes"
)

// RouteAPI is one host's API as the routing push uses it.
type RouteAPI interface {
	Host(ctx context.Context) (hostapi.Host, error)
	PutRoutes(ctx context.Context, r hostapi.RoutesRequest) (hostapi.RoutesAnswer, error)
}

// RouteHosts implements routes.Hosts over the admitted hosts' APIs.
type RouteHosts struct {
	inv  Inventory
	dial func(id, addr string) RouteAPI
}

var _ routes.Hosts = (*RouteHosts)(nil)

// NewRouteHosts returns the routing push's view of the hosts of inv.
func NewRouteHosts(inv Inventory, dial func(id, addr string) RouteAPI) *RouteHosts {
	return &RouteHosts{inv: inv, dial: dial}
}

// DialRouteAPI is DialNetAPI for the routing calls.
func DialRouteAPI(hc *http.Client, revocations *identity.RevocationList) func(id, addr string) RouteAPI {
	n := newNamed(hc, revocations)
	return func(id, addr string) RouteAPI {
		return hostapi.NewClient((&url.URL{Scheme: "https", Host: addr}).String(), withToken(n.client(id, addr)))
	}
}

// Proxies probes the admitted hosts' GET /v1/host. A host that holds the operator's grant and
// reports a proxy, healthy or not (one that is not healthy may still forward, and takes the table
// that stops it), is a proxy, with the max age it reports; one that reports a proxy without the
// grant is ungranted; one that answers otherwise runs no proxy the push serves.
func (h *RouteHosts) Proxies(ctx context.Context) (routes.Probe, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return routes.Probe{}, err
	}
	type seen struct {
		id      string
		proxy   *hostapi.ProxyStatus
		granted bool
	}
	var out routes.Probe
	for _, s := range probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) (seen, bool) {
		info, err := h.dial(a.ID, a.Address).Host(ctx)
		return seen{a.ID, info.Proxy, a.Proxy}, err == nil
	}) {
		switch {
		case s.proxy != nil && s.granted:
			out.Proxies = append(out.Proxies, routes.Proxy{ID: s.id, MaxAgeSeconds: s.proxy.RouteMaxAgeSeconds})
		case s.proxy != nil:
			out.Ungranted = append(out.Ungranted, s.id)
		default:
			out.NoProxy = append(out.NoProxy, s.id)
		}
	}
	return out, nil
}

// Admitted are the admitted hosts' IDs.
func (h *RouteHosts) Admitted(ctx context.Context) ([]string, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(admitted))
	for i, a := range admitted {
		out[i] = a.ID
	}
	return out, nil
}

// Push puts the table to the host, within ctx's deadline, and returns the revision its proxy has
// loaded.
func (h *RouteHosts) Push(ctx context.Context, hostID string, t hostapi.RoutesRequest) (int64, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return 0, err
	}
	for _, a := range admitted {
		if a.ID == hostID {
			ans, err := h.dial(a.ID, a.Address).PutRoutes(ctx, t)
			return ans.Loaded, err
		}
	}
	return 0, errNotAdmitted(hostID)
}
