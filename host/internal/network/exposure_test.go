package network_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
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
	// boot, when set, is the boot of the host; otherwise it is boot-1, up as long as the clock has run since t0.
	boot func() (string, time.Duration, error)
}

func (c *clock) bootInfo() (string, time.Duration, error) {
	if c.boot != nil {
		return c.boot()
	}
	return "boot-1", c.now().Sub(t0), nil
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
	// refused are the addresses Check refuses; valid is the time the kernel was given for each address.
	refused map[netip.Addr]bool
	valid   map[netip.Addr]time.Duration
	// failAdd are the addresses Add fails for.
	failAdd map[netip.Addr]bool
	// removeErr, when set, is the error of every Remove.
	removeErr error
	// takenByProbe are the addresses Probe reports as held by another host.
	takenByProbe map[netip.Addr]bool
	// lanPrefixes, when set, are the subnets Check requires the address to be in.
	lanPrefixes []netip.Prefix
}

func newLink(left ...netip.Addr) *fakeLink {
	l := &fakeLink{addrs: map[netip.Addr]bool{}, refused: map[netip.Addr]bool{}, valid: map[netip.Addr]time.Duration{}, failAdd: map[netip.Addr]bool{}, takenByProbe: map[netip.Addr]bool{}}
	for _, a := range left {
		l.addrs[a] = true
	}
	return l
}

func (l *fakeLink) Check(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refused[a] {
		return network.ErrHostAddress
	}
	if len(l.lanPrefixes) > 0 {
		for _, p := range l.lanPrefixes {
			if p.Contains(a) {
				return nil
			}
		}
		return network.ErrOutsideLAN
	}
	return nil
}

func (l *fakeLink) Probe(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.takenByProbe[a] {
		return network.ErrTaken
	}
	return nil
}

func (l *fakeLink) Add(a netip.Addr, valid time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.addErr != nil {
		return l.addErr
	}
	if l.failAdd[a] {
		return errors.New("no such device")
	}
	l.addrs[a] = true
	l.valid[a] = valid
	return nil
}

func (l *fakeLink) validFor(a netip.Addr) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.valid[a]
}

