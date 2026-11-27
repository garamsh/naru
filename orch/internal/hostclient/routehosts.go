package hostclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

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

// Proxies are the admitted hosts whose GET /v1/host answers and reports a proxy, healthy or not
// (one that is not healthy may still forward, and takes the table that stops it), each with the
// max age it reports for its proxy.
func (h *RouteHosts) Proxies(ctx context.Context) ([]routes.Proxy, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	return probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) (routes.Proxy, bool) {
		info, err := h.dial(a.ID, a.Address).Host(ctx)
		if err != nil || info.Proxy == nil {
			return routes.Proxy{}, false
		}
		return routes.Proxy{ID: a.ID, MaxAge: time.Duration(info.Proxy.RouteMaxAgeSeconds) * time.Second}, true
	}), nil
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
