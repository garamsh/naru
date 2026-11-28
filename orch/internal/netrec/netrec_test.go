package netrec_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/netrec"
)

// fakeHost is one host of the fake fleet.
type fakeHost struct {
	netrec.Host
	tunnel   *netrec.Tunnel // nil: the host gives no tunnel
	microVMs []netrec.MicroVM
	networks map[string]netrec.NetworkState
	peers    []netrec.Peer
	peered   bool
	mesh     string // the mesh address the host was told is its own
	pool     string // the mesh pool the host was told
}

type fakeHosts struct {
	mu    sync.Mutex
	hosts map[string]*fakeHost
}

func (f *fakeHosts) List(context.Context) ([]netrec.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []netrec.Host
	for _, h := range f.hosts {
		out = append(out, h.Host)
	}
	return out, nil
}

func (f *fakeHosts) Tunnel(_ context.Context, id string) (netrec.Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[id]
	if h.tunnel == nil {
		return netrec.Tunnel{}, errors.New("no tunnel on this host")
	}
	return *h.tunnel, nil
}

func (f *fakeHosts) MicroVMs(_ context.Context, id string) ([]netrec.MicroVM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]netrec.MicroVM(nil), f.hosts[id].microVMs...), nil
}

func (f *fakeHosts) PutNetwork(_ context.Context, id, name string, n netrec.NetworkState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[id]
	if h.networks == nil {
		h.networks = map[string]netrec.NetworkState{}
	}
	h.networks[name] = n
	return nil
}

func (f *fakeHosts) PutPeers(_ context.Context, id, mesh, pool string, p []netrec.Peer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[id]
	h.peers, h.peered, h.mesh, h.pool = p, true, mesh, pool
	return nil
}

func (f *fakeHosts) Networks(_ context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for name := range f.hosts[id].networks {
		out = append(out, name)
	}
	return out, nil
}

func (f *fakeHosts) DeleteNetwork(_ context.Context, id, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hosts[id].networks, name)
	return nil
}

type fakeNetworks []orchapi.Network

func (n fakeNetworks) List(context.Context) ([]orchapi.Network, error) { return n, nil }

func fleet() *fakeHosts {
	return &fakeHosts{hosts: map[string]*fakeHost{
		"h1": {
			Host:   netrec.Host{ID: "h1", Address: "192.0.2.1:7443"},
			tunnel: &netrec.Tunnel{PublicKey: "key1", ListenPort: 51820, MachinePrefixes: []string{"10.200.1.0/24"}},
			microVMs: []netrec.MicroVM{
				{Name: "a1", Network: "lab", Addresses: []string{"10.9.0.2"}},
				{Name: "b1", Network: "edge", Addresses: []string{"10.8.0.2"}},
			},
		},
		"h2": {
			Host:   netrec.Host{ID: "h2", Address: "192.0.2.2:7443"},
			tunnel: &netrec.Tunnel{PublicKey: "key2", ListenPort: 51821, MachinePrefixes: []string{"10.200.2.0/24"}},
			microVMs: []netrec.MicroVM{
				{Name: "a2", Network: "lab", Addresses: []string{"10.9.0.3"}},
				{Name: "b2", Network: "edge", Addresses: []string{"10.8.0.3"}},
				{Name: "loose", Addresses: []string{"10.200.2.9"}},
			},
		},
	}}
}

var twoNetworks = fakeNetworks{
	{Name: "edge", Prefix: "10.8.0.0/24", APIEndpoint: "10.8.0.2"},
	{Name: "lab", Prefix: "10.9.0.0/24"},
}

func newRec(h netrec.Hosts, n netrec.Networks, buf *bytes.Buffer) *netrec.Reconciler {
	return netrec.New(netrec.Config{Hosts: h, Networks: n, Log: slog.New(slog.NewTextHandler(buf, nil))})
}

