// Package network keeps the virtual networks of a host closed by default: it generates the nftables
// ruleset that confines each network to its own prefix and the WireGuard arguments that carry the
// networks' prefixes between hosts, and applies both.
package network

import (
	"net/netip"
	"sort"
)

// Network is a virtual network: a name and the one prefix every microVM, Pod and API endpoint of it
// lives in.
type Network struct {
	Name   string
	Prefix netip.Prefix
	// Endpoint is the address of the network's API endpoint, which the management plane reaches one
	// way. The zero value means the network has none.
	Endpoint netip.Addr
}

// Member is a microVM attached to a network: its own address and the prefix routed to it, which is
// inside the network's prefix.
type Member struct {
	Network string
	Address netip.Addr
	Routed  netip.Prefix // zero when the microVM has none
}

// Peer is another host the tunnel reaches: its public key, where it listens, and the prefixes it
// holds, which are the only ones the tunnel accepts from it and sends to it.
type Peer struct {
	Name      string
	PublicKey string
	Endpoint  netip.AddrPort
	Prefixes  []netip.Prefix
}

// Config is everything the ruleset and the tunnel are made from.
type Config struct {
	Networks []Network
	Members  []Member
	Peers    []Peer
	// Management is the prefix of the management plane: nothing from a network reaches it, and it
	// reaches only a network's API endpoint.
	Management netip.Prefix
	// HostAddrs are the host's own addresses (the host API, the Node API, mvmd): nothing from a
	// network reaches them.
	HostAddrs []netip.Addr
	// TapPrefix is the name prefix of the microVMs' tap devices, such as "tap". A packet from one of
	// them whose source is no member's is forged and is dropped and counted. Empty turns the rule off.
	TapPrefix string
	// Egress is the interface a network's traffic leaves the cluster by. Empty means a network has
	// no egress.
	Egress string
	// Tunnel is the WireGuard interface.
	Tunnel string
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
