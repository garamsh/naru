package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
)

// Networks is what the host API asks of the host's networks: *network.Manager.
type Networks interface {
	Declare(ctx context.Context, n network.Network) error
	Remove(ctx context.Context, name string) error
	Network(name string) (network.Network, bool)
	Networks() []network.Network
	Attach(ctx context.Context, m network.Member) error
	Detach(ctx context.Context, a netip.Addr) error
	SetPeers(ctx context.Context, mesh netip.Addr, peers []network.Peer) error
	Peers() []network.Peer
	Mesh() netip.Addr
	Members() []network.Member
	TunnelInfo() (network.TunnelInfo, bool)
}

const (
	maxNetworks = 1024
	maxPeers    = 256
	maxPeerNets = 1024
	// maxRemoteMembers bounds the remote members of one network: every microVM on the other hosts.
	maxRemoteMembers = 65536
	// minPrefixBits is the widest prefix a network, a remote member or a peer may hold: a /0 in a set
	// or an allowed-IPs list would reach every address.
	minPrefixBits = 8
)

// checkNetwork refuses a create or resume on a network that is not declared, and one whose pod
// prefix is outside the network's prefix. It answers and returns false on a refusal. The
// microVM's address is not checked: mvmd chose it, from the host's Machine prefix.
func (s *Server) checkNetwork(w http.ResponseWriter, name string) bool {
	if s.noNetworks(w) {
		return false
	}
	if _, ok := s.deps.Networks.Network(name); !ok {
		invalid(w, "network")
		return false
	}
	return true
}

// attach puts the microVM on the named network, by the address mvmd gave it. It does nothing for a
// request with no network.
func (s *Server) attach(ctx context.Context, name string, m mvmd.Machine) error {
	if name == "" {
		return nil
	}
	if len(m.Addresses) == 0 {
		return errors.New("mvmd gave the microVM no address")
	}
	a, err := netip.ParseAddr(m.Addresses[0])
	if err != nil {
		return err
	}
	return s.deps.Networks.Attach(ctx, network.Member{Network: name, Address: a})
}

// memberOf returns the membership of the Machine, by any address it has.
func (s *Server) memberOf(m mvmd.Machine) (network.Member, bool) {
	if s.deps.Networks == nil {
		return network.Member{}, false
	}
	for _, x := range s.deps.Networks.Members() {
		if slices.Contains(m.Addresses, x.Address.String()) {
			return x, true
		}
	}
	return network.Member{}, false
}

// detach takes a deleted microVM off its network. A failure is logged: the microVM is gone, and
// the next change of the networks drops the member's rules with the rest.
func (s *Server) detach(ctx context.Context, m mvmd.Machine) {
	if s.deps.Networks == nil {
		return
	}
	for _, x := range m.Addresses {
		if a, err := netip.ParseAddr(x); err == nil {
			if err := s.deps.Networks.Detach(ctx, a); err != nil {
				s.deps.Log.Error("cannot detach a deleted microVM from its network", "machine", m.ID, "err", err.Error())
			}
		}
	}
}

func networkView(n network.Network) hostapi.Network {
	v := hostapi.Network{Name: n.Name, Prefix: n.Prefix.String(), RemoteMembers: []string{}}
	for _, p := range n.Remote {
		v.RemoteMembers = append(v.RemoteMembers, p.String())
	}
	if n.Endpoint.IsValid() {
		v.APIEndpoint = n.Endpoint.String()
	}
	return v
}

// noNetworks answers a call that needs networks when the host has none configured.
func (s *Server) noNetworks(w http.ResponseWriter) bool {
	if s.deps.Networks != nil {
		return false
	}
	writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started without networks"})
	return true
}

// networkStatus maps a refusal of the networks to a status.
func (s *Server) networkFail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, network.ErrOverlap):
		status = http.StatusConflict
	case errors.Is(err, network.ErrUnknownNetwork):
		status = http.StatusNotFound
	}
	if status == http.StatusBadGateway {
		s.deps.Log.Error("cannot apply the networks", "err", err.Error())
	}
	writeJSON(w, status, hostapi.ErrorBody{Error: err.Error()})
}