func TestTwoHostsAndTwoNetworksEachGetTheOthersMembersAndPeers(t *testing.T) {
	f := fleet()
	require.NoError(t, newRec(f, twoNetworks, &bytes.Buffer{}).Reconcile(t.Context()))

	h1, h2 := f.hosts["h1"], f.hosts["h2"]
	assert.Equal(t, netrec.NetworkState{Prefix: "10.9.0.0/24", RemoteMembers: []string{"10.9.0.3/32"}}, h1.networks["lab"])
	assert.Equal(t, netrec.NetworkState{Prefix: "10.8.0.0/24", APIEndpoint: "10.8.0.2", RemoteMembers: []string{"10.8.0.3/32"}}, h1.networks["edge"])
	assert.Equal(t, netrec.NetworkState{Prefix: "10.9.0.0/24", RemoteMembers: []string{"10.9.0.2/32"}}, h2.networks["lab"])
	assert.Equal(t, netrec.NetworkState{Prefix: "10.8.0.0/24", APIEndpoint: "10.8.0.2", RemoteMembers: []string{"10.8.0.2/32"}}, h2.networks["edge"])

	assert.Equal(t, []netrec.Peer{{Name: "h2", PublicKey: "key2", Endpoint: "192.0.2.2:51821", Prefixes: []string{"10.200.2.0/24"}}}, h1.peers)
	assert.Equal(t, []netrec.Peer{{Name: "h1", PublicKey: "key1", Endpoint: "192.0.2.1:51820", Prefixes: []string{"10.200.1.0/24"}}}, h2.peers)
}

func TestEachHostIsToldItsOwnMeshAddressAndEveryPeersBesideItsPrefixes(t *testing.T) {
	f := fleet()
	f.hosts["h1"].MeshAddress = "10.99.0.1"
	f.hosts["h2"].MeshAddress = "10.99.0.2"
	require.NoError(t, newRec(f, twoNetworks, &bytes.Buffer{}).Reconcile(t.Context()))

	h1, h2 := f.hosts["h1"], f.hosts["h2"]
	assert.Equal(t, "10.99.0.1", h1.mesh, "h1 is told its own")
	assert.Equal(t, "10.99.0.2", h2.mesh)
	require.Len(t, h1.peers, 1)
	assert.Equal(t, "10.99.0.2", h1.peers[0].MeshAddress, "and its peer's")
	assert.Equal(t, []string{"10.200.2.0/24"}, h1.peers[0].Prefixes, "the mesh address is not one of the peer's prefixes")
	require.Len(t, h2.peers, 1)
	assert.Equal(t, "10.99.0.1", h2.peers[0].MeshAddress)

	f.hosts["h2"].MeshAddress = ""
	require.NoError(t, newRec(f, twoNetworks, &bytes.Buffer{}).Reconcile(t.Context()))
	assert.Empty(t, f.hosts["h1"].peers[0].MeshAddress, "the control: a host orch gave none is a peer with none")
	assert.Empty(t, f.hosts["h2"].mesh)
}

func TestAHostThatRunsNoProxyAdmitsExactlyTheProxyPeersAndFollowsThemComeAndGo(t *testing.T) {
	f := fleet()
	f.hosts["h1"].MeshAddress = "10.99.0.1"
	f.hosts["h2"].MeshAddress = "10.99.0.2"
	f.hosts["h3"] = &fakeHost{
		Host:   netrec.Host{ID: "h3", Address: "192.0.2.3:7443", MeshAddress: "10.99.0.3"},
		tunnel: &netrec.Tunnel{PublicKey: "key3", ListenPort: 51822, MachinePrefixes: []string{"10.200.3.0/24"}},
	}
	f.hosts["h1"].tunnel.Proxy, f.hosts["h1"].Proxy = true, true
	// proxies are the mesh addresses the Machine-only host h2 is told run a proxy.
	proxies := func() []string {
		t.Helper()
		require.NoError(t, newRec(f, twoNetworks, &bytes.Buffer{}).Reconcile(t.Context()))
		var out []string
		for _, p := range f.hosts["h2"].peers {
			if p.Proxy {
				out = append(out, p.MeshAddress)
			}
		}
		return out
	}
	assert.Equal(t, []string{"10.99.0.1"}, proxies(), "the one proxy host, and not h3")

	f.hosts["h3"].tunnel.Proxy = true
	assert.Equal(t, []string{"10.99.0.1"}, proxies(), "a host that reports a proxy without the operator's grant is no proxy")
	f.hosts["h3"].Proxy = true
	assert.ElementsMatch(t, []string{"10.99.0.1", "10.99.0.3"}, proxies(), "granted, a proxy host that comes is added")

	f.hosts["h1"].tunnel.Proxy = false
	assert.Equal(t, []string{"10.99.0.3"}, proxies(), "and one that goes is taken out, though it holds the grant")

	f.hosts["h3"].MeshAddress = ""
	assert.Empty(t, proxies(), "a proxy host with no mesh address is marked no proxy: it reaches no microVM")
}

