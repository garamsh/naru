// Package netrec keeps the networks of every admitted host as orch declares them. One pass reads
// each host's tunnel and microVMs, then tells each host which networks exist, which addresses
// and prefixes its networks hold on the other hosts, and which other hosts are its peers.
package netrec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// DefaultInterval is the pause between passes when Config.Interval is zero.
const DefaultInterval = 10 * time.Second

// Host is an admitted host. Address is where its API listens, as host:port.
type Host struct {
	ID      string
	Address string
}

// Tunnel is what a host reports of its tunnel: the key and port peers dial, and the prefixes it
// holds for its own machines.
type Tunnel struct {
	PublicKey       string
	ListenPort      int
	MachinePrefixes []string
}

// MicroVM is a microVM as its host reports it. Network is empty for one on no network.
type MicroVM struct {
	Name      string
	Network   string
	PodPrefix string
	Addresses []string
}

// NetworkState is what a host is told of a network: orch's declaration, and the members of the
// network that are on other hosts, as prefixes (an address is a /32).
type NetworkState struct {
	Prefix        string
	APIEndpoint   string
	RemoteMembers []string
}

// Peer is another host a host's tunnel reaches. Prefixes are the ones that host holds.
type Peer struct {
	Name      string
	PublicKey string
	Endpoint  string
	Prefixes  []string
}

// Hosts is the host API as the reconciler needs it.
type Hosts interface {
	// List returns every admitted host.
	List(ctx context.Context) ([]Host, error)
	Tunnel(ctx context.Context, hostID string) (Tunnel, error)
	MicroVMs(ctx context.Context, hostID string) ([]MicroVM, error)
	// PutNetwork declares the network on the host, replacing what it held under the name.
	PutNetwork(ctx context.Context, hostID, name string, n NetworkState) error
	// PutPeers replaces the host's whole list of peers.
	PutPeers(ctx context.Context, hostID string, peers []Peer) error
}

// Networks lists the networks orch keeps.
type Networks interface {
	List(ctx context.Context) ([]orchapi.Network, error)
}

// Config is what New needs. Whoever holds the leader lease constructs and runs one Reconciler.
type Config struct {
	Hosts    Hosts
	Networks Networks
	Interval time.Duration
	Log      *slog.Logger
}

// Reconciler runs the passes. It keeps no state between them.
type Reconciler struct {
	hosts    Hosts
	networks Networks
	interval time.Duration
	log      *slog.Logger
}

// New returns a Reconciler over the given adapters.
func New(c Config) *Reconciler {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	return &Reconciler{hosts: c.Hosts, networks: c.Networks, interval: c.Interval, log: c.Log}
}

// Run reconciles every interval until ctx ends, and returns ctx's error. A pass that fails is
// logged and does not end the loop.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.log.Error("network pass failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// observed is what a host reported in one pass.
type observed struct {
	host    Host
	tunnel  Tunnel
	microVM []MicroVM
}

// Reconcile runs one pass. A host that does not answer is left alone and logged, and the others
// still converge: it holds no members and is no peer for this pass.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	hosts, err := r.hosts.List(ctx)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}
	nets, err := r.networks.List(ctx)
	if err != nil {
		return fmt.Errorf("list networks: %w", err)
	}
	seen := r.observe(ctx, hosts)
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, o := range seen {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := r.apply(ctx, o, seen, nets); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// observe asks every host for its tunnel and microVMs, and returns the hosts that answered both,
// in ID order.
func (r *Reconciler) observe(ctx context.Context, hosts []Host) []observed {
	got := make([]*observed, len(hosts))
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t, err := r.hosts.Tunnel(ctx, h.ID)
			if err != nil {
				r.log.Warn("host skipped: no tunnel", "host", h.ID, "err", err.Error())
				return
			}
			vms, err := r.hosts.MicroVMs(ctx, h.ID)
			if err != nil {
				r.log.Warn("host skipped: no microVMs", "host", h.ID, "err", err.Error())
				return
			}
			got[i] = &observed{host: h, tunnel: t, microVM: vms}
		}()
	}
	wg.Wait()
	var out []observed
	for _, o := range got {
		if o != nil {
			out = append(out, *o)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].host.ID < out[j].host.ID })
	return out
}

// apply tells one host the networks and the peers.
func (r *Reconciler) apply(ctx context.Context, self observed, all []observed, nets []orchapi.Network) error {
	var errs []error
	for _, n := range nets {
		st := NetworkState{Prefix: n.Prefix, APIEndpoint: n.APIEndpoint, RemoteMembers: remoteMembers(self.host.ID, n.Name, all)}
		if err := r.hosts.PutNetwork(ctx, self.host.ID, n.Name, st); err != nil {
			errs = append(errs, fmt.Errorf("host %s: network %s: %w", self.host.ID, n.Name, err))
		}
	}
	if err := r.hosts.PutPeers(ctx, self.host.ID, peers(self.host.ID, all)); err != nil {
		errs = append(errs, fmt.Errorf("host %s: peers: %w", self.host.ID, err))
	}
	return errors.Join(errs...)
}

// remoteMembers is the members of the network on the hosts other than self: the addresses of
// their microVMs on it as /32 and the Pod prefixes they hold, sorted and without duplicates.
func remoteMembers(self, network string, all []observed) []string {
	set := map[string]bool{}
	for _, o := range all {
		if o.host.ID == self {
			continue
		}
		for _, m := range o.microVM {
			if m.Network != network {
				continue
			}
			for _, a := range m.Addresses {
				set[hostPrefix(a)] = true
			}
			if m.PodPrefix != "" {
				set[m.PodPrefix] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// hostPrefix is the prefix of one address: a /32 for IPv4 and a /128 for IPv6. An address that is
// already a prefix stays as it is.
func hostPrefix(a string) string {
	if addr, err := netip.ParseAddr(a); err == nil {
		return netip.PrefixFrom(addr, addr.BitLen()).String()
	}
	return a
}

// peers is every other host as a peer: its tunnel's key and port at its advertised address, and
// the prefixes it holds, which are its machines' and the Pod prefixes of its members.
func peers(self string, all []observed) []Peer {
	out := []Peer{}
	for _, o := range all {
		if o.host.ID == self {
			continue
		}
		prefixes := slices.Clone(o.tunnel.MachinePrefixes)
		for _, m := range o.microVM {
			if m.PodPrefix != "" && m.Network != "" {
				prefixes = append(prefixes, m.PodPrefix)
			}
		}
		sort.Strings(prefixes)
		out = append(out, Peer{Name: o.host.ID, PublicKey: o.tunnel.PublicKey, Endpoint: endpoint(o.host.Address, o.tunnel.ListenPort), Prefixes: slices.Compact(prefixes)})
	}
	return out
}

// endpoint is the host's address with the tunnel's port in place of the API's.
func endpoint(address string, port int) string {
	h, _, err := net.SplitHostPort(address)
	if err != nil {
		h = address
	}
	return net.JoinHostPort(h, strconv.Itoa(port))
}
