package api

import (
	"context"
	"errors"
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
	SetPeers(ctx context.Context, peers []network.Peer) error
	Peers() []network.Peer
}

const (
	maxNetworks = 1024
	maxPeers    = 256
	maxPeerNets = 1024
)

// checkNetwork refuses a create on a network that is not declared, and one whose pod prefix is
// outside the network's prefix. It answers and returns false on a refusal.
func (s *Server) checkNetwork(w http.ResponseWriter, req hostapi.CreateRequest) bool {
	if s.noNetworks(w) {
		return false
	}
	n, ok := s.deps.Networks.Network(req.Network)
	if !ok {
		invalid(w, "network")
		return false
	}
	if req.PodPrefix != "" {
		if pf, err := netip.ParsePrefix(req.PodPrefix); err != nil || !n.Prefix.Contains(pf.Addr()) || pf.Bits() < n.Prefix.Bits() {
			invalid(w, "podPrefix")
			return false
		}
	}
	return true
}

// attach puts the microVM on the network the request names, by the address mvmd gave it and the
// pod prefix routed to it. It does nothing for a request with no network.
func (s *Server) attach(ctx context.Context, req hostapi.CreateRequest, m mvmd.Machine) error {
	if req.Network == "" {
		return nil
	}
	if len(m.Addresses) == 0 {
		return errors.New("mvmd gave the microVM no address")
	}
	a, err := netip.ParseAddr(m.Addresses[0])
	if err != nil {
		return err
	}
	member := network.Member{Network: req.Network, Address: a}
	if req.PodPrefix != "" {
		member.Routed = netip.MustParsePrefix(req.PodPrefix)
	}
	return s.deps.Networks.Attach(ctx, member)
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
	v := hostapi.Network{Name: n.Name, Prefix: n.Prefix.String()}
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
	case errors.Is(err, network.ErrOutside):
		status = http.StatusUnprocessableEntity
	}
	if status == http.StatusBadGateway {
		s.deps.Log.Error("cannot apply the networks", "err", err.Error())
	}
	writeJSON(w, status, hostapi.ErrorBody{Error: err.Error()})
}

// putNetwork declares the network of the path's name, or replaces it: a network of that name keeps
// its microVMs. Its prefix is an IPv4 prefix that does not overlap another network's.
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
	if err != nil || !prefix.Addr().Is4() || prefix.Masked() != prefix || prefix.Bits() < 8 || prefix.Bits() > 24 {
		fields = append(fields, "prefix")
	}
	var endpoint netip.Addr
	if req.APIEndpoint != "" {
		endpoint, err = netip.ParseAddr(req.APIEndpoint)
		if err != nil || !prefix.Contains(endpoint) {
			fields = append(fields, "apiEndpoint")
		}
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
	n := network.Network{Name: name, Prefix: prefix, Endpoint: endpoint}
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
		for _, x := range p.Prefixes {
			v.Prefixes = append(v.Prefixes, x.String())
		}
		slices.Sort(v.Prefixes)
		out = append(out, v)
	}
	return out
}

// putPeers replaces the tunnel's peers. Each has a WireGuard public key (44 base64 characters), at
// most one endpoint and the prefixes it holds; a prefix is held by one peer only.
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
			if err != nil || pf.Masked() != pf || !pf.Addr().Is4() || held[pf] {
				bad("peers.prefixes")
				continue
			}
			held[pf] = true
			np.Prefixes = append(np.Prefixes, pf)
		}
		peers = append(peers, np)
	}
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.deps.Networks.SetPeers(ctx, peers); err != nil {
		s.networkFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hostapi.PeersRequest{Peers: peerViews(s.deps.Networks.Peers())})
}

func (s *Server) getPeers(w http.ResponseWriter, _ *http.Request) {
	if s.noNetworks(w) {
		return
	}
	writeJSON(w, http.StatusOK, hostapi.PeersRequest{Peers: peerViews(s.deps.Networks.Peers())})
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