func (l *fakeLink) Remove(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.removeErr != nil {
		return l.removeErr
	}
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
// The link's LAN prefixes are set to 192.168.0.0/24, as the LAN interface of the tests.
func startExposer(t *testing.T, state string, link *fakeLink, c *clock) *exposureEnv {
	t.Helper()
	if len(link.lanPrefixes) == 0 {
		link.lanPrefixes = []netip.Prefix{pfx("192.168.0.0/24")}
	}
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, state)
	m.Base.LAN = "lan0"
	m.Now = c.now
	m.Boot = c.bootInfo
	require.NoError(t, m.Load(context.Background()))
	if _, ok := m.Network("a"); !ok {
		require.NoError(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16"), Remote: []netip.Prefix{pfx("10.0.2.7/32")}}))
		for _, b := range []string{"10.8.0.5", "10.8.0.6"} {
			require.NoError(t, m.Attach(context.Background(), network.Member{Network: "a", Address: addr(b)}))
		}
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
	c.advance(5 * time.Second)
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.Contains(t, e.f.lastScript(), "flush set inet naru leased", "a renewal refreshes the leased set")
	assert.NotContains(t, e.f.lastScript(), "delete table", "the table is not replaced by a renewal")

	changed := web(false)
	changed.Backends = changed.Backends[:1]
	require.NoError(t, e.x.Put(ctx, changed, 15*time.Second))
	assert.Contains(t, e.f.lastScript(), "delete table", "control: a change of backends replaces it")
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
	// A member whose address is inside the LAN prefix exercises checkExposure after the LAN prefix
	// check passes.
	require.NoError(t, e.m.Attach(ctx, network.Member{Network: "a", Address: addr("192.168.0.33")}))

	for _, tc := range []struct {
		name   string
		change func(*network.Exposure)
		want   error
	}{
		// Addresses outside the LAN prefix are refused by Link.Check (the LAN prefix check runs before
		// checkExposure). In production, a network's prefix never overlaps the LAN; the LAN prefix
		// check catches these addresses first.
		{"an address of the network", func(x *network.Exposure) { x.Address = addr("10.8.1.1") }, network.ErrOutsideLAN},
		{"a member's address", func(x *network.Exposure) { x.Address = addr("10.0.0.3") }, network.ErrOutsideLAN},
		{"a remote member's address", func(x *network.Exposure) { x.Address = addr("10.0.2.7") }, network.ErrOutsideLAN},
		{"an address outside the LAN's prefix", func(x *network.Exposure) { x.Address = addr("172.16.0.9") }, network.ErrOutsideLAN},
		// A member's address in the LAN prefix is refused by checkExposure after the LAN prefix check passes.
		{"a member's address in the LAN prefix", func(x *network.Exposure) { x.Address = addr("192.168.0.33") }, network.ErrReserved},
		{"a backend on another host", func(x *network.Exposure) { x.Backends = []netip.Addr{addr("10.0.2.7")} }, network.ErrBackend},
		{"a backend only in the prefix", func(x *network.Exposure) { x.Backends = []netip.Addr{addr("10.8.0.99")} }, network.ErrBackend},
		{"a backend outside the network", func(x *network.Exposure) { x.Backends = []netip.Addr{addr("10.8.0.5"), addr("10.9.0.5")} }, network.ErrBackend},
		{"a backend that is the LAN's", func(x *network.Exposure) { x.Backends = []netip.Addr{addr("192.168.0.1")} }, network.ErrBackend},
		{"an unknown network", func(x *network.Exposure) { x.Network = "nope" }, network.ErrUnknownNetwork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := web(false)
			tc.change(&x)
			err := e.x.Put(ctx, x, 15*time.Second)
			assert.ErrorIs(t, err, tc.want)
			assert.False(t, link.has(x.Address), "the address was not taken")
		})
	}
	assert.Empty(t, e.x.Exposures())

	ok := web(false)
	ok.Backends = []netip.Addr{addr("10.0.0.3"), addr("10.8.0.5")}
	require.NoError(t, e.x.Put(ctx, ok, 15*time.Second), "control: microVMs attached on this host are backends")
}

func TestExposer_AHostWithoutALANInterfaceRefuses(t *testing.T) {
	f := &fakeRunner{}
	m := newManager(f, &fakeTunnel{}, "")
	require.NoError(t, m.Declare(context.Background(), network.Network{Name: "a", Prefix: pfx("10.8.0.0/16")}))
	x := &network.Exposer{Manager: m, Link: newLink(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	require.NoError(t, x.Restore(context.Background()))
	assert.ErrorIs(t, x.Put(context.Background(), web(false), time.Second), network.ErrNoLAN)
	m.Base.LAN = "lan0"
	require.NoError(t, m.Attach(context.Background(), network.Member{Network: "a", Address: addr("10.8.0.5")}))
	require.NoError(t, m.Attach(context.Background(), network.Member{Network: "a", Address: addr("10.8.0.6")}))
	assert.NoError(t, x.Put(context.Background(), web(false), time.Second), "control: with one it is taken")
}

func TestExposer_AnAddressTheLinkRefusesLeavesNoRules(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	link.addErr = errors.New("no such device")
	require.Error(t, e.x.Put(context.Background(), web(false), 15*time.Second))
	assert.Empty(t, e.x.Exposures())
	assert.NotContains(t, e.f.last(), "dnat", "the host forwards no address it does not own")

	link.addErr = nil
	require.NoError(t, e.x.Put(context.Background(), web(false), 15*time.Second), "control: with the link working the same lease is taken")
	assert.Contains(t, e.f.last(), "dnat ip to")
}

func TestExposer_AnAddressAnsweredByARPRefusesWith409(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	a := addr("192.168.0.80")
	link.takenByProbe[a] = true
	assert.ErrorIs(t, e.x.Put(ctx, web(false), 15*time.Second), network.ErrTaken)
	assert.False(t, link.has(a), "the address was not taken")
	assert.Empty(t, e.x.Exposures())

	// Control: once the probe no longer answers, the same address is taken.
	delete(link.takenByProbe, a)
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second), "control: silence from the probe takes the lease")
	assert.True(t, link.has(a))

	// Renewals do not probe: the host already holds the address.
	link.takenByProbe[a] = true
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second), "control: a renewal is not probed")
}