// putNetwork declares the network of the path's name, or replaces it: a network of that name keeps
// its microVMs. Its prefix is an IPv4 prefix that does not overlap another network's, and its
// remote members are the IPv4 prefixes (/32 for an address) of its microVMs on other hosts.
func (s *Server) putNetwork(w http.ResponseWriter, r *http.Request) {
	if s.noNetworks(w) {
		return
	}
	var req hostapi.NetworkRequest
	if !decode(w, r, &req) {
		return
	}
	name := r.PathValue("name")
	var fields []string
	if !dnsLabel.MatchString(name) {
		fields = append(fields, "name")
	}
	prefix, err := netip.ParsePrefix(req.Prefix)
	if err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix || prefix.Bits() < minPrefixBits || prefix.Bits() > 24 {
		fields = append(fields, "prefix")
	}
	var endpoint netip.Addr
	if req.APIEndpoint != "" {
		endpoint, err = netip.ParseAddr(req.APIEndpoint)
		if err != nil || !prefix.Contains(endpoint) {
			fields = append(fields, "apiEndpoint")
		}
	}
	var remote []netip.Prefix
	if len(req.RemoteMembers) > maxRemoteMembers {
		fields = append(fields, "remoteMembers")
	}
	for i, x := range req.RemoteMembers {
		p, err := netip.ParsePrefix(x)
		if err != nil || !p.Addr().Is4() || p.Masked() != p || p.Bits() < minPrefixBits {
			fields = append(fields, fmt.Sprintf("remoteMembers[%d]", i))
			continue
		}
		remote = append(remote, p)
	}
	if len(fields) == 0 && len(s.deps.Networks.Networks()) >= maxNetworks {
		if _, ok := s.deps.Networks.Network(name); !ok {
			writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "the host holds the most networks it takes"})
			return
		}
	}
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	n := network.Network{Name: name, Prefix: prefix, Endpoint: endpoint, Remote: remote}
	if err := s.deps.Networks.Declare(ctx, n); err != nil {
		s.networkFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, networkView(n))
}

func (s *Server) getNetwork(w http.ResponseWriter, r *http.Request) {
	if s.noNetworks(w) {
		return
	}
	n, ok := s.deps.Networks.Network(r.PathValue("name"))
	if !ok {
		writeJSON(w, http.StatusNotFound, hostapi.ErrorBody{Error: network.ErrUnknownNetwork.Error()})
		return
	}
	writeJSON(w, http.StatusOK, networkView(n))
}

func (s *Server) listNetworks(w http.ResponseWriter, _ *http.Request) {
	if s.noNetworks(w) {
		return
	}
	out := []hostapi.Network{}
	for _, n := range s.deps.Networks.Networks() {
		out = append(out, networkView(n))
	}
	writeJSON(w, http.StatusOK, map[string]any{"networks": out})
}

