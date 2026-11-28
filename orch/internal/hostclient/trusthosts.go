package hostclient

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/trust"
)

var _ trust.Hosts = (*RouteHosts)(nil)

// Trust probes the admitted hosts' GET /v1/host for when each host's pool of authorities stops
// trusting one it holds. A host that does not answer, or reports no expiry, is left out.
func (h *RouteHosts) Trust(ctx context.Context) ([]trust.Host, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	return probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) (trust.Host, bool) {
		info, err := h.dial(a.ID, a.Address).Host(ctx)
		if err != nil || info.TrustExpiresAt == nil {
			return trust.Host{}, false
		}
		return trust.Host{ID: a.ID, ExpiresAt: *info.TrustExpiresAt, Authority: info.TrustExpiresAuthority}, true
	}), nil
}
