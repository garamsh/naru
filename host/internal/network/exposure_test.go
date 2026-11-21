package network_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// clock is the time of the tests.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeLink is the LAN interface of the tests.
type fakeLink struct {
	mu        sync.Mutex
	addrs     map[netip.Addr]bool
	announced []netip.Addr
	addErr    error
}

func newLink(left ...netip.Addr) *fakeLink {
	l := &fakeLink{addrs: map[netip.Addr]bool{}}
	for _, a := range left {
		l.addrs[a] = true
	}
	return l
}

func (l *fakeLink) Add(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addErr != nil {
		return l.addErr
	}
	l.addrs[a] = true
	return nil
}

func (l *fakeLink) Remove(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.addrs, a)
	return nil
}

func (l *fakeLink) Held() ([]netip.Addr, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []netip.Addr
	for a := range l.addrs {
		out = append(out, a)
	}
	return out, nil
}

func (l *fakeLink) Announce(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.announced = append(l.announced, a)
	return nil
}

func (l *fakeLink) has(a netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addrs[a]
}

func (l *fakeLink) announcements() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.announced)
}

type exposureEnv struct {
	f     *fakeRunner
	m     *network.Manager
	x     *network.Exposer
	link  *fakeLink
	clock *clock
	state string
}

// startExposer is an Exposer over a Manager that holds the network "a" and has restored.
func startExposer(t *testing.T, state string, link *fakeLink, c *clock) *exposureEnv {
	t.Helper()
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, state)
	m.Base.LAN = "lan0"
	m.Now = c.now
	require.NoError(t, m.Load(context.Background()))
	if _, ok := m.Network("a"); !ok {
		require.NoError(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16"), Remote: []netip.Prefix{pfx("10.0.2.7/32")}}))
	}
	x := &network.Exposer{Manager: m, Link: link, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: c.now}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, x.Restore(ctx))
	return &exposureEnv{f: f, m: m, x: x, link: link, clock: c, state: state}
}

func web(proxy bool) network.Exposure {
	return network.Exposure{
		Address: addr("192.168.0.80"), Network: "a", Proxy: proxy,
		Ports:    []network.ExposurePort{{Protocol: "tcp", Port: 80, NodePort: 30080}, {Protocol: "udp", Port: 53, NodePort: 30053}},
		Backends: []netip.Addr{addr("10.8.0.5"), addr("10.8.0.6")},
	}
}

func TestRuleset_ExposureForwardsExactlyItsFlow(t *testing.T) {
	c := twoNetworks()
	c.LAN = "lan0"
	c.Exposures = []network.Exposure{web(false)}
	golden(t, "exposure.nft", network.Ruleset(c))
}

func TestRuleset_ProxiedTCPPortIsNotForwarded(t *testing.T) {
	c := twoNetworks()
	c.LAN = "lan0"
	e := web(true)
	e.Ports = []network.ExposurePort{{Protocol: "tcp", Port: 443, NodePort: 30443}}
	c.Exposures = []network.Exposure{e}
	rules := network.Ruleset(c)
	assert.NotContains(t, rules, "dport 443", "the proxy answers the port, no DNAT does")
	assert.NotContains(t, rules, "dport 30443")
	assert.NotContains(t, rules, "exposure_dnat")

	e.Proxy = false
	c.Exposures = []network.Exposure{e}
	assert.Contains(t, network.Ruleset(c), "tcp dport 443 dnat", "control: the same exposure without the proxy is forwarded")
}

func TestRuleset_NoLANForwardsNothing(t *testing.T) {
	c := twoNetworks()
	c.Exposures = []network.Exposure{web(false)}
	rules := network.Ruleset(c)
	assert.NotContains(t, rules, "dnat")
	c.LAN = "lan0"
	assert.Contains(t, network.Ruleset(c), "dnat", "control: with a LAN interface it is forwarded")
}

func TestExposer_PutAddsTheAddressAndAnnouncesFor30Seconds(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.True(t, link.has(addr("192.168.0.80")))
	assert.Contains(t, e.f.last(), "dnat ip to numgen random mod 2 map { 0 : 10.8.0.5 . 30080, 1 : 10.8.0.6 . 30080 }")

	var counts []int
	for i := 0; i < 8; i++ {
		require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second), "orch renews every 5 s")
		require.NoError(t, e.x.Tick(ctx))
		counts = append(counts, link.announcements())
		c.advance(5 * time.Second)
	}
	assert.Equal(t, []int{1, 2, 3, 4, 5, 6, 7, 7}, counts, "once on acquiring, then every 5 s until 30 s after")
}

func TestExposer_ALeaseThatIsNotRenewedIsRemoved(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	a := addr("192.168.0.80")
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))

	c.advance(10 * time.Second)
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	c.advance(14 * time.Second) // 24 s: 1 s before the renewed deadline
	require.NoError(t, e.x.Tick(ctx))
	assert.True(t, link.has(a), "control: a lease within its deadline is kept")
	assert.Len(t, e.x.Exposures(), 1)

	c.advance(time.Second)
	require.NoError(t, e.x.Tick(ctx))
	assert.False(t, link.has(a), "the address is taken off by itself")
	assert.Empty(t, e.x.Exposures())
	assert.NotContains(t, e.f.last(), "dnat", "and so are its rules")
}

