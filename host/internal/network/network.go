// Package network keeps the virtual networks of a host closed by default: it generates the nftables
// ruleset that confines each network to its own membership set, makes the WireGuard tunnel that
// carries the networks' prefixes between hosts, and applies both. The state it holds survives a
// restart of the host daemon.
package network

import (
	"net/netip"
	"sort"
	"time"
)

// Network is a virtual network: a membership set. Its prefix is the routed space, which no other
// network's prefix overlaps; its members are the prefix, the address of each microVM attached on
// this host, and the Remote members, which are those on other hosts.
type Network struct {
	Name   string       `json:"name"`
	Prefix netip.Prefix `json:"prefix"`
	// Endpoint is the address of the network's API endpoint, which the management plane reaches one
	// way. The zero value means the network has none.
	Endpoint netip.Addr `json:"endpoint"`
	// Remote are the addresses (/32) of the network's microVMs on other hosts.
	Remote []netip.Prefix `json:"remoteMembers"`
}

// Member is a microVM attached to a network: its own address, which mvmd chose and need not be
// inside the network's prefix.
type Member struct {
	Network string     `json:"network"`
	Address netip.Addr `json:"address"`
}

// Peer is another host the tunnel reaches: its public key, where it listens, the prefixes it
// holds and its mesh address, which are the only addresses the tunnel accepts from it and sends to
// it. Mesh is the zero value for a peer that has none. Proxy is whether the peer runs naru-proxy,
// whose mesh address alone of the hosts' reaches this host's microVMs.
type Peer struct {
	Name      string         `json:"name"`
	PublicKey string         `json:"publicKey"`
	Endpoint  netip.AddrPort `json:"endpoint"`
	Prefixes  []netip.Prefix `json:"prefixes"`
	Mesh      netip.Addr     `json:"meshAddress,omitzero"`
	Proxy     bool           `json:"proxy,omitempty"`
}

// allowed is what the tunnel accepts from p and routes to p: its prefixes and its mesh address.
func (p Peer) allowed() []netip.Prefix {
	out := append([]netip.Prefix(nil), p.Prefixes...)
	if p.Mesh.IsValid() {
		out = append(out, netip.PrefixFrom(p.Mesh, 32))
	}
	return out
}

// Config is everything the ruleset and the tunnel are made from.
type Config struct {
	Networks []Network
	Members  []Member
	Peers    []Peer
	// Mesh is the host's own address on the tunnel (ADR 0113 item 1); the zero value means none.
	Mesh netip.Addr
	// Management is the prefix of the management plane: nothing from a network reaches it, and it
	// reaches only a network's API endpoint.
	Management netip.Prefix
	// TapPrefix is the name prefix of the microVMs' tap devices, such as "tap". A packet from one of
	// them whose source is no local member's is forged and is dropped and counted, and so is a new
	// connection from one to any address of the host. Empty turns both rules off.
	TapPrefix string
	// Egress is the interface a network's traffic leaves the cluster by. Empty means a network has
	// no egress.
	Egress string
	// LAN is the interface the host announces exposed addresses on; empty means the host exposes
	// none.
	LAN string
	// Exposures are the leases the host holds; the ruleset forwards their ports.
	Exposures []Exposure
	// Proxy is the host's naru-proxy, which a proxy exposure's ports are redirected to; its zero
	// value means the host runs none, and refuses a proxy exposure.
	Proxy ProxyPorts
	// At is the time the leases' time left is counted from: the instant the ruleset is made.
	At time.Time
}

func sortedNetworks(in []Network) []Network {
	out := append([]Network(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortedPeers(in []Peer) []Peer {
	out := append([]Peer(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].PublicKey < out[j].PublicKey })
	return out
}

func sortedPrefixes(in []netip.Prefix) []netip.Prefix {
	out := append([]netip.Prefix(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}
