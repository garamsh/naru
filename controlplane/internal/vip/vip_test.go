package vip

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	a1 = netip.MustParseAddr("10.0.0.1")
	a2 = netip.MustParseAddr("10.0.0.2")
	a3 = netip.MustParseAddr("10.0.0.3")
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type fakeHost struct {
	mu                            sync.Mutex
	holds                         bool
	acquires, releases, announces int
}

func (f *fakeHost) Holds() (bool, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.holds, nil }
func (f *fakeHost) Acquire() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds, f.acquires = true, f.acquires+1
	return nil
}
func (f *fakeHost) Announce() error { f.mu.Lock(); defer f.mu.Unlock(); f.announces++; return nil }
func (f *fakeHost) Release() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.holds, f.releases = false, f.releases+1
	return nil
}

func TestHolder_IsTheFirstMemberThatAnsweredAndHasNotMissedTooManyRounds(t *testing.T) {
	members := []netip.Addr{a1, a2, a3}
	for name, tc := range map[string]struct {
		missed map[netip.Addr]int
		want   netip.Addr
		ok     bool
	}{
		"nobody has answered":               {map[netip.Addr]int{}, netip.Addr{}, false},
		"the first answered":                {map[netip.Addr]int{a1: 0, a2: 0}, a1, true},
		"the first missed fewer than three": {map[netip.Addr]int{a1: 2, a2: 0}, a1, true},
		"the first missed three":            {map[netip.Addr]int{a1: 3, a2: 0}, a2, true},
		"only the last is alive":            {map[netip.Addr]int{a1: 5, a2: 9, a3: 0}, a3, true},
		"all missed":                        {map[netip.Addr]int{a1: 3, a2: 3, a3: 3}, netip.Addr{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := Holder(members, tc.missed, 3)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestStep_TheFirstLiveMemberAcquiresAndAnnouncesAndTheOthersDoNot(t *testing.T) {
	missed := map[netip.Addr]int{a1: 0, a2: 0, a3: 0}
	cfg := Config{Members: []netip.Addr{a1, a2, a3}, Misses: 3, AnnounceEvery: 10}
	for self, wantAcquires := range map[netip.Addr]int{a1: 1, a2: 0, a3: 0} {
		h := &fakeHost{}
		cfg.Self = self
		step(cfg, h, missed, 1, discard())
		assert.Equal(t, wantAcquires, h.acquires, "self %s", self)
	}
}

func TestStep_ANodeThatStopsBeingTheFirstLiveMemberReleasesTheAddress(t *testing.T) {
	cfg := Config{Members: []netip.Addr{a1, a2}, Self: a2, Misses: 3, AnnounceEvery: 10}
	h := &fakeHost{holds: true}
	// a1 is alive again: a2 yields.
	step(cfg, h, map[netip.Addr]int{a1: 0, a2: 0}, 1, discard())
	assert.False(t, h.holds)
	assert.Equal(t, 1, h.releases)
	// a2 holds and is alive and first: nothing changes, and a new holder announces nothing yet.
	h = &fakeHost{holds: true}
	step(cfg, h, map[netip.Addr]int{a1: 3, a2: 0}, 1, discard())
	assert.True(t, h.holds)
	assert.Zero(t, h.releases)
}

func TestStep_ANodeWhoseOwnAPIServerStoppedAnsweringReleasesTheAddress(t *testing.T) {
	cfg := Config{Members: []netip.Addr{a1, a2}, Self: a1, Misses: 3, AnnounceEvery: 10}
	h := &fakeHost{holds: true}
	step(cfg, h, map[netip.Addr]int{a1: 3, a2: 0}, 1, discard())
	assert.False(t, h.holds, "a Node that does not serve gives the address to one that does")
}

func TestStep_TheHolderAnnouncesAgainEveryAnnounceEveryRounds(t *testing.T) {
	cfg := Config{Members: []netip.Addr{a1}, Self: a1, Misses: 3, AnnounceEvery: 3}
	h := &fakeHost{holds: true}
	for round := 1; round <= 6; round++ {
		step(cfg, h, map[netip.Addr]int{a1: 0}, round, discard())
	}
	assert.Equal(t, 2, h.announces)
}

func TestRun_TakesTheAddressWhenTheFirstMemberStopsAnsweringAndYieldsWhenItReturns(t *testing.T) {
	var mu sync.Mutex
	down := map[netip.Addr]bool{}
	probe := func(_ context.Context, a netip.Addr) bool {
		mu.Lock()
		defer mu.Unlock()
		return !down[a]
	}
	set := func(a netip.Addr, v bool) { mu.Lock(); down[a] = v; mu.Unlock() }
	h := &fakeHost{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Config{Members: []netip.Addr{a1, a2}, Self: a2, Interval: 2 * time.Millisecond, Misses: 3, AnnounceEvery: 1000}, h, probe, discard())
		close(done)
	}()
	holds := func() bool { v, _ := h.Holds(); return v }
	// a1 answers first, so a2 holds nothing; a1 stops, and a2 holds the address.
	time.Sleep(20 * time.Millisecond)
	require.False(t, holds())
	set(a1, true)
	require.Eventually(t, holds, 2*time.Second, time.Millisecond)
	set(a1, false)
	require.Eventually(t, func() bool { return !holds() }, 2*time.Second, time.Millisecond)
	cancel()
	<-done
}

func TestARPFrame_AnnouncesTheAddressAtTheCardsAddressToEveryone(t *testing.T) {
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 7}
	f := ARPFrame(mac, netip.MustParseAddr("192.168.0.41"), 1)
	require.Len(t, f, 42)
	assert.Equal(t, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, f[0:6], "to everyone")
	assert.Equal(t, []byte(mac), f[6:12], "from the card")
	assert.Equal(t, []byte{0x08, 0x06}, f[12:14], "ARP")
	assert.Equal(t, []byte{0, 1}, f[20:22], "a request")
	assert.Equal(t, []byte(mac), f[22:28], "sender hardware address")
	assert.Equal(t, []byte{192, 168, 0, 41}, f[28:32], "sender is the address")
	assert.Equal(t, []byte{192, 168, 0, 41}, f[38:42], "so is the target")
	assert.Equal(t, []byte{0, 2}, ARPFrame(mac, netip.MustParseAddr("192.168.0.41"), 2)[20:22], "or a reply")
}

func TestStep_NoNodeTakesTheAddressInTheSettlingRoundsAfterTheStart(t *testing.T) {
	cfg := Config{Members: []netip.Addr{a1, a2}, Self: a2, Misses: 3, AnnounceEvery: 10, Settle: 3}
	h := &fakeHost{}
	alive := map[netip.Addr]int{a2: 0}
	for round := 1; round <= 3; round++ {
		step(cfg, h, alive, round, discard())
	}
	assert.Zero(t, h.acquires, "a2 is the first that answered, but the others may not have started")
	assert.Zero(t, h.announces)
	step(cfg, h, alive, 4, discard())
	assert.Equal(t, 1, h.acquires)
}
