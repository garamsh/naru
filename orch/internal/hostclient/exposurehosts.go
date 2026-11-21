package hostclient

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/orch/internal/exposures"
)

// ExposureAPI is one host's API as the exposure reconciler and the backend check use it.
type ExposureAPI interface {
	Host(ctx context.Context) (hostapi.Host, error)
	MicroVMs(ctx context.Context) ([]hostapi.MicroVM, error)
	PutExposure(ctx context.Context, address string, r hostapi.ExposureRequest) (hostapi.Exposure, error)
	Exposures(ctx context.Context) ([]hostapi.Exposure, error)
	DeleteExposure(ctx context.Context, address string) error
}

// ExposureHosts implements exposures.Hosts and exposures.Members over the admitted hosts' APIs.
type ExposureHosts struct {
	inv  Inventory
	dial func(id, addr string) ExposureAPI
}

var (
	_ exposures.Hosts   = (*ExposureHosts)(nil)
	_ exposures.Members = (*ExposureHosts)(nil)
)

// NewExposureHosts returns the exposures' view of the hosts of inv.
func NewExposureHosts(inv Inventory, dial func(id, addr string) ExposureAPI) *ExposureHosts {
	return &ExposureHosts{inv: inv, dial: dial}
}

// DialExposureAPI is DialNetAPI for the exposure calls.
func DialExposureAPI(hc *http.Client, revocations *identity.RevocationList) func(id, addr string) ExposureAPI {
	n := newNamed(hc, revocations)
	return func(id, addr string) ExposureAPI {
		return hostapi.NewClient((&url.URL{Scheme: "https", Host: addr}).String(), withToken(n.client(id, addr)))
	}
}

func (h *ExposureHosts) api(ctx context.Context, id string, timeout time.Duration) (ExposureAPI, context.Context, context.CancelFunc, error) {
	hosts, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, a := range hosts {
		if a.ID == id {
			ctx, cancel := context.WithTimeout(ctx, timeout)
			return h.dial(a.ID, a.Address), ctx, cancel, nil
		}
	}
	return nil, nil, nil, errNotAdmitted(id)
}

type errNotAdmitted string

func (e errNotAdmitted) Error() string { return "host " + string(e) + " is not admitted" }

// List returns the admitted hosts that answered, with their LAN interface and the networks they
// run a microVM on.
func (h *ExposureHosts) List(ctx context.Context) ([]exposures.Host, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []exposures.Host
	for _, a := range admitted {
		c := h.dial(a.ID, a.Address)
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		info, err := c.Host(cctx)
		var vms []hostapi.MicroVM
		if err == nil {
			vms, err = c.MicroVMs(cctx)
		}
		cancel()
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		var nets []string
		for _, m := range vms {
			if m.Network != "" && !seen[m.Network] {
				seen[m.Network] = true
				nets = append(nets, m.Network)
			}
		}
		sort.Strings(nets)
		out = append(out, exposures.Host{ID: a.ID, LAN: info.LANInterface, Networks: nets})
	}
	return out, nil
}

// Put takes or renews the lease on the host.
func (h *ExposureHosts) Put(ctx context.Context, hostID, address string, l exposures.Lease) error {
	c, ctx, cancel, err := h.api(ctx, hostID, callTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	ports := make([]hostapi.ExposurePort, len(l.Ports))
	for i, p := range l.Ports {
		ports[i] = hostapi.ExposurePort{Protocol: p.Protocol, Port: p.Port, NodePort: p.NodePort}
	}
	_, err = c.PutExposure(ctx, address, hostapi.ExposureRequest{Network: l.Network, Ports: ports, Backends: l.Backends, ProxyProtocol: l.ProxyProtocol, LeaseSeconds: l.Seconds})
	return err
}

// Held returns the addresses the host holds a lease on.
func (h *ExposureHosts) Held(ctx context.Context, hostID string) ([]string, error) {
	c, ctx, cancel, err := h.api(ctx, hostID, callTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	es, err := c.Exposures(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Address
	}
	return out, nil
}

// Delete removes the address from the host.
func (h *ExposureHosts) Delete(ctx context.Context, hostID, address string) error {
	c, ctx, cancel, err := h.api(ctx, hostID, callTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	return c.DeleteExposure(ctx, address)
}

// Addresses returns the addresses of the microVMs of the network on every host that answers.
func (h *ExposureHosts) Addresses(ctx context.Context, network string) ([]string, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range admitted {
		cctx, cancel := context.WithTimeout(ctx, probeTimeout)
		vms, err := h.dial(a.ID, a.Address).MicroVMs(cctx)
		cancel()
		if err != nil {
			continue
		}
		for _, m := range vms {
			if m.Network == network {
				out = append(out, m.Addresses...)
			}
		}
	}
	return out, nil
}
