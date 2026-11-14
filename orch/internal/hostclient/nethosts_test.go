package hostclient_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/netrec"
)

type invFake struct{ hosts []orchapi.Host }

func (i *invFake) Hosts(context.Context) ([]orchapi.Host, error) { return i.hosts, nil }

// api records which address it was dialed for.
type api struct {
	addr  string
	peers map[string][]netrec.Peer
	nets  map[string]netrec.NetworkState
}

func (a *api) Tunnel(context.Context) (netrec.Tunnel, error) {
	return netrec.Tunnel{PublicKey: "key@" + a.addr}, nil
}

func (a *api) MicroVMs(context.Context) ([]netrec.MicroVM, error) {
	return []netrec.MicroVM{{Name: "vm@" + a.addr}}, nil
}

func (a *api) PutNetwork(_ context.Context, name string, n netrec.NetworkState) error {
	a.nets[a.addr+"/"+name] = n
	return nil
}

func (a *api) Networks(context.Context) ([]string, error) { return []string{"net@" + a.addr}, nil }

func (a *api) DeleteNetwork(_ context.Context, name string) error {
	a.nets["deleted/"+a.addr+"/"+name] = netrec.NetworkState{}
	return nil
}

func (a *api) PutPeers(_ context.Context, p []netrec.Peer) error {
	a.peers[a.addr] = p
	return nil
}

func TestNetHostsReachEachHostAtItsRecordedAddress(t *testing.T) {
	inv := &invFake{hosts: []orchapi.Host{{ID: "h1", Address: "192.0.2.1:7443"}, {ID: "h2", Address: "192.0.2.2:7443"}}}
	peers, nets := map[string][]netrec.Peer{}, map[string]netrec.NetworkState{}
	h := hostclient.NewNetHosts(inv, func(addr string) hostclient.NetAPI { return &api{addr: addr, peers: peers, nets: nets} })

	list, err := h.List(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []netrec.Host{{ID: "h1", Address: "192.0.2.1:7443"}, {ID: "h2", Address: "192.0.2.2:7443"}}, list)

	tun, err := h.Tunnel(t.Context(), "h2")
	require.NoError(t, err)
	assert.Equal(t, "key@192.0.2.2:7443", tun.PublicKey)
	vms, err := h.MicroVMs(t.Context(), "h1")
	require.NoError(t, err)
	assert.Equal(t, "vm@192.0.2.1:7443", vms[0].Name)

	require.NoError(t, h.PutNetwork(t.Context(), "h1", "lab", netrec.NetworkState{Prefix: "10.9.0.0/24"}))
	require.NoError(t, h.PutPeers(t.Context(), "h2", []netrec.Peer{{Name: "h1"}}))
	assert.Equal(t, "10.9.0.0/24", nets["192.0.2.1:7443/lab"].Prefix)
	assert.Equal(t, []netrec.Peer{{Name: "h1"}}, peers["192.0.2.2:7443"])

	names, err := h.Networks(t.Context(), "h2")
	require.NoError(t, err)
	assert.Equal(t, []string{"net@192.0.2.2:7443"}, names)
	require.NoError(t, h.DeleteNetwork(t.Context(), "h1", "old"))
	assert.Contains(t, nets, "deleted/192.0.2.1:7443/old")

	_, err = h.Tunnel(t.Context(), "gone")
	require.Error(t, err, "a host the inventory no longer holds is refused, not dialed")
}
