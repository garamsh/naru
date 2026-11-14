package hostclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/orch/internal/netrec"
)

// NetAPI is one host's API as the network reconciler uses it. The tests' fakes implement it,
// and so does the client over lib/hostapi that DialNetAPI returns.
type NetAPI interface {
	Tunnel(ctx context.Context) (netrec.Tunnel, error)
	MicroVMs(ctx context.Context) ([]netrec.MicroVM, error)
	PutNetwork(ctx context.Context, name string, n netrec.NetworkState) error
	PutPeers(ctx context.Context, peers []netrec.Peer) error
	Networks(ctx context.Context) ([]string, error)
	DeleteNetwork(ctx context.Context, name string) error
}

// NetHosts implements netrec.Hosts: it reads the admitted hosts from the inventory and reaches
// each at the address the inventory recorded, through the NetAPI that dial returns for it.
type NetHosts struct {
	inv  Inventory
	dial func(addr string) NetAPI
}

var _ netrec.Hosts = (*NetHosts)(nil)

// NewNetHosts returns the reconciler's view of the hosts of inv.
func NewNetHosts(inv Inventory, dial func(addr string) NetAPI) *NetHosts {
	return &NetHosts{inv: inv, dial: dial}
}

// List returns every admitted host with the address of its API.
func (h *NetHosts) List(ctx context.Context) ([]netrec.Host, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]netrec.Host, len(admitted))
	for i, a := range admitted {
		out[i] = netrec.Host{ID: a.ID, Address: a.Address}
	}
	return out, nil
}

// api is the API of the host with the ID, at the address the inventory holds for it now.
func (h *NetHosts) api(ctx context.Context, id string) (NetAPI, error) {
	admitted, err := h.inv.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	for _, a := range admitted {
		if a.ID == id {
			return h.dial(a.Address), nil
		}
	}
	return nil, fmt.Errorf("host %s is not admitted", id)
}

// Tunnel reads the host's tunnel.
func (h *NetHosts) Tunnel(ctx context.Context, id string) (netrec.Tunnel, error) {
	a, err := h.api(ctx, id)
	if err != nil {
		return netrec.Tunnel{}, err
	}
	return a.Tunnel(ctx)
}

// MicroVMs reads the host's microVMs.
func (h *NetHosts) MicroVMs(ctx context.Context, id string) ([]netrec.MicroVM, error) {
	a, err := h.api(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.MicroVMs(ctx)
}

// PutNetwork declares the network on the host.
func (h *NetHosts) PutNetwork(ctx context.Context, id, name string, n netrec.NetworkState) error {
	a, err := h.api(ctx, id)
	if err != nil {
		return err
	}
	return a.PutNetwork(ctx, name, n)
}

// Networks lists the names of the networks the host holds.
func (h *NetHosts) Networks(ctx context.Context, id string) ([]string, error) {
	a, err := h.api(ctx, id)
	if err != nil {
		return nil, err
	}
	return a.Networks(ctx)
}

// DeleteNetwork removes the network from the host.
func (h *NetHosts) DeleteNetwork(ctx context.Context, id, name string) error {
	a, err := h.api(ctx, id)
	if err != nil {
		return err
	}
	return a.DeleteNetwork(ctx, name)
}

// PutPeers replaces the host's peers.
func (h *NetHosts) PutPeers(ctx context.Context, id string, peers []netrec.Peer) error {
	a, err := h.api(ctx, id)
	if err != nil {
		return err
	}
	return a.PutPeers(ctx, peers)
}

// hostNet is NetAPI over lib/hostapi.
type hostNet struct{ api *hostapi.Client }

// DialNetAPI returns the dial func for NewNetHosts that reaches a host at its recorded address
// through hc, which carries orch's client identity for the host API.
func DialNetAPI(hc *http.Client) func(addr string) NetAPI {
	return func(addr string) NetAPI {
		return hostNet{api: hostapi.NewClient((&url.URL{Scheme: "https", Host: addr}).String(), hc)}
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
		out[i] = netrec.MicroVM{Name: m.Name, Network: m.Network, PodPrefix: m.PodPrefix, Addresses: m.Addresses}
	}
	return out, nil
}

func (n hostNet) PutNetwork(ctx context.Context, name string, s netrec.NetworkState) error {
	_, err := n.api.PutNetwork(ctx, name, hostapi.NetworkRequest{Prefix: s.Prefix, APIEndpoint: s.APIEndpoint, RemoteMembers: s.RemoteMembers})
	return err
}

func (n hostNet) PutPeers(ctx context.Context, peers []netrec.Peer) error {
	out := make([]hostapi.Peer, len(peers))
	for i, p := range peers {
		out[i] = hostapi.Peer{Name: p.Name, PublicKey: p.PublicKey, Endpoint: p.Endpoint, Prefixes: p.Prefixes}
	}
	_, err := n.api.SetPeers(ctx, out)
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
