package network

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sort"
	"time"

	"github.com/garamsh/naru/lib/wire"
)

// Errors the exposures answer; the API maps them to statuses.
var (
	ErrNoLAN    = errors.New("the host has no LAN interface")
	ErrReserved = errors.New("the address belongs to a network or a member")
	ErrBackend  = errors.New("a backend is not a member of the exposure's network")
	// ErrHostAddress is an address the host cannot take for a lease because it is the host's own,
	// the LAN's network or broadcast address, or its gateway: announcing it would take it over.
	ErrHostAddress = errors.New("the address belongs to the host or to the LAN")
	// ErrOutsideLAN is an address not inside any prefix of the LAN interface: a layer-2 announcement
	// cannot reach beyond the interface's own subnet.
	ErrOutsideLAN = errors.New("the address is not inside any prefix of the LAN interface")
	// ErrTaken is an address answered by an ARP probe: another host on the LAN already holds it.
	ErrTaken = errors.New("the address is held by another host on the LAN")
	// ErrInvalid is an exposure that is malformed: a port or a BackendPort outside
	// 1 to 65535, a repeated port, no backend, or a UDP port with the PROXY protocol.
	ErrInvalid = errors.New("the exposure is invalid")
	// ErrNoProxy is a proxy exposure on a host that runs no naru-proxy.
	ErrNoProxy = errors.New("the host runs no naru-proxy")
)

// ProxyPorts are the host's naru-proxy's listeners a proxy exposure reaches: TLS the one its 443
// is redirected to, and Redirect the one its 80 is, which answers with a redirect to https.
type ProxyPorts struct {
	TLS, Redirect uint16
}

// Runs reports whether the host runs a proxy.
func (p ProxyPorts) Runs() bool { return p.TLS != 0 }

// target is the proxy's port an exposure's port is redirected to: 443 to TLS, 80 to Redirect.
func (p ProxyPorts) target(port int) uint16 {
	if port == 80 {
		return p.Redirect
	}
	return p.TLS
}

// ExposurePort is a port of an exposure: the port on the address and the BackendPort it goes to
// on the backends. Which ports a caller may name is orch's policy (ADR 0102); the host forwards what its lease says.
type ExposurePort struct {
	Protocol    wire.Protocol `json:"protocol"`
	Port        int           `json:"port"`
	BackendPort int           `json:"backendPort"`
}

// Exposure is a lease on a LAN address: the host owns the address and forwards its ports to the
// backends, the Nodes of Network, until Deadline (ADR 0085 items 5 and 6). With Proxy the TCP ports
// go through hostd's proxy, which writes a PROXY protocol v2 header, and are not forwarded by DNAT.
// With ToProxy it is a proxy exposure's (ADR 0112 item 8): no network and no backends, and its
// ports, TCP 443 and 80, are redirected to the host's own naru-proxy.
type Exposure struct {
	Address  netip.Addr     `json:"address"`
	Network  string         `json:"network"`
	Ports    []ExposurePort `json:"ports"`
	Backends []netip.Addr   `json:"backends"`
	Proxy    bool           `json:"proxy,omitempty"`
	ToProxy  bool           `json:"toProxy,omitempty"`
	Deadline time.Time      `json:"deadline"`
}

// forwarded returns the ports DNAT carries: all of them, or the UDP ones of an exposure with Proxy,
// and none of a proxy exposure's.
// served is the TCP ports a flow to the exposure's address reaches the host on: each port the
// host's PROXY protocol listener takes, or, for a proxy exposure, each port of the proxy its ports
// are redirected to.
func (e Exposure) served(proxy ProxyPorts) []uint16 {
	var out []uint16
	for _, p := range e.Ports {
		switch {
		case e.ToProxy:
			out = append(out, proxy.target(p.Port))
		case e.Proxy && p.Protocol == wire.ProtocolTCP:
			out = append(out, uint16(p.Port))
		}
	}
	return out
}

func (e Exposure) forwarded() []ExposurePort {
	if e.ToProxy {
		return nil
	}
	var out []ExposurePort
	for _, p := range e.Ports {
		if !e.Proxy || p.Protocol != wire.ProtocolTCP {
			out = append(out, p)
		}
	}
	return out
}

