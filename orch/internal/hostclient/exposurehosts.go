package hostclient

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/groups"
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
	_ exposures.Hosts    = (*ExposureHosts)(nil)
	_ exposures.Members  = (*ExposureHosts)(nil)
	_ exposures.Machines = (*ExposureHosts)(nil)
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

// probeAll runs f on every admitted host at once, each with its own probeTimeout, so one host that
// does not answer holds back no other and the whole takes no longer than one probe.
func probeAll[T any](ctx context.Context, admitted []orchapi.Host, f func(ctx context.Context, a orchapi.Host) (T, bool)) []T {
	got := make([]T, len(admitted))
	ok := make([]bool, len(admitted))
	var wg sync.WaitGroup
	for i, a := range admitted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			got[i], ok[i] = f(cctx, a)
		}()
	}
	wg.Wait()
	var out []T
	for i := range got {
		if ok[i] {
			out = append(out, got[i])
		}
	}
	return out
}

// live reports whether the microVM is running: a stopped or failed one answers nothing, so it is
// no backend and its host no place for an address on its network.
func live(m hostapi.MicroVM) bool { return machineState(m.State) == groups.MachineRunning }

// List returns the admitted hosts with their LAN interface and the networks they run a live
// microVM on; a host that did not answer is returned Silent, so the reconciler can tell a slow
// holder from a gone one.
func (h *ExposureHosts) List(ctx context.Context) ([]exposures.Host, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	return probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) (exposures.Host, bool) {
		c := h.dial(a.ID, a.Address)
		info, err := c.Host(ctx)
		if err != nil {
			return exposures.Host{ID: a.ID, Silent: true}, true
		}
		vms, err := c.MicroVMs(ctx)
		if err != nil {
			return exposures.Host{ID: a.ID, Silent: true}, true
		}
		seen := map[string]bool{}
		var nets []string
		var members map[string][]string
		for _, m := range vms {
			if live(m) && m.Network != "" {
				if !seen[m.Network] {
					seen[m.Network] = true
					nets = append(nets, m.Network)
				}
				if members == nil {
					members = map[string][]string{}
				}
				members[m.Network] = append(members[m.Network], m.Addresses...)
			}
		}
		sort.Strings(nets)
		for net := range members {
			sort.Strings(members[net])
		}
		return exposures.Host{ID: a.ID, LAN: info.LANInterface, Networks: nets, Members: members}, true
	}), nil
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
		ports[i] = hostapi.ExposurePort{Protocol: p.Protocol, Port: p.Port, BackendPort: p.BackendPort}
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

// Addresses returns the addresses of the running microVMs of the network on every host that
// answers.
func (h *ExposureHosts) Addresses(ctx context.Context, network string) ([]string, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, addrs := range probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) ([]string, bool) {
		vms, err := h.dial(a.ID, a.Address).MicroVMs(ctx)
		if err != nil {
			return nil, false
		}
		var addrs []string
		for _, m := range vms {
			if live(m) && m.Network == network {
				addrs = append(addrs, m.Addresses...)
			}
		}
		return addrs, true
	}) {
		out = append(out, addrs...)
	}
	return out, nil
}

// Machines returns the microVMs of the network on every host that answers.
func (h *ExposureHosts) Machines(ctx context.Context, network string) ([]exposures.Machine, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	var out []exposures.Machine
	for _, ms := range probeAll(ctx, admitted, func(ctx context.Context, a orchapi.Host) ([]exposures.Machine, bool) {
		vms, err := h.dial(a.ID, a.Address).MicroVMs(ctx)
		if err != nil {
			return nil, false
		}
		var ms []exposures.Machine
		for _, m := range vms {
			if m.Network != network {
				continue
			}
			x := exposures.Machine{Name: m.Name, Host: a.ID, Running: live(m)}
			if len(m.Addresses) > 0 {
				x.Address = m.Addresses[0]
			}
			ms = append(ms, x)
		}
		return ms, true
	}) {
		out = append(out, ms...)
	}
	return out, nil
}
