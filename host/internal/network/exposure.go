package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"time"
)

// Errors the exposures answer; the API maps them to statuses.
var (
	ErrNoLAN    = errors.New("the host has no LAN interface")
	ErrReserved = errors.New("the address belongs to a network, a member or the management plane")
	ErrBackend  = errors.New("a backend is not a member of the exposure's network")
)

// The protocols of an exposed port.
const (
	TCP = "tcp"
	UDP = "udp"
)

// ExposurePort is a port of an exposure: the port on the address and the NodePort it goes to.
type ExposurePort struct {
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
	NodePort int    `json:"nodePort"`
}

// Exposure is a lease on a LAN address: the host owns the address and forwards its ports to the
// backends, the Nodes of Network, until Deadline (ADR 0085 items 5 and 6). With Proxy the TCP ports
// go through hostd's proxy, which writes a PROXY protocol v2 header, and are not forwarded by DNAT.
type Exposure struct {
	Address  netip.Addr     `json:"address"`
	Network  string         `json:"network"`
	Ports    []ExposurePort `json:"ports"`
	Backends []netip.Addr   `json:"backends"`
	Proxy    bool           `json:"proxy,omitempty"`
	Deadline time.Time      `json:"deadline"`
}

// forwarded returns the ports DNAT carries: all of them, or the UDP ones of an exposure with Proxy.
func (e Exposure) forwarded() []ExposurePort {
	var out []ExposurePort
	for _, p := range e.Ports {
		if !e.Proxy || p.Protocol != TCP {
			out = append(out, p)
		}
	}
	return out
}

// proxied returns the TCP ports of an exposure with Proxy.
func (e Exposure) proxied() []ExposurePort {
	var out []ExposurePort
	for _, p := range e.Ports {
		if e.Proxy && p.Protocol == TCP {
			out = append(out, p)
		}
	}
	return out
}

func sortedExposures(in []Exposure) []Exposure {
	out := append([]Exposure(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Address.Less(out[j].Address) })
	return out
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Exposures returns the leases the host holds, ordered by address.
func (m *Manager) Exposures() []Exposure {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedExposures(m.c.Exposures)
}

// PutExposure takes the lease on e.Address or renews it, and applies. The address must not be one
// the host's networks, their members or the management plane hold, since the host would answer for
// it on the LAN, and every backend must be a member of the exposure's network, since the host
// would forward LAN traffic to it. A renewal that changes only the deadline changes no rule.
func (m *Manager) PutExposure(ctx context.Context, e Exposure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Base.LAN == "" {
		return ErrNoLAN
	}
	if err := m.checkExposure(e); err != nil {
		return err
	}
	next := m.c
	next.Exposures = []Exposure{e}
	for _, o := range m.c.Exposures {
		if o.Address != e.Address {
			next.Exposures = append(next.Exposures, o)
		}
	}
	return m.apply(ctx, next)
}

// checkExposure refuses an exposure whose address or backends the host must not serve.
func (m *Manager) checkExposure(e Exposure) error {
	var n *Network
	for i := range m.c.Networks {
		if m.c.Networks[i].Name == e.Network {
			n = &m.c.Networks[i]
		}
	}
	if n == nil {
		return ErrUnknownNetwork
	}
	if m.Base.Management.IsValid() && m.Base.Management.Contains(e.Address) {
		return ErrReserved
	}
	for _, o := range m.c.Networks {
		if o.Prefix.Contains(e.Address) || slices.ContainsFunc(o.Remote, func(p netip.Prefix) bool { return p.Contains(e.Address) }) {
			return ErrReserved
		}
	}
	for _, x := range m.c.Members {
		if x.Address == e.Address || (x.Routed.IsValid() && x.Routed.Contains(e.Address)) {
			return ErrReserved
		}
	}
	for _, b := range e.Backends {
		if !m.isMember(*n, b) {
			return fmt.Errorf("%w: %s", ErrBackend, b)
		}
	}
	return nil
}

// isMember reports whether a is an address of the network's membership: inside its prefix, one of
// its remote members, or the address of a member attached here.
func (m *Manager) isMember(n Network, a netip.Addr) bool {
	if n.Prefix.Contains(a) || slices.ContainsFunc(n.Remote, func(p netip.Prefix) bool { return p.Contains(a) }) {
		return true
	}
	return slices.ContainsFunc(m.c.Members, func(x Member) bool { return x.Network == n.Name && x.Address == a })
}

// DeleteExposure drops the lease on a, and applies. An absent one is not an error.
func (m *Manager) DeleteExposure(ctx context.Context, a netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Exposures = nil
	for _, o := range m.c.Exposures {
		if o.Address != a {
			next.Exposures = append(next.Exposures, o)
		}
	}
	return m.apply(ctx, next)
}

// ExpireExposures drops the leases whose deadline has passed, and applies. It returns the addresses
// it dropped.
func (m *Manager) ExpireExposures(ctx context.Context) ([]netip.Addr, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Exposures = nil
	var gone []netip.Addr
	for _, o := range m.c.Exposures {
		if m.expired(o) {
			gone = append(gone, o.Address)
		} else {
			next.Exposures = append(next.Exposures, o)
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	return gone, m.apply(ctx, next)
}

func (m *Manager) expired(e Exposure) bool { return !m.now().Before(e.Deadline) }
