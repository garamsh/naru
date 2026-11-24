package exposures_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/exposures/exposurestest"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// typedEvents is an exposures.Log that keeps the type of each event it was given.
type typedEvents struct {
	mu  sync.Mutex
	got []string
}

func (e *typedEvents) AppendEvent(_ context.Context, ev orchapi.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.got = append(e.got, ev.Type)
	return nil
}

func (e *typedEvents) types() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.got...)
}

// proxyFixture has the pools lan and edge, both of network a; a proxy exposure draws from either.
func proxyFixture(t *testing.T) (*exposures.Service, *exposurestest.Store, *typedEvents) {
	t.Helper()
	st := exposurestest.NewStore()
	ev := &typedEvents{}
	s := exposures.New(st, bothNetworks, exposurestest.Members{"a": {"10.1.0.5"}}, exposurestest.Profile{}, ev, &exposurestest.Clock{T: time.Unix(1000, 0)})
	_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	_, err = s.ApplyPool(t.Context(), "op", "edge", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.80/30"}, Networks: []string{"a"}})
	require.NoError(t, err)
	return s, st, ev
}

func proxySpec(ports ...int) orchapi.ProxyExposureSpec {
	return orchapi.ProxyExposureSpec{Pool: "edge", Ports: ports}
}

func TestAProxyExposureTakesItsPortsAsTCPToTheSameNumbers(t *testing.T) {
	s, st, ev := proxyFixture(t)
	e, err := s.ApplyProxyExposure(t.Context(), "op", "web", proxySpec(443, 80))
	require.NoError(t, err)
	assert.Equal(t, orchapi.ProxyExposure{ID: "web", Pool: "edge", Address: "192.168.1.80", Ports: []int{443, 80}, Phase: orchapi.ExposurePending}, e)
	r, err := st.GetExposure(t.Context(), "", "web")
	require.NoError(t, err)
	assert.Empty(t, r.Network)
	assert.Equal(t, orchapi.OwnerOperator, r.Owner)
	assert.Empty(t, r.Backends)
	assert.Equal(t, []orchapi.ExposurePort{{Protocol: wire.ProtocolTCP, Port: 443, BackendPort: 443}, {Protocol: wire.ProtocolTCP, Port: 80, BackendPort: 80}}, r.Ports)
	assert.Contains(t, ev.types(), exposures.EventProxyExposureApplied)
	assert.NotContains(t, ev.types(), exposures.EventExposureApplied)
}

func TestAProxyExposuresPortsAre443AndOptionally80(t *testing.T) {
	s, _, _ := proxyFixture(t)
	for name, ports := range map[string][]int{
		"none":       nil,
		"80 alone":   {80},
		"another":    {443, 8443},
		"443 twice":  {443, 443},
		"80 twice":   {443, 80, 80},
		"a negative": {443, -1},
	} {
		_, err := s.ApplyProxyExposure(t.Context(), "op", "web", proxySpec(ports...))
		assert.ErrorIs(t, err, inventory.ErrInvalid, name)
	}
	_, err := s.ApplyProxyExposure(t.Context(), "op", "web", proxySpec(443))
	require.NoError(t, err, "control: 443 alone")
}

func TestAProxyExposureDrawsFromAnyPoolThatExists(t *testing.T) {
	s, _, _ := proxyFixture(t)
	_, err := s.ApplyProxyExposure(t.Context(), "op", "web", orchapi.ProxyExposureSpec{Pool: "nope", Ports: []int{443}})
	require.ErrorIs(t, err, exposures.ErrForbidden, "a missing pool")
	e, err := s.ApplyProxyExposure(t.Context(), "op", "web", orchapi.ProxyExposureSpec{Pool: "lan", Ports: []int{443}})
	require.NoError(t, err, "a pool that lists only network a")
	require.Equal(t, "192.168.0.80", e.Address)
	_, err = s.ApplyProxyExposure(t.Context(), "op", "Web!", proxySpec(443))
	require.ErrorIs(t, err, inventory.ErrInvalid, "an id that is not valid")
}

func TestAProxyExposureSharesItsPoolsAddressesWithTheNetworks(t *testing.T) {
	s, _, _ := proxyFixture(t)
	ctx := t.Context()
	sp := spec("a", "10.1.0.5")
	sp.Pool = "edge"
	n, err := s.ApplyExposure(ctx, "op", "a", "web", sp)
	require.NoError(t, err)
	p, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err, "a proxy exposure's id is not a network's")
	assert.NotEqual(t, n.Address, p.Address, "one address space")

	req := proxySpec(443)
	req.RequestedAddress = n.Address
	_, err = s.ApplyProxyExposure(ctx, "op", "other", req)
	require.ErrorIs(t, err, inventory.ErrConflict, "the address a network's exposure holds")

	all, err := s.AllExposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the networks' listing holds no proxy exposure")
	assert.Equal(t, "a", all[0].Network)
	proxies, err := s.ProxyExposures(ctx)
	require.NoError(t, err)
	require.Len(t, proxies, 1, "nor the proxies' a network's")
	assert.Equal(t, p.Address, proxies[0].Address)
}