// proxied returns the TCP ports of an exposure with Proxy.
func (e Exposure) proxied() []ExposurePort {
	var out []ExposurePort
	for _, p := range e.Ports {
		if e.Proxy && p.Protocol == wire.ProtocolTCP {
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

// PutExposure takes the lease on e.Address or renews it, and applies. The address must not belong
// to the host's networks or their members, since the host would answer for it on the LAN, and every
// backend must be a member of the exposure's network on this host, since the host would forward LAN
// traffic to it. A renewal that changes only the deadline changes no rule.
func (m *Manager) PutExposure(ctx context.Context, e Exposure) error {
	m.mu.Lock()
	defer m.mu.Unlock()
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

// Validate refuses an exposure that is malformed, whatever the host holds.
func (e Exposure) Validate() error {
	if !e.Address.Is4() {
		return fmt.Errorf("%w: the address is not IPv4", ErrInvalid)
	}
	if e.ToProxy {
		return e.validateToProxy()
	}
	if len(e.Ports) == 0 || len(e.Backends) == 0 {
		return fmt.Errorf("%w: it needs a port and a backend", ErrInvalid)
	}
	seen := map[ExposurePort]bool{}
	for _, p := range e.Ports {
		key := ExposurePort{Protocol: p.Protocol, Port: p.Port}
		switch {
		case !p.Protocol.Valid():
			return fmt.Errorf("%w: protocol %q", ErrInvalid, p.Protocol)
		case p.Port < 1 || p.Port > 65535:
			return fmt.Errorf("%w: port %d", ErrInvalid, p.Port)
		case p.BackendPort < 1 || p.BackendPort > 65535:
			return fmt.Errorf("%w: backendPort %d", ErrInvalid, p.BackendPort)
		case seen[key]:
			return fmt.Errorf("%w: %s port %d twice", ErrInvalid, p.Protocol, p.Port)
		case e.Proxy && p.Protocol == wire.ProtocolUDP:
			return fmt.Errorf("%w: a UDP port cannot carry the PROXY protocol", ErrInvalid)
		}
		seen[key] = true
	}
	for _, b := range e.Backends {
		if !b.Is4() {
			return fmt.Errorf("%w: backend %s is not IPv4", ErrInvalid, b)
		}
	}
	return nil
}

// validateToProxy refuses a proxy exposure that names a network, a backend or the PROXY protocol,
// or a port other than TCP 443 and 80 with its BackendPort equal to it; 443 is required.
func (e Exposure) validateToProxy() error {
	if e.Network != "" || len(e.Backends) > 0 || e.Proxy {
		return fmt.Errorf("%w: a proxy exposure has no network, no backends and no PROXY protocol", ErrInvalid)
	}
	seen := map[int]bool{}
	for _, p := range e.Ports {
		if p.Protocol != wire.ProtocolTCP || p.Port != 443 && p.Port != 80 || p.BackendPort != p.Port || seen[p.Port] {
			return fmt.Errorf("%w: a proxy exposure's ports are TCP 443 and 80, each once, to the same port", ErrInvalid)
		}
		seen[p.Port] = true
	}
	if !seen[443] {
		return fmt.Errorf("%w: a proxy exposure has port 443", ErrInvalid)
	}
	return nil
}

// checkExposure refuses an exposure whose address or backends the host must not serve.
func (m *Manager) checkExposure(e Exposure) error { return checkExposure(m.Base, m.c, e) }

func checkExposure(base, c Config, e Exposure) error {
	if base.LAN == "" {
		return ErrNoLAN
	}
	if err := e.Validate(); err != nil {
		return err
	}
	if e.ToProxy {
		if !base.Proxy.Runs() {
			return ErrNoProxy
		}
		return checkReserved(c, e.Address)
	}
	var n *Network
	for i := range c.Networks {
		if c.Networks[i].Name == e.Network {
			n = &c.Networks[i]
		}
	}
	if n == nil {
		return ErrUnknownNetwork
	}
	if err := checkReserved(c, e.Address); err != nil {
		return err
	}
	for _, b := range e.Backends {
		if !isAttached(c, *n, b) {
			return fmt.Errorf("%w: %s", ErrBackend, b)
		}
	}
	return nil
}

// checkReserved refuses an address of the host's networks or their members, which the host would
// answer for on the LAN.
func checkReserved(c Config, a netip.Addr) error {
	for _, o := range c.Networks {
		if o.Prefix.Contains(a) || slices.ContainsFunc(o.Remote, func(p netip.Prefix) bool { return p.Contains(a) }) {
			return ErrReserved
		}
	}
	for _, x := range c.Members {
		if x.Address == a {
			return ErrReserved
		}
	}
	return nil
}

// isAttached reports whether a is the address of a microVM attached to the network on this host. A
// backend on another host is refused: the host has no source for a flow to it that both ends accept
// (ADR 0085 item 6).
func isAttached(c Config, n Network, a netip.Addr) bool {
	return slices.ContainsFunc(c.Members, func(x Member) bool { return x.Network == n.Name && x.Address == a })
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
// it dropped, also when the host refused the ruleset: a lease that ran out is not served again, and
// the table is made again on the next pass.
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
		if m.ran {
			return nil, nil
		}
		return nil, m.run(ctx, m.c)
	}
	err := m.apply(ctx, next)
	if err != nil {
		m.c.Exposures, m.ran = next.Exposures, false
		_ = m.save()
	}
	return gone, err
}

func (m *Manager) expired(e Exposure) bool { return !m.now().Before(e.Deadline) }