func TestExposer_AnAddressOutsideTheLANPrefixIsRefused(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	link.lanPrefixes = []netip.Prefix{pfx("10.78.0.0/24")}
	e := startExposer(t, "", link, c)
	ctx := context.Background()

	// 192.168.0.80 is outside 10.78.0.0/24.
	assert.ErrorIs(t, e.x.Put(ctx, web(false), 15*time.Second), network.ErrOutsideLAN)
	assert.False(t, link.has(addr("192.168.0.80")), "the address was not taken")

	// An address inside the LAN prefix is accepted.
	inside := web(false)
	inside.Address = addr("10.78.0.80")
	require.NoError(t, e.x.Put(ctx, inside, 15*time.Second), "control: an address inside the prefix is taken")
	assert.True(t, link.has(addr("10.78.0.80")))
}

func TestExposer_AnAddressTheHostHoldsIsRefusedBeforeAnythingIsDone(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	link.refused[addr("192.168.0.80")] = true
	before := len(e.f.calls)
	assert.ErrorIs(t, e.x.Put(context.Background(), web(false), 15*time.Second), network.ErrHostAddress)
	assert.False(t, link.has(addr("192.168.0.80")))
	assert.Len(t, e.f.calls, before, "no rule was applied")
	other := web(false)
	other.Address = addr("192.168.0.81")
	assert.NoError(t, e.x.Put(context.Background(), other, 15*time.Second), "control: another address is taken")
}

func TestExposer_TheKernelIsGivenTheTimeThatIsLeft(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	a := addr("192.168.0.80")
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.Equal(t, 15*time.Second, link.validFor(a), "the address is valid for the lease")
	c.advance(5 * time.Second)
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.Equal(t, 15*time.Second, link.validFor(a), "a renewal gives it the lease again")
	c.advance(5 * time.Second)
	require.NoError(t, e.x.Tick(ctx))
	assert.Equal(t, 15*time.Second, link.validFor(a), "control: a pass with no renewal does not extend it")
}

func TestExposer_OneLeaseFailingLeavesTheOthersServed(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	second := web(false)
	second.Address = addr("192.168.0.81")
	link.failAdd[second.Address] = true
	require.Error(t, e.x.Put(ctx, second, 15*time.Second))
	assert.Equal(t, []netip.Addr{addr("192.168.0.80")}, addresses(e.x.Exposures()), "the lease that failed was released")
	assert.True(t, link.has(addr("192.168.0.80")), "the other is held")
	assert.Contains(t, e.f.last(), "192.168.0.80", "and served")
	assert.NotContains(t, e.f.last(), "192.168.0.81")
	delete(link.failAdd, second.Address)
	assert.NoError(t, e.x.Put(ctx, second, 15*time.Second), "control: without the failure it is taken")
}

func TestExposer_AFailedRenewalKeepsTheLease(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	a := addr("192.168.0.80")
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))
	c.advance(5 * time.Second)
	link.failAdd[a] = true
	assert.Error(t, e.x.Put(ctx, web(false), 15*time.Second))
	assert.Equal(t, []netip.Addr{a}, addresses(e.x.Exposures()), "the lease is kept")
	assert.True(t, link.has(a))
	assert.Contains(t, e.f.last(), "dnat ip to")
}