func TestAProxyExposureKeepsItsPoolAndAddressAndChangesItsPorts(t *testing.T) {
	s, _, _ := proxyFixture(t)
	ctx := t.Context()
	e, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443, 80))
	require.NoError(t, err)
	got, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err)
	assert.Equal(t, e.Address, got.Address)
	assert.Equal(t, []int{443}, got.Ports)
	got, err = s.ProxyExposure(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, []int{443}, got.Ports, "stored")

	_, err = s.ApplyPool(ctx, "op", "edge2", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.2.0/30"}, Networks: []string{"b"}})
	require.NoError(t, err)
	_, err = s.ApplyProxyExposure(ctx, "op", "web", orchapi.ProxyExposureSpec{Pool: "edge2", Ports: []int{443}})
	require.ErrorIs(t, err, inventory.ErrConflict, "another pool")
	req := proxySpec(443)
	req.RequestedAddress = "192.168.1.83"
	_, err = s.ApplyProxyExposure(ctx, "op", "web", req)
	require.ErrorIs(t, err, inventory.ErrConflict, "another address")
}

func TestAPoolKeepsTheAddressAProxyExposureHoldsWhateverItsNetworks(t *testing.T) {
	s, _, _ := proxyFixture(t)
	ctx := t.Context()
	_, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err)
	_, err = s.ApplyPool(ctx, "op", "edge", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.82/31"}, Networks: []string{"a"}})
	require.ErrorIs(t, err, inventory.ErrConflict, "its address would leave the pool")
	_, err = s.ApplyPool(ctx, "op", "edge", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.80/30"}, Networks: []string{"b"}})
	require.NoError(t, err, "a proxy exposure is of no network, so network a may leave")
	require.ErrorIs(t, s.DeletePool(ctx, "op", "edge"), inventory.ErrConflict, "nor is the pool deleted under it")
}

func TestThereAreAtMostSixtyFourProxyExposures(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.ApplyPool(t.Context(), "op", "big", orchapi.AddressPoolSpec{CIDRs: []string{"172.16.0.0/24"}, Networks: []string{"a"}})
	require.NoError(t, err)
	for i := range exposures.MaxProxyExposures {
		_, err := s.ApplyProxyExposure(t.Context(), "op", fmt.Sprintf("p%d", i), orchapi.ProxyExposureSpec{Pool: "big", Ports: []int{443}})
		require.NoError(t, err)
	}
	_, err = s.ApplyProxyExposure(t.Context(), "op", "over", orchapi.ProxyExposureSpec{Pool: "big", Ports: []int{443}})
	require.ErrorIs(t, err, inventory.ErrConflict)
	sp := spec("a", "10.1.0.5")
	_, err = s.ApplyExposure(t.Context(), "op", "a", "web", sp)
	require.NoError(t, err, "control: a network's limit is its own")
}

func TestADeletedProxyExposureIsReleasedAndHiddenAndADeleteOfNoneIsNoError(t *testing.T) {
	s, st, ev := proxyFixture(t)
	ctx := t.Context()
	_, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err)
	require.NoError(t, s.DeleteProxyExposure(ctx, "op", "web"))
	r, err := st.GetExposure(ctx, "", "web")
	require.NoError(t, err)
	assert.Equal(t, exposures.PhaseReleasing, r.Phase, "the address stays claimed until the reconciler frees it")
	_, err = s.ProxyExposure(ctx, "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	_, err = s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.ErrorIs(t, err, inventory.ErrConflict, "it is applied again once it is gone")
	assert.Contains(t, ev.types(), exposures.EventProxyExposureDeleted)
	require.NoError(t, s.DeleteProxyExposure(ctx, "op", "web"))
	require.NoError(t, s.DeleteProxyExposure(ctx, "op", "none"))
	require.ErrorIs(t, s.DeleteProxyExposure(ctx, "op", "Web!"), inventory.ErrInvalid)
}

// proxyHosts are hosts with a LAN interface, a healthy proxy and no microVM.
func proxyHosts(ids ...string) []exposures.Host {
	var out []exposures.Host
	for _, id := range ids {
		out = append(out, exposures.Host{ID: id, LAN: "eth0", Proxy: true})
	}
	return out
}

func TestSpreadProxiesTakesOnlyAHostThatAnsweredWithALANAndAHealthyProxy(t *testing.T) {
	hosts := []exposures.Host{
		{ID: "nolan", Proxy: true},
		{ID: "silent", LAN: "eth0", Proxy: true, Silent: true},
		{ID: "noproxy", LAN: "eth0", Networks: []string{"a"}, Members: defaultMembers},
	}
	assert.Empty(t, exposures.SpreadProxies([]string{"192.168.1.80"}, nil, hosts), "no candidate: the address is left out")
	hosts = append(hosts, proxyHosts("h1")...)
	assert.Equal(t, map[string]string{"192.168.1.80": "h1"}, exposures.SpreadProxies([]string{"192.168.1.80"}, nil, hosts))

	_, ok := exposures.Choose("192.168.1.80", "a", []string{"10.1.0.5"}, proxyHosts("h1", "h2"))
	assert.False(t, ok, "a proxy is no candidate for a network's exposure")
}

func TestSpreadProxiesPutsEachAddressOnAHostOfItsOwnWhileThereAreHostsEnough(t *testing.T) {
	for i := range 64 {
		pair := []string{fmt.Sprintf("192.168.%d.80", i), fmt.Sprintf("192.168.%d.81", i)}
		got := exposures.SpreadProxies(pair, nil, proxyHosts("h1", "h2"))
		require.Len(t, got, 2)
		assert.NotEqual(t, got[pair[0]], got[pair[1]], "%v: two addresses, two hosts", pair)
		again := exposures.SpreadProxies([]string{pair[1], pair[0]}, nil, proxyHosts("h2", "h1"))
		assert.Equal(t, got, again, "%v: the order of the addresses and of the hosts does not matter", pair)

		three := exposures.SpreadProxies(append(pair, fmt.Sprintf("192.168.%d.82", i)), nil, proxyHosts("h1", "h2"))
		assert.ElementsMatch(t, []string{"h1", "h2"}, distinct(three), "%v: a third address goes on a host that holds one, and no host is idle", pair)

		one := exposures.SpreadProxies(pair, nil, proxyHosts("h1"))
		assert.Equal(t, map[string]string{pair[0]: "h1", pair[1]: "h1"}, one, "control: with one candidate both are on it")
	}
}

func TestAProxyExposureBeingReleasedTakesNoHostFromTheOthers(t *testing.T) {
	// Two addresses whose highest-ranked host is the same: spread, the second would go to the
	// other host while the first holds its own.
	hosts := proxyHosts("p1", "p2")
	first, second := "192.168.1.80", ""
	for _, a := range []string{"192.168.1.81", "192.168.1.82", "192.168.1.83"} {
		if exposures.SpreadProxies([]string{a}, nil, hosts)[a] == exposures.SpreadProxies([]string{first}, nil, hosts)[first] {
			second = a
			break
		}
	}
	require.NotEmpty(t, second, "two addresses of the pool rank the same host first")
	top := exposures.SpreadProxies([]string{second}, nil, hosts)[second]
	spread := exposures.SpreadProxies([]string{first, second}, nil, hosts)
	require.NotEqual(t, top, spread[second], "control: placed together, the second goes to the other host")

	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	for id, a := range map[string]string{"first": first, "second": second} {
		req := proxySpec(443)
		req.RequestedAddress = a
		_, err := s.ApplyProxyExposure(ctx, "op", id, req)
		require.NoError(t, err)
	}
	require.NoError(t, s.DeleteProxyExposure(ctx, "op", "first"))
	h := newHosts(hosts...)
	require.NoError(t, reconciler(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)}).Reconcile(ctx))
	assert.Equal(t, []string{top}, h.holders(second), "the released one is not placed, so the other takes the host it ranks first")
}

