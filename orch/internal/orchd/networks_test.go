package orchd_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/netrec"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// netHosts is two hosts that answer the network reconciler and keep what they are told.
type netHosts struct {
	mu   sync.Mutex
	nets map[string]map[string]netrec.NetworkState
}

func (n *netHosts) List(context.Context) ([]netrec.Host, error) {
	return []netrec.Host{{ID: "h1", Address: "192.0.2.1:7443"}, {ID: "h2", Address: "192.0.2.2:7443"}}, nil
}

func (n *netHosts) Tunnel(_ context.Context, id string) (netrec.Tunnel, error) {
	return netrec.Tunnel{PublicKey: "key-" + id, ListenPort: 51820}, nil
}

func (n *netHosts) MicroVMs(_ context.Context, id string) ([]netrec.MicroVM, error) {
	if id == "h2" {
		return []netrec.MicroVM{{Name: "m", Network: "lab", Addresses: []string{"10.9.0.5"}}}, nil
	}
	return nil, nil
}

func (n *netHosts) PutNetwork(_ context.Context, id, name string, st netrec.NetworkState) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.nets[id] == nil {
		n.nets[id] = map[string]netrec.NetworkState{}
	}
	n.nets[id][name] = st
	return nil
}

func (n *netHosts) Networks(_ context.Context, id string) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for name := range n.nets[id] {
		out = append(out, name)
	}
	return out, nil
}

func (n *netHosts) DeleteNetwork(_ context.Context, id, name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.nets[id], name)
	return nil
}

func (n *netHosts) PutPeers(context.Context, string, []netrec.Peer) error { return nil }

func (n *netHosts) told(id, name string) (netrec.NetworkState, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	st, ok := n.nets[id][name]
	return st, ok
}

func TestNetworksAPIAndTheLeaderTellTheHostsTheirNetworks(t *testing.T) {
	nh := &netHosts{nets: map[string]map[string]netrec.NetworkState{}}
	fake := orchtest.NewFakeHosts("h1", "h2")
	o := startOne(t, orchtest.Options{
		NewHosts:        func(inventory.Service) groups.Hosts { return fake },
		NewNetworkHosts: func(inventory.Service) netrec.Hosts { return nh },
	})
	spec := orchapi.NetworkSpec{Prefix: "10.9.0.0/24"}

	// A reader reads and may not write; nobody unauthenticated does either.
	_, err := o.reader.ApplyNetwork(t.Context(), "lab", spec)
	require.Equal(t, 403, status(err))
	_, err = o.anon.Networks(t.Context())
	require.Equal(t, 401, status(err))
	_, err = o.op.Network(t.Context(), "lab")
	require.Equal(t, 404, status(err))
	_, err = o.op.ApplyNetwork(t.Context(), "lab", orchapi.NetworkSpec{Prefix: "10.9.0.1/24"})
	require.Equal(t, 400, status(err))

	n, err := o.op.ApplyNetwork(t.Context(), "lab", spec)
	require.NoError(t, err)
	require.Equal(t, orchapi.Network{Name: "lab", Prefix: "10.9.0.0/24"}, n)
	all, err := o.reader.Networks(t.Context())
	require.NoError(t, err)
	require.Equal(t, []orchapi.Network{n}, all)

	// The leader's reconciler tells each host the network and the members on the other host.
	require.Eventually(t, func() bool {
		st, ok := nh.told("h1", "lab")
		return ok && len(st.RemoteMembers) == 1 && st.RemoteMembers[0] == "10.9.0.5/32"
	}, 30*time.Second, 50*time.Millisecond)
	st, ok := nh.told("h2", "lab")
	require.True(t, ok)
	require.Empty(t, st.RemoteMembers)

	// A group that uses the network holds it: it cannot be deleted until the group is.
	g := webGroup()
	g.Template.Network, g.Template.PodPrefixBits = "lab", 26
	_, err = o.op.ApplyGroup(t.Context(), g)
	require.NoError(t, err)
	require.Equal(t, 409, status(o.op.DeleteNetwork(t.Context(), "lab")))

	zero := g
	zero.Count = 0
	_, err = o.op.ApplyGroup(t.Context(), zero)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		gr, err := o.op.Group(t.Context(), "web")
		if err != nil {
			return false
		}
		for _, m := range gr.Members {
			if m.Phase != orchapi.PhaseGone {
				return false
			}
		}
		return true
	}, 30*time.Second, 100*time.Millisecond)
	require.NoError(t, o.op.DeleteGroup(t.Context(), "web"))
	require.NoError(t, o.op.DeleteNetwork(t.Context(), "lab"))
	require.Equal(t, 404, status(o.op.DeleteNetwork(t.Context(), "lab")))
}