func TestAMemberThatIsRemovedDisappearsFromTheOtherHosts(t *testing.T) {
	f := fleet()
	r := newRec(f, twoNetworks, &bytes.Buffer{})
	require.NoError(t, r.Reconcile(t.Context()))
	require.Contains(t, f.hosts["h1"].networks["lab"].RemoteMembers, "10.9.0.3/32", "control: the member is there first")

	f.hosts["h2"].microVMs = []netrec.MicroVM{{Name: "b2", Network: "edge", Addresses: []string{"10.8.0.3"}}}
	require.NoError(t, r.Reconcile(t.Context()))

	assert.Empty(t, f.hosts["h1"].networks["lab"].RemoteMembers)
	assert.Equal(t, []string{"10.8.0.3/32"}, f.hosts["h1"].networks["edge"].RemoteMembers, "the other network's member stays")
	assert.Equal(t, []string{"10.200.2.0/24"}, f.hosts["h1"].peers[0].Prefixes)
}

func TestANetworkOrchNoLongerKeepsIsRemovedFromEveryHost(t *testing.T) {
	f := fleet()
	r := newRec(f, twoNetworks, &bytes.Buffer{})
	require.NoError(t, r.Reconcile(t.Context()))
	for _, h := range []string{"h1", "h2"} {
		require.Contains(t, f.hosts[h].networks, "lab", "control: both hosts hold the network first")
	}

	only := newRec(f, fakeNetworks{twoNetworks[0]}, &bytes.Buffer{})
	require.NoError(t, only.Reconcile(t.Context()))
	for _, h := range []string{"h1", "h2"} {
		assert.NotContains(t, f.hosts[h].networks, "lab")
		assert.Contains(t, f.hosts[h].networks, "edge", "a network orch still keeps stays")
	}
}

func TestAHostWithNoTunnelIsSkippedAndLoggedWhileTheOthersConverge(t *testing.T) {
	f := fleet()
	f.hosts["h3"] = &fakeHost{Host: netrec.Host{ID: "h3", Address: "192.0.2.3:7443"}}
	var buf bytes.Buffer
	require.NoError(t, newRec(f, twoNetworks, &buf).Reconcile(t.Context()))

	assert.False(t, f.hosts["h3"].peered, "nothing is sent to the host that gave no tunnel")
	assert.Empty(t, f.hosts["h3"].networks)
	assert.Contains(t, buf.String(), "host skipped")
	assert.Contains(t, buf.String(), "h3")
	require.Len(t, f.hosts["h1"].peers, 1, "h3 is no peer of the others")
	assert.Equal(t, "h2", f.hosts["h1"].peers[0].Name)
	assert.Contains(t, f.hosts["h1"].networks, "lab")
	assert.Contains(t, f.hosts["h2"].networks, "edge")

	// The control: once the host answers it converges like the rest.
	f.hosts["h3"].tunnel = &netrec.Tunnel{PublicKey: "key3", ListenPort: 51822}
	require.NoError(t, newRec(f, twoNetworks, &buf).Reconcile(t.Context()))
	assert.True(t, f.hosts["h3"].peered)
	assert.Len(t, f.hosts["h1"].peers, 2)
}

func TestARunPassesUntilItsContextEnds(t *testing.T) {
	f := fleet()
	r := netrec.New(netrec.Config{Hosts: f, Networks: twoNetworks, Interval: time.Millisecond, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.hosts["h1"].peered
	}, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestEveryHostIsToldTheMeshPoolWithItsPeers(t *testing.T) {
	f := fleet()
	pool := netip.MustParsePrefix("10.99.0.0/24")
	r := netrec.New(netrec.Config{Hosts: f, Networks: twoNetworks, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		MeshPool: func(context.Context) (netip.Prefix, error) { return pool, nil }})
	require.NoError(t, r.Reconcile(t.Context()))
	for id, h := range f.hosts {
		assert.Equal(t, "10.99.0.0/24", h.pool, "%s drops every mesh source in it but a proxy's", id)
	}
	require.NoError(t, newRec(f, twoNetworks, &bytes.Buffer{}).Reconcile(t.Context()))
	for id, h := range f.hosts {
		assert.Empty(t, h.pool, "control: %s is told none when orch has none", id)
	}
	failing := netrec.New(netrec.Config{Hosts: f, Networks: twoNetworks, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		MeshPool: func(context.Context) (netip.Prefix, error) { return netip.Prefix{}, errors.New("etcd is away") }})
	assert.ErrorContains(t, failing.Reconcile(t.Context()), "mesh pool", "a pass that cannot read the pool puts no peers without it")
}