func TestExposer_ARenewalRunsNoNft(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, filepath.Join(t.TempDir(), "networks.json"), link, c)
	ctx := context.Background()
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	before := len(e.f.calls)
	c.advance(5 * time.Second)
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.Len(t, e.f.calls, before, "the table is not replaced by a renewal")

	changed := web(false)
	changed.Backends = changed.Backends[:1]
	require.NoError(t, e.x.Put(ctx, changed, 15*time.Second))
	assert.Len(t, e.f.calls, before+1, "control: a change of backends is")
}

func TestExposer_DeleteReleasesAtOnce(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	require.NoError(t, e.x.Delete(ctx, addr("192.168.0.80")))
	assert.False(t, link.has(addr("192.168.0.80")))
	assert.NotContains(t, e.f.last(), "dnat")
	require.NoError(t, e.x.Delete(ctx, addr("192.168.0.80")), "an absent lease is not an error")
}

func TestExposer_RefusesWhatTheHostMustNotServe(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	require.NoError(t, e.m.Attach(ctx, network.Member{Network: "a", Address: addr("10.0.0.3")}))

	for name, tc := range map[string]struct {
		change func(*network.Exposure)
		want   error
	}{
		"an address of the network":     {func(x *network.Exposure) { x.Address = addr("10.8.1.1") }, network.ErrReserved},
		"a member's address":            {func(x *network.Exposure) { x.Address = addr("10.0.0.3") }, network.ErrReserved},
		"a remote member's address":     {func(x *network.Exposure) { x.Address = addr("10.0.2.7") }, network.ErrReserved},
		"the management plane":          {func(x *network.Exposure) { x.Address = addr("172.16.0.9") }, network.ErrReserved},
		"a backend outside the network": {func(x *network.Exposure) { x.Backends = []netip.Addr{addr("10.8.0.5"), addr("10.9.0.5")} }, network.ErrBackend},
		"a backend that is the LAN's":   {func(x *network.Exposure) { x.Backends = []netip.Addr{addr("192.168.0.1")} }, network.ErrBackend},
		"an unknown network":            {func(x *network.Exposure) { x.Network = "nope" }, network.ErrUnknownNetwork},
	} {
		x := web(false)
		tc.change(&x)
		err := e.x.Put(ctx, x, 15*time.Second)
		assert.ErrorIs(t, err, tc.want, name)
		assert.False(t, link.has(x.Address), "%s: the address was not taken", name)
	}
	assert.Empty(t, e.x.Exposures())

	ok := web(false)
	ok.Backends = []netip.Addr{addr("10.0.0.3"), addr("10.0.2.7"), addr("10.8.0.5")}
	require.NoError(t, e.x.Put(ctx, ok, 15*time.Second), "control: an attached member, a remote member and an address of the prefix are backends")
}

func TestExposer_AHostWithoutALANInterfaceRefuses(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, "")
	require.NoError(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))
	x := &network.Exposer{Manager: m, Link: newLink(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.NoError(t, x.Restore(context.Background()))
	assert.ErrorIs(t, x.Put(context.Background(), web(false), time.Second), network.ErrNoLAN)
	m.Base.LAN = "lan0"
	assert.NoError(t, x.Put(context.Background(), web(false), time.Second), "control: with one it is taken")
}

func TestExposer_AnAddressTheLinkRefusesLeavesNoRules(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	link.addErr = errors.New("no such device")
	require.Error(t, e.x.Put(context.Background(), web(false), 15*time.Second))
	assert.Empty(t, e.x.Exposures())
	assert.NotContains(t, e.f.last(), "dnat", "the host forwards no address it does not own")
}

func TestExposer_ARestartRestoresTheLeaseAndDropsWhatRanOut(t *testing.T) {
	state := filepath.Join(t.TempDir(), "networks.json")
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, state, link, c)
	ctx := context.Background()
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	second := web(false)
	second.Address = addr("192.168.0.81")
	require.NoError(t, e.x.Put(ctx, second, 40*time.Second))
	require.Equal(t, 4, strings.Count(e.f.last(), "dnat ip to"))

	// hostd is restarted 20 s later; the first lease ran out meanwhile and the address stays on the
	// interface, as the kernel keeps it.
	c.advance(20 * time.Second)
	again := startExposer(t, state, link, c)
	assert.Equal(t, []netip.Addr{addr("192.168.0.81")}, addresses(again.x.Exposures()), "the lease that ran out is not restored")
	assert.False(t, link.has(addr("192.168.0.80")), "and its address is taken off before the API serves")
	assert.True(t, link.has(addr("192.168.0.81")), "control: the other is held")
	assert.Equal(t, 2, strings.Count(again.f.table, "dnat ip to"), "the table holds the lease that is left")
	before := link.announcements()
	require.NoError(t, again.x.Tick(ctx))
	assert.Equal(t, before+1, link.announcements(), "it is announced again")
}

func TestExposer_ARestartTakesOffAnAddressNoLeaseHolds(t *testing.T) {
	stray := addr("192.168.0.99")
	link := newLink(stray)
	startExposer(t, filepath.Join(t.TempDir(), "networks.json"), link, &clock{t: t0})
	assert.False(t, link.has(stray))
}

func TestManager_RemovingANetworkDropsItsExposures(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	require.NoError(t, e.x.Put(context.Background(), web(false), 15*time.Second))
	require.NoError(t, e.m.Remove(context.Background(), "a"))
	assert.Empty(t, e.m.Exposures())
	require.NoError(t, e.x.Tick(context.Background()))
	assert.False(t, link.has(addr("192.168.0.80")))
}

func addresses(in []network.Exposure) []netip.Addr {
	var out []netip.Addr
	for _, e := range in {
		out = append(out, e.Address)
	}
	return out
}
