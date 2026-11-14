package network

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
	"sync"
)

// Errors a Manager answers; the API maps them to statuses.
var (
	ErrUnknownNetwork = errors.New("no such network")
	ErrOutside        = errors.New("outside the network's prefix")
	ErrOverlap        = errors.New("overlaps another network's prefix")
)

// Runner runs a program with stdin and returns its stdout.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// ExecRunner runs programs on the host.
type ExecRunner struct{}

// Run runs name with args, feeding it stdin. A failure carries the program's stderr.
func (ExecRunner) Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// Manager holds the host's networks, their members and the tunnel's peers, and applies them.
// Orch declares networks and peers through the host API; the members are the microVMs created on
// them. Every change applies the whole state, so the host converges on what Manager holds.
type Manager struct {
	Run     Runner
	Base    Config // Management, HostAddrs, TapPrefix, Egress and Tunnel, which do not change
	KeyFile string // the tunnel's private key, a file of mode 0600
	Port    int    // the tunnel's listen port

	mu sync.Mutex
	c  Config
}

// Networks returns the declared networks, ordered by name.
func (m *Manager) Networks() []Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedNetworks(m.c.Networks)
}

// Network returns the network named name.
func (m *Manager) Network(name string) (Network, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.c.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return Network{}, false
}

// Declare adds the network or replaces the one of its name, and applies. A prefix that overlaps
// another network's is refused.
func (m *Manager) Declare(ctx context.Context, n Network) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Networks = nil
	for _, o := range m.c.Networks {
		if o.Name == n.Name {
			continue
		}
		if o.Prefix.Overlaps(n.Prefix) {
			return fmt.Errorf("%w: %s", ErrOverlap, o.Name)
		}
		next.Networks = append(next.Networks, o)
	}
	next.Networks = append(next.Networks, n)
	return m.apply(ctx, next)
}

// Remove drops the network and its members, and applies. An absent network is not an error.
func (m *Manager) Remove(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Networks, next.Members = nil, nil
	for _, o := range m.c.Networks {
		if o.Name != name {
			next.Networks = append(next.Networks, o)
		}
	}
	for _, x := range m.c.Members {
		if x.Network != name {
			next.Members = append(next.Members, x)
		}
	}
	return m.apply(ctx, next)
}

// Attach adds a member, which must be inside its network's prefix, and applies. A microVM is
// attached once: a second Attach of the same address replaces the first.
func (m *Manager) Attach(ctx context.Context, x Member) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n *Network
	for i := range m.c.Networks {
		if m.c.Networks[i].Name == x.Network {
			n = &m.c.Networks[i]
		}
	}
	if n == nil {
		return ErrUnknownNetwork
	}
	if !n.Prefix.Contains(x.Address) || (x.Routed.IsValid() && !(n.Prefix.Contains(x.Routed.Addr()) && x.Routed.Bits() >= n.Prefix.Bits())) {
		return ErrOutside
	}
	next := m.c
	next.Members = nil
	for _, o := range m.c.Members {
		if o.Address != x.Address {
			next.Members = append(next.Members, o)
		}
	}
	next.Members = append(next.Members, x)
	return m.apply(ctx, next)
}

// Detach drops the member of address a, and applies. An absent one is not an error.
func (m *Manager) Detach(ctx context.Context, a netip.Addr) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Members = nil
	for _, o := range m.c.Members {
		if o.Address != a {
			next.Members = append(next.Members, o)
		}
	}
	return m.apply(ctx, next)
}

// SetPeers replaces the tunnel's peers, and applies.
func (m *Manager) SetPeers(ctx context.Context, peers []Peer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.c
	next.Peers = append([]Peer(nil), peers...)
	return m.apply(ctx, next)
}

// Peers returns the tunnel's peers, ordered by public key.
func (m *Manager) Peers() []Peer {
	m.mu.Lock()
	defer m.mu.Unlock()
	return sortedPeers(m.c.Peers)
}

// apply makes next the state when the host accepted it: the ruleset first, then the tunnel. A
// failure leaves the state as it was.
func (m *Manager) apply(ctx context.Context, next Config) error {
	next.Management, next.HostAddrs, next.TapPrefix = m.Base.Management, m.Base.HostAddrs, m.Base.TapPrefix
	next.Egress, next.Tunnel = m.Base.Egress, m.Base.Tunnel
	if _, err := m.Run.Run(ctx, []byte(Ruleset(next)), "nft", "-f", "-"); err != nil {
		return err
	}
	if next.Tunnel != "" {
		out, err := m.Run.Run(ctx, nil, "wg", "show", next.Tunnel, "peers")
		if err != nil {
			return err
		}
		if _, err := m.Run.Run(ctx, nil, "wg", TunnelArgs(next, m.KeyFile, m.Port, strings.Fields(string(out)))...); err != nil {
			return err
		}
	}
	m.c = next
	return nil
}