func TestSpreadProxiesKeepsAnAddressOnItsSilentHolder(t *testing.T) {
	// Without kept, each address of the pair goes to a host of its own; kept on a silent holder, which
	// is no candidate, the other goes to the other host, whichever its hash prefers.
	for i := range 16 {
		pair := []string{fmt.Sprintf("192.168.%d.80", i), fmt.Sprintf("192.168.%d.81", i)}
		free := exposures.SpreadProxies(pair, nil, proxyHosts("h1", "h2"))
		hosts := proxyHosts("h1", "h2")
		other := map[string]string{"h1": "h2", "h2": "h1"}
		kept := other[free[pair[1]]] // the host the second address would not get
		for j := range hosts {
			hosts[j].Silent = hosts[j].ID == kept
		}
		got := exposures.SpreadProxies(pair, map[string]string{pair[1]: kept}, hosts)
		assert.Equal(t, kept, got[pair[1]], "%v: the kept address stays on its silent holder", pair)
		assert.Equal(t, other[kept], got[pair[0]], "%v: the other goes to the host that holds none", pair)
	}
}

func distinct(m map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range m {
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func TestTwoProxyExposuresAreHeldByTwoHostsAndBothMoveToTheOneLeftAndOneMovesBack(t *testing.T) {
	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	a, err := s.ApplyProxyExposure(ctx, "op", "a", proxySpec(443))
	require.NoError(t, err)
	b, err := s.ApplyProxyExposure(ctx, "op", "b", proxySpec(443))
	require.NoError(t, err)
	h := newHosts(proxyHosts("p1", "p2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(ctx))
	ha, hb := h.holders(a.Address), h.holders(b.Address)
	require.Len(t, ha, 1)
	require.Len(t, hb, 1)
	require.NotEqual(t, ha, hb, "each on a host of its own")

	lost := ha[0]
	h.lose(lost)
	clk.Advance(lease + exposures.Skew)
	require.NoError(t, r.Reconcile(ctx))
	for _, id := range []string{"a", "b"} {
		rec, err := st.GetExposure(ctx, "", id)
		require.NoError(t, err)
		assert.Equal(t, hb[0], rec.Host, "%s: after the lease and the skew both are on the host left", id)
		assert.Contains(t, h.holders(rec.Address), hb[0])
	}
	h.mu.Lock()
	delete(h.held, lost) // its leases ran out while it was away
	h.mu.Unlock()

	h.mu.Lock()
	h.hosts = proxyHosts("p1", "p2")
	h.mu.Unlock()
	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(ctx))
	require.NoError(t, r.Reconcile(ctx), "the first pass frees it from its holder, which confirms; the second leases it")
	ra, err := st.GetExposure(ctx, "", "a")
	require.NoError(t, err)
	rb, err := st.GetExposure(ctx, "", "b")
	require.NoError(t, err)
	assert.NotEqual(t, ra.Host, rb.Host, "the host that came back takes one back")
	assert.Equal(t, []string{ra.Host}, h.holders(a.Address))
	assert.Equal(t, []string{rb.Host}, h.holders(b.Address))
}

func TestASilentProxyHolderKeepsItsAddressAndTheOtherGoesElsewhere(t *testing.T) {
	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	a, err := s.ApplyProxyExposure(ctx, "op", "a", proxySpec(443))
	require.NoError(t, err)
	h := newHosts(proxyHosts("p1", "p2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(ctx))
	holder := h.holders(a.Address)[0]
	h.silence(holder, true)
	b, err := s.ApplyProxyExposure(ctx, "op", "b", proxySpec(443))
	require.NoError(t, err)
	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(ctx))
	assert.Equal(t, []string{holder}, h.holders(a.Address), "slow, not known gone: renewed where it is")
	other := h.holders(b.Address)
	require.Len(t, other, 1)
	assert.NotEqual(t, holder, other[0], "a silent host is no candidate for a new one")
}

func TestTheLeaderLeasesAProxyExposureToAProxyHostWithNoBackendsAndWithoutANetwork(t *testing.T) {
	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	e, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443, 80))
	require.NoError(t, err)
	h := newHosts(append(lanHosts("vm"), proxyHosts("p1")...)...)
	var mu sync.Mutex
	var got exposures.Lease
	h.onPut = func(_ context.Context, _, _ string, l exposures.Lease) error {
		mu.Lock()
		got = l
		mu.Unlock()
		return nil
	}
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	// No network is live: a proxy exposure is of none, so it is not taken for one whose network is gone.
	require.NoError(t, reconcilerOn(st, h, clk, exposurestest.Networks{}).Reconcile(ctx))
	assert.Equal(t, []string{"p1"}, h.holders(e.Address), "the host that runs a proxy")
	mu.Lock()
	assert.Equal(t, exposures.Lease{
		ToProxy: true, Seconds: exposures.LeaseSeconds,
		Ports: []orchapi.ExposurePort{{Protocol: wire.ProtocolTCP, Port: 443, BackendPort: 443}, {Protocol: wire.ProtocolTCP, Port: 80, BackendPort: 80}},
	}, got)
	mu.Unlock()
	r, err := st.GetExposure(ctx, "", "web")
	require.NoError(t, err)
	assert.Equal(t, "p1", r.Host)
	assert.Equal(t, orchapi.ExposureAnnounced, r.Phase)
}

func TestAProxyExposureMovesWhenItsHostsProxyFailsAndWaitsWithoutACandidate(t *testing.T) {
	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	e, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err)
	h := newHosts(proxyHosts("p1", "p2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(ctx))
	old := h.holders(e.Address)
	require.Len(t, old, 1)

	h.mu.Lock()
	for i := range h.hosts {
		h.hosts[i].Proxy = h.hosts[i].ID != old[0]
	}
	h.mu.Unlock()
	require.NoError(t, r.Reconcile(ctx))
	now := h.holders(e.Address)
	require.Len(t, now, 1, "the old host confirmed it removed it, so it moves at once")
	assert.NotEqual(t, old, now)

	h.mu.Lock()
	for i := range h.hosts {
		h.hosts[i].Proxy = false
	}
	h.mu.Unlock()
	require.NoError(t, r.Reconcile(ctx))
	rec, err := st.GetExposure(ctx, "", "web")
	require.NoError(t, err)
	assert.Equal(t, orchapi.ExposurePending, rec.Phase, "no proxy answers: no host holds it")
	assert.Empty(t, h.holders(e.Address))
}

func TestAReleasedProxyExposureLeavesItsHostAndTheStore(t *testing.T) {
	s, st, _ := proxyFixture(t)
	ctx := t.Context()
	e, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err)
	h := newHosts(proxyHosts("p1")...)
	r := reconciler(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)})
	require.NoError(t, r.Reconcile(ctx))
	require.Len(t, h.holders(e.Address), 1)
	require.NoError(t, s.DeleteProxyExposure(ctx, "op", "web"))
	require.NoError(t, r.Reconcile(ctx))
	assert.Empty(t, h.holders(e.Address))
	_, err = st.GetExposure(ctx, "", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	again, err := s.ApplyProxyExposure(ctx, "op", "web", proxySpec(443))
	require.NoError(t, err, "its id and address are free again")
	assert.Equal(t, e.Address, again.Address)
}
