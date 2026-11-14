// Package network keeps the virtual networks of a host closed by default: it generates the nftables
// ruleset that confines each network to its own membership set, makes the WireGuard tunnel that
// carries the networks' prefixes between hosts, and applies both. The state it holds survives a
// restart of the host daemon.
package network

import (
	"net/netip"
	"sort"
)

// Network is a virtual network: a membership set. Its prefix is the Pod and routed space, which no
// other network's prefix overlaps; its members are the prefix, the address and pod prefix of each
// microVM attached on this host, and the Remote members, which are those on other hosts.
type Network struct {
	Name   string       `json:"name"`
	Prefix netip.Prefix `json:"prefix"`
	// Endpoint is the address of the network's API endpoint, which the management plane reaches one
	// way. The zero value means the network has none.
	Endpoint netip.Addr `json:"endpoint"`
	// Remote are the addresses (/32) and pod prefixes of the network's microVMs on other hosts.
	Remote []netip.Prefix `json:"remoteMembers"`
}

// Member is a microVM attached to a network: its own address, which mvmd chose and need not be
// inside the network's prefix, and the prefix routed to it, which is.
type Member struct {
	Network string       `json:"network"`
	Address netip.Addr   `json:"address"`
	Routed  netip.Prefix `json:"podPrefix"` // zero when the microVM has none
}

// Peer is another host the tunnel reaches: its public key, where it listens, and the prefixes it
// holds, which are the only ones the tunnel accepts from it and sends to it.
type Peer struct {
	Name      string         `json:"name"`
	PublicKey string         `json:"publicKey"`
	Endpoint  netip.AddrPort `json:"endpoint"`
	Prefixes  []netip.Prefix `json:"prefixes"`
}

// Config is everything the ruleset and the tunnel are made from.
type Config struct {
	Networks []Network
	Members  []Member
	Peers    []Peer
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
