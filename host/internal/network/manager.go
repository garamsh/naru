package network

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"sort"
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
// them. Every change applies the whole state, then writes it to State, so the host converges on
// what Manager holds and Load returns it to that after a restart.
type Manager struct {
	Run Runner
	// Tunnel is the host's end of the tunnel; nil leaves the host with no tunnel.
	Tunnel Tunnel
	Base   Config // Management, TapPrefix and Egress, which do not change
	// State is the file the state is kept in; empty keeps it in memory only.
	State string

	mu sync.Mutex
	c  Config
}

// state is the file: everything a restart must bring back.
type state struct {
	Networks []Network `json:"networks"`
	Members  []Member  `json:"members"`
	Peers    []Peer    `json:"peers"`
}

// Load reads State, when it exists, and applies it, so the host is closed by the ruleset before
// the API serves, whatever State holds.
func (m *Manager) Load(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var next Config
	if m.State != "" {
		raw, err := os.ReadFile(m.State)
		switch {
		case err == nil:
			var s state
			if err := json.Unmarshal(raw, &s); err != nil {
				return fmt.Errorf("%s: %w", m.State, err)
			}
			next.Networks, next.Members, next.Peers = s.Networks, s.Members, s.Peers
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
	}
	return m.run(ctx, next)
}

// TunnelInfo returns the tunnel's public key and port; false when the host has no tunnel.
func (m *Manager) TunnelInfo() (TunnelInfo, bool) {
	if m.Tunnel == nil {
		return TunnelInfo{}, false
	}
	return TunnelInfo{PublicKey: m.Tunnel.PublicKey(), Port: m.Tunnel.Port()}, true
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

// Members returns the attached microVMs, ordered by address.
func (m *Manager) Members() []Member {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]Member(nil), m.c.Members...)
	sort.Slice(out, func(i, j int) bool { return out[i].Address.Less(out[j].Address) })
	return out
}

// Declare adds the network or replaces the one of its name, keeping the members attached to it, and
// applies. A prefix that overlaps another network's is refused, and so is one that leaves a member's
// pod prefix outside it.
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
	for _, x := range m.c.Members {
		if x.Network == n.Name && !inside(n.Prefix, x.Routed) {
			return fmt.Errorf("%w: the pod prefix %s of %s", ErrOutside, x.Routed, x.Address)
		}
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

// inside reports whether the pod prefix p, when there is one, is inside the network prefix n.
func inside(n, p netip.Prefix) bool {
	return !p.IsValid() || (n.Contains(p.Addr()) && p.Bits() >= n.Bits())
}

// Attach adds a member and applies. The member's address is whatever its host gave it; only its pod
// prefix must be inside its network's prefix. A microVM is attached once: a second Attach of the
// same address replaces the first.
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
	if !inside(n.Prefix, x.Routed) {
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

// run makes next the host's state: the ruleset first, then the tunnel.
func (m *Manager) run(ctx context.Context, next Config) error {
	next.Management, next.TapPrefix, next.Egress = m.Base.Management, m.Base.TapPrefix, m.Base.Egress
	if _, err := m.Run.Run(ctx, []byte(Ruleset(next)), "nft", "-f", "-"); err != nil {
		return err
	}
	if m.Tunnel != nil {
		if err := m.Tunnel.Set(next.Peers); err != nil {
			return err
		}
	}
	m.c = next
	return nil
}

// apply makes next the state when the host accepted it and State holds it. A failure leaves the
// state as it was; one to write State puts the host back to the state before.
func (m *Manager) apply(ctx context.Context, next Config) error {
	prev := m.c
	if err := m.run(ctx, next); err != nil {
		return err
	}
	if m.State == "" {
		return nil
	}
	raw, err := json.Marshal(state{Networks: sortedNetworks(m.c.Networks), Members: m.c.Members, Peers: sortedPeers(m.c.Peers)})
	if err == nil {
		err = writeFile(m.State, raw)
	}
	if err != nil {
		_ = m.run(ctx, prev)
		return fmt.Errorf("write %s: %w", m.State, err)
	}
	return nil
}