// deleteNetwork removes the network and detaches its microVMs, which then reach nothing. An absent
// network is 204.
func (s *Server) deleteNetwork(w http.ResponseWriter, r *http.Request) {
	if s.noNetworks(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Networks.Remove(ctx, r.PathValue("name")); err != nil {
		s.networkFail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func peerViews(in []network.Peer) []hostapi.Peer {
	out := []hostapi.Peer{}
	for _, p := range in {
		v := hostapi.Peer{Name: p.Name, PublicKey: p.PublicKey, Prefixes: []string{}}
		if p.Endpoint.IsValid() {
			v.Endpoint = p.Endpoint.String()
		}
		if p.Mesh.IsValid() {
			v.MeshAddress = p.Mesh.String()
		}
		for _, x := range p.Prefixes {
			v.Prefixes = append(v.Prefixes, x.String())
		}
		slices.Sort(v.Prefixes)
		out = append(out, v)
	}
	return out
}

// putPeers replaces the tunnel's peers and the host's mesh address. Each peer has a WireGuard public
// key (44 base64 characters), at most one endpoint, the prefixes it holds and at most one mesh
// address; a prefix or a mesh address is held by one peer only, and the host's own mesh address by
// none.
func (s *Server) putPeers(w http.ResponseWriter, r *http.Request) {
	if s.noNetworks(w) {
		return
	}
	var req hostapi.PeersRequest
	if !decode(w, r, &req) {
		return
	}
	if len(req.Peers) > maxPeers {
		invalid(w, "peers")
		return
	}
	var (
		peers  []network.Peer
		fields []string
		keys   = map[string]bool{}
		held   = map[netip.Prefix]bool{}
	)
	bad := func(f string) {
		if !slices.Contains(fields, f) {
			fields = append(fields, f)
		}
	}
	mesh, ok := meshAddress(req.MeshAddress)
	if !ok {
		bad("meshAddress")
	}
	if mesh.IsValid() {
		held[netip.PrefixFrom(mesh, 32)] = true
	}
	for _, p := range req.Peers {
		np := network.Peer{Name: p.Name, PublicKey: p.PublicKey}
		if !validWireGuardKey(p.PublicKey) || keys[p.PublicKey] {
			bad("peers.publicKey")
		}
		keys[p.PublicKey] = true
		if p.Endpoint != "" {
			ep, err := netip.ParseAddrPort(p.Endpoint)
			if err != nil {
				bad("peers.endpoint")
			}
			np.Endpoint = ep
		}
		if len(p.Prefixes) > maxPeerNets {
			bad("peers.prefixes")
			continue
		}
		for _, x := range p.Prefixes {
			pf, err := netip.ParsePrefix(x)
			if err != nil || pf.Masked() != pf || !pf.Addr().Is4() || pf.Bits() < minPrefixBits || held[pf] {
				bad("peers.prefixes")
				continue
			}
			held[pf] = true
			np.Prefixes = append(np.Prefixes, pf)
		}
		if m, ok := meshAddress(p.MeshAddress); !ok || held[netip.PrefixFrom(m, 32)] {
			bad("peers.meshAddress")
		} else if m.IsValid() {
			held[netip.PrefixFrom(m, 32)] = true
			np.Mesh = m
		}
		peers = append(peers, np)
	}
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Networks.SetPeers(ctx, mesh, peers); err != nil {
		s.networkFail(w, err)
		return
	}
	s.writePeers(w)
}

func (s *Server) getPeers(w http.ResponseWriter, _ *http.Request) {
	if s.noNetworks(w) {
		return
	}
	s.writePeers(w)
}

// writePeers answers the peers and the mesh address the host holds.
func (s *Server) writePeers(w http.ResponseWriter) {
	out := hostapi.PeersRequest{Peers: peerViews(s.deps.Networks.Peers())}
	if m := s.deps.Networks.Mesh(); m.IsValid() {
		out.MeshAddress = m.String()
	}
	writeJSON(w, http.StatusOK, out)
}

// meshAddress parses a mesh address: empty for none, else an IPv4 address that may be a host's own.
// ok is false for anything else.
func meshAddress(s string) (netip.Addr, bool) {
	if s == "" {
		return netip.Addr{}, true
	}
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() || !a.IsGlobalUnicast() {
		return netip.Addr{}, false
	}
	return a, true
}

// validWireGuardKey reports whether k is a WireGuard key as wg prints it: 32 bytes in base64,
// which is 44 characters ending in '='.
func validWireGuardKey(k string) bool {
	if len(k) != 44 || k[43] != '=' {
		return false
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for _, c := range k[:43] {
		if !strings.ContainsRune(alphabet, c) {
			return false
		}
	}
	return strings.ContainsRune("AEIMQUYcgkosw048", rune(k[42]))
}

// getTunnel reports what a peer needs to reach this host: the tunnel's public key and port, the
// prefixes the host's microVMs have addresses in, and the host's mesh address. The private key is never reported.
func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request) {
	if s.noNetworks(w) {
		return
	}
	t, ok := s.deps.Networks.TunnelInfo()
	if !ok {
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started without a tunnel"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	info, err := s.deps.Runtime.Info(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	prefixes := info.MachinePrefixes
	if prefixes == nil {
		prefixes = []string{}
	}
	out := hostapi.Tunnel{PublicKey: t.PublicKey, ListenPort: t.Port, MachinePrefixes: prefixes}
	if t.Mesh.IsValid() {
		out.MeshAddress = t.Mesh.String()
	}
	writeJSON(w, http.StatusOK, out)
}