func TestExposer_ALeaseThatRanOutLeavesWhenNftFails(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ctx := context.Background()
	a := addr("192.168.0.80")
	require.NoError(t, e.x.Put(ctx, web(false), 15*time.Second))

	c.advance(15 * time.Second)
	e.f.fail = true
	assert.Error(t, e.x.Tick(ctx), "the failure is reported")
	assert.Empty(t, e.x.Exposures(), "the lease is gone")
	assert.False(t, link.has(a), "and its address with it")

	e.f.fail = false
	require.NoError(t, e.x.Tick(ctx))
	assert.NotContains(t, e.f.last(), "dnat", "the next pass makes the table again")
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

func TestExposer_ALeaseDoesNotSurviveWhatTheClockCannotTell(t *testing.T) {
	for _, tc := range []struct {
		name    string
		restart func(c *clock)
		kept    bool
	}{
		{"control: the same boot, within the lease", func(c *clock) { c.advance(5 * time.Second) }, true},
		{"another boot", func(c *clock) {
			c.advance(5 * time.Second)
			c.boot = func() (string, time.Duration, error) { return "boot-2", 5 * time.Second, nil }
		}, false},
		{"the wall clock stepped back after the lease ran out", func(c *clock) {
			c.advance(-time.Hour)
			c.boot = func() (string, time.Duration, error) { return "boot-1", 20 * time.Second, nil }
		}, false},
		{"the boot cannot be read", func(c *clock) {
			c.boot = func() (string, time.Duration, error) { return "", 0, errors.New("no /proc") }
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "networks.json")
			c, link := &clock{t: t0}, newLink()
			e := startExposer(t, state, link, c)
			require.NoError(t, e.x.Put(context.Background(), web(false), 15*time.Second))
			tc.restart(c)
			again := startExposer(t, state, link, c)
			if tc.kept {
				assert.Len(t, again.x.Exposures(), 1)
				assert.True(t, link.has(addr("192.168.0.80")))
				assert.InDelta(t, 10, link.validFor(addr("192.168.0.80")).Seconds(), 1, "the kernel is given what is left")
				return
			}
			assert.Empty(t, again.x.Exposures())
			assert.False(t, link.has(addr("192.168.0.80")))
		})
	}
}

func TestExposer_AStateThatNoLongerHoldsIsDroppedAtLoad(t *testing.T) {
	state := filepath.Join(t.TempDir(), "networks.json")
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, state, link, c)
	require.NoError(t, e.x.Put(context.Background(), web(false), 15*time.Second))
	raw, err := os.ReadFile(state)
	require.NoError(t, err)

	for _, tc := range []struct {
		name string
		edit func(string) string
		kept bool
	}{
		{"control: the file as it was written", func(s string) string { return s }, true},
		{"a backend outside the network", func(s string) string { return strings.Replace(s, "10.8.0.6", "10.9.9.9", 1) }, false},
		{"a node port outside the range", func(s string) string { return strings.Replace(s, "30080", "80", 1) }, false},
		{"an address inside a network prefix", func(s string) string { return strings.Replace(s, "192.168.0.80", "10.8.0.50", 1) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "networks.json")
			require.NoError(t, os.WriteFile(path, []byte(tc.edit(string(raw))), 0o600))
			again := startExposer(t, path, newLink(), c)
			assert.Equal(t, tc.kept, len(again.x.Exposures()) == 1)
		})
	}
}

func TestManager_RefusesAnExposureThatIsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*network.Exposure)
	}{
		{"a node port below the range", func(x *network.Exposure) { x.Ports[0].NodePort = 29999 }},
		{"a node port above the range", func(x *network.Exposure) { x.Ports[0].NodePort = 32768 }},
		{"a protocol that is neither", func(x *network.Exposure) { x.Ports[0].Protocol = "sctp" }},
		{"a port twice", func(x *network.Exposure) { x.Ports[1] = x.Ports[0] }},
		{"the PROXY protocol on a UDP port", func(x *network.Exposure) { x.Proxy = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := startExposer(t, "", newLink(), &clock{t: t0})
			x := web(false)
			tc.change(&x)
			x.Deadline = t0.Add(time.Minute)
			assert.ErrorIs(t, e.m.PutExposure(context.Background(), x), network.ErrInvalid)
		})
	}
	for _, port := range []int{30000, 32767} {
		e := startExposer(t, "", newLink(), &clock{t: t0})
		x := web(false)
		x.Ports[0].NodePort = port
		x.Deadline = t0.Add(time.Minute)
		assert.NoError(t, e.m.PutExposure(context.Background(), x), "control: node port %d is in the range", port)
	}
}
