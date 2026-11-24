package hostclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/netrec"
)

// NetAPI is one host's API as the network reconciler uses it. The tests' fakes implement it,
// and so does the client over lib/hostapi that DialNetAPI returns.
type NetAPI interface {
	Tunnel(ctx context.Context) (netrec.Tunnel, error)
	MicroVMs(ctx context.Context) ([]netrec.MicroVM, error)
	PutNetwork(ctx context.Context, name string, n netrec.NetworkState) error
	PutPeers(ctx context.Context, meshAddress string, peers []netrec.Peer) error
	Networks(ctx context.Context) ([]string, error)
	DeleteNetwork(ctx context.Context, name string) error
}

// NetHosts implements netrec.Hosts: it reads the admitted hosts from the inventory and reaches
// each at the address the inventory recorded, through the NetAPI that dial returns for it.
//
// A host's tunnel and microVMs are read through a probe that tolerates probeFailures-1 failures in a
// row by answering with the last answer, so one lost probe does not take the host's peers and
// routes from every other host.
type NetHosts struct {
	inv  Inventory
	dial func(id, addr string) NetAPI

	mu      sync.Mutex
	pass    map[string]orchapi.Host
	tunnels probes[netrec.Tunnel]
	vms     probes[[]netrec.MicroVM]
}

var _ netrec.Hosts = (*NetHosts)(nil)

// NewNetHosts returns the reconciler's view of the hosts of inv.
func NewNetHosts(inv Inventory, dial func(id, addr string) NetAPI) *NetHosts {
	return &NetHosts{inv: inv, dial: dial, tunnels: probes[netrec.Tunnel]{by: map[string]*probed[netrec.Tunnel]{}}, vms: probes[[]netrec.MicroVM]{by: map[string]*probed[[]netrec.MicroVM]{}}}
}

// List returns every admitted host with the address of its API.
func (h *NetHosts) List(ctx context.Context) ([]netrec.Host, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]netrec.Host, len(admitted))
	pass := make(map[string]orchapi.Host, len(admitted))
	for i, a := range admitted {
		out[i] = netrec.Host{ID: a.ID, Address: a.Address, MeshAddress: a.MeshAddress}
		pass[a.ID] = a
	}
	h.mu.Lock()
	h.pass = pass
	h.mu.Unlock()
	h.tunnels.keep(pass)
	h.vms.keep(pass)
	return out, nil
}

// api is the API of the host with the ID, at the address the last List read for it, and the
// context of one call that ends after timeout.
func (h *NetHosts) api(ctx context.Context, id string, timeout time.Duration) (NetAPI, context.Context, context.CancelFunc, error) {
	h.mu.Lock()
	a, ok := h.pass[id]
	h.mu.Unlock()
	if !ok {
		return nil, nil, nil, fmt.Errorf("host %s is not admitted", id)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return h.dial(a.ID, a.Address), ctx, cancel, nil
}

// Tunnel reads the host's tunnel.
func (h *NetHosts) Tunnel(ctx context.Context, id string) (netrec.Tunnel, error) {
	return h.tunnels.get(id, func() (netrec.Tunnel, error) {
		a, ctx, cancel, err := h.api(ctx, id, probeTimeout)
		if err != nil {
			return netrec.Tunnel{}, err
		}
		defer cancel()
		return a.Tunnel(ctx)
	})
}

// MicroVMs reads the host's microVMs.
func (h *NetHosts) MicroVMs(ctx context.Context, id string) ([]netrec.MicroVM, error) {
	return h.vms.get(id, func() ([]netrec.MicroVM, error) {
		a, ctx, cancel, err := h.api(ctx, id, probeTimeout)
		if err != nil {
			return nil, err
		}
		defer cancel()
		return a.MicroVMs(ctx)
	})
}

// PutNetwork declares the network on the host.
func (h *NetHosts) PutNetwork(ctx context.Context, id, name string, n netrec.NetworkState) error {
	a, ctx, cancel, err := h.api(ctx, id, callTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	return a.PutNetwork(ctx, name, n)
}

// Networks lists the names of the networks the host holds.
func (h *NetHosts) Networks(ctx context.Context, id string) ([]string, error) {
	a, ctx, cancel, err := h.api(ctx, id, callTimeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	return a.Networks(ctx)
}

// DeleteNetwork removes the network from the host.
func (h *NetHosts) DeleteNetwork(ctx context.Context, id, name string) error {
	a, ctx, cancel, err := h.api(ctx, id, callTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	return a.DeleteNetwork(ctx, name)
}

// PutPeers replaces the host's peers and its mesh address.
func (h *NetHosts) PutPeers(ctx context.Context, id, meshAddress string, peers []netrec.Peer) error {
	a, ctx, cancel, err := h.api(ctx, id, callTimeout)
	if err != nil {
		return err
	}
	defer cancel()
	return a.PutPeers(ctx, meshAddress, peers)
}

// hostNet is NetAPI over lib/hostapi.
type hostNet struct{ api *hostapi.Client }

// DialNetAPI returns the dial func for NewNetHosts that reaches a host at its recorded address
// through hc, which carries orch's client identity for the host API. The host's certificate is
// accepted when it chains to hc's authority, names the address or the host's ID and is not on revocations.
func DialNetAPI(hc *http.Client, revocations *identity.RevocationList) func(id, addr string) NetAPI {
	n := newNamed(hc, revocations)
	return func(id, addr string) NetAPI {
		return hostNet{api: hostapi.NewClient((&url.URL{Scheme: "https", Host: addr}).String(), withToken(n.client(id, addr)))}
	}
}

func (n hostNet) Tunnel(ctx context.Context) (netrec.Tunnel, error) {
	t, err := n.api.Tunnel(ctx)
	if err != nil {
		return netrec.Tunnel{}, err
	}
	return netrec.Tunnel{PublicKey: t.PublicKey, ListenPort: t.ListenPort, MachinePrefixes: t.MachinePrefixes}, nil
}

func (n hostNet) MicroVMs(ctx context.Context) ([]netrec.MicroVM, error) {
	vms, err := n.api.MicroVMs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]netrec.MicroVM, len(vms))
	for i, m := range vms {
		out[i] = netrec.MicroVM{Name: m.Name, Network: m.Network, Addresses: m.Addresses}
	}
	return out, nil
}

func (n hostNet) PutNetwork(ctx context.Context, name string, s netrec.NetworkState) error {
	_, err := n.api.PutNetwork(ctx, name, hostapi.NetworkRequest{Prefix: s.Prefix, APIEndpoint: s.APIEndpoint, RemoteMembers: s.RemoteMembers})
	return err
}

func (n hostNet) PutPeers(ctx context.Context, meshAddress string, peers []netrec.Peer) error {
	out := make([]hostapi.Peer, len(peers))
	for i, p := range peers {
		out[i] = hostapi.Peer{Name: p.Name, PublicKey: p.PublicKey, Endpoint: p.Endpoint, Prefixes: p.Prefixes, MeshAddress: p.MeshAddress}
	}
	_, err := n.api.SetPeers(ctx, hostapi.PeersRequest{MeshAddress: meshAddress, Peers: out})
	return err
}

func (n hostNet) Networks(ctx context.Context) ([]string, error) {
	nets, err := n.api.Networks(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(nets))
	for i, x := range nets {
		out[i] = x.Name
	}
	return out, nil
}

func (n hostNet) DeleteNetwork(ctx context.Context, name string) error {
	return n.api.DeleteNetwork(ctx, name)
}
