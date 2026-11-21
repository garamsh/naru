package exposures_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/exposures/exposurestest"
	"github.com/garamsh/naru/orch/internal/inventory"
)

type fakeHosts struct {
	mu     sync.Mutex
	hosts  []exposures.Host
	held   map[string]map[string]bool // host -> address
	delErr map[string]error           // host -> error of Delete
	// onPut, when set, runs before a Put is recorded; it may block or advance the clock.
	onPut func(ctx context.Context, id, address string) error
	// received is the time each Put was recorded, by address.
	received map[string]time.Time
	clock    *exposurestest.Clock
}

func newHosts(h ...exposures.Host) *fakeHosts {
	return &fakeHosts{hosts: h, held: map[string]map[string]bool{}, delErr: map[string]error{}, received: map[string]time.Time{}}
}

func (f *fakeHosts) List(context.Context) ([]exposures.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exposures.Host(nil), f.hosts...), nil
}

func (f *fakeHosts) Put(ctx context.Context, id, address string, _ exposures.Lease) error {
	if f.onPut != nil {
		if err := f.onPut(ctx, id, address); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.clock != nil {
		f.received[address] = f.clock.Now()
	}
	if f.held[id] == nil {
		f.held[id] = map[string]bool{}
	}
	f.held[id][address] = true
	return nil
}

func (f *fakeHosts) Held(_ context.Context, id string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for a := range f.held[id] {
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeHosts) Delete(_ context.Context, id, address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.delErr[id]; err != nil {
		return err
	}
	delete(f.held[id], address)
	return nil
}

func (f *fakeHosts) lose(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, h := range f.hosts {
		if h.ID == id {
			f.hosts = append(f.hosts[:i:i], f.hosts[i+1:]...)
			return
		}
	}
}

func (f *fakeHosts) holders(address string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id, m := range f.held {
		if m[address] {
			out = append(out, id)
		}
	}
	return out
}

var bothNetworks = exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}, {Name: "b", Prefix: "10.2.0.0/24"}}

func reconciler(st *exposurestest.Store, h *fakeHosts, clk *exposurestest.Clock) *exposures.Reconciler {
	return reconcilerOn(st, h, clk, bothNetworks)
}

func reconcilerOn(st *exposurestest.Store, h *fakeHosts, clk *exposurestest.Clock, nets exposures.Networks) *exposures.Reconciler {
	return exposures.NewReconciler(exposures.Config{Hosts: h, Exposures: st, Networks: nets, Clock: clk, Log: slog.New(slog.DiscardHandler)})
}

// lease is the longest a host may keep an address after orch asked it to: the call, then the lease.
const lease = (exposures.CallTimeout + exposures.LeaseSeconds*time.Second)

func lanHosts(ids ...string) []exposures.Host {
	var out []exposures.Host
	for _, id := range ids {
		out = append(out, exposures.Host{ID: id, LAN: "eth0", Networks: []string{"a"}})
	}
	return out
}

func TestChooseSkipsAHostWithoutALANOrTheNetworkAndIsStable(t *testing.T) {
	hosts := []exposures.Host{{ID: "h1", Networks: []string{"a"}}, {ID: "h2", LAN: "eth0", Networks: []string{"b"}}}
	_, ok := exposures.Choose("192.168.0.80", "a", hosts)
	require.False(t, ok, "no host has both a LAN interface and a microVM on a")
	hosts = lanHosts("h1", "h2", "h3", "h4")
	first, ok := exposures.Choose("192.168.0.80", "a", hosts)
	require.True(t, ok)
	for range 3 {
		again, _ := exposures.Choose("192.168.0.80", "a", hosts)
		require.Equal(t, first, again)
	}
	// Losing a host that does not hold the address does not move it.
	var rest []exposures.Host
	for _, h := range hosts {
		if h.ID != first {
			rest = append(rest, h)
		}
	}
	for _, h := range rest {
		var without []exposures.Host
		for _, o := range hosts {
			if o.ID != h.ID {
				without = append(without, o)
			}
		}
		got, _ := exposures.Choose("192.168.0.80", "a", without)
		require.Equal(t, first, got, "losing %s moved the address", h.ID)
	}
}

func TestTheLeaderLeasesAnAddressToOneHostAndRenewsIt(t *testing.T) {
	s, st := fixture(t)
	e, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1", "h2", "h3")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(t.Context()))
	holders := h.holders(e.Address)
	require.Len(t, holders, 1)
	got, err := st.GetExposure(t.Context(), "a", "web")
	require.NoError(t, err)
	require.Equal(t, orchapi.ExposureAnnounced, got.Phase)
	require.Equal(t, holders[0], got.Host)
	require.Equal(t, clk.Now().Add(lease), got.LeaseUntil)

	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ = st.GetExposure(t.Context(), "a", "web")
	require.Equal(t, clk.Now().Add(lease), got.LeaseUntil, "renewed")
	require.Equal(t, holders, h.holders(e.Address))
}

func TestALostHostsAddressMovesAfterItsLeaseAndSkewAndNotBefore(t *testing.T) {
	s, st := fixture(t)
	e, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1", "h2", "h3")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(t.Context()))
	old := h.holders(e.Address)[0]
	h.lose(old)

	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ := st.GetExposure(t.Context(), "a", "web")
	require.Equal(t, old, got.Host, "the old lease may still run: the address stays")

	clk.Advance(lease - 5*time.Second + exposures.Skew)
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ = st.GetExposure(t.Context(), "a", "web")
	require.NotEqual(t, old, got.Host, "after the deadline and the skew the address moves")
	require.Equal(t, orchapi.ExposureAnnounced, got.Phase)
}

func TestAnAddressMovesAtOnceWhenTheOldHostConfirmsItRemovedIt(t *testing.T) {
	s, st := fixture(t)
	e, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1", "h2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(t.Context()))
	old := h.holders(e.Address)[0]
	// The old host is still up but no longer a candidate: it lost its microVMs on the network.
	h.mu.Lock()
	for i := range h.hosts {
		if h.hosts[i].ID == old {
			h.hosts[i].Networks = nil
		}
	}
	h.mu.Unlock()
	require.NoError(t, r.Reconcile(t.Context()))
	require.Len(t, h.holders(e.Address), 1)
	require.NotEqual(t, old, h.holders(e.Address)[0])

	// A host that cannot confirm keeps the address until the deadline passes.
	cur := h.holders(e.Address)[0]
	h.mu.Lock()
	h.delErr[cur] = errors.New("unreachable")
	for i := range h.hosts {
		if h.hosts[i].ID == cur {
			h.hosts[i].Networks = nil
		}
	}
	h.hosts = append(h.hosts, exposures.Host{ID: old, LAN: "eth0", Networks: []string{"a"}})
	h.mu.Unlock()
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ := st.GetExposure(t.Context(), "a", "web")
	require.Equal(t, cur, got.Host)
}

func TestALeaseNoExposureHoldsIsRemovedFromTheHost(t *testing.T) {
	_, st := fixture(t)
	h := newHosts(lanHosts("h1")...)
	h.held["h1"] = map[string]bool{"192.168.0.85": true}
	require.NoError(t, reconciler(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)}).Reconcile(t.Context()))
	require.Empty(t, h.holders("192.168.0.85"))
}

func TestNoCandidateHostLeavesTheExposurePending(t *testing.T) {
	s, st := fixture(t)
	_, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(exposures.Host{ID: "h1", Networks: []string{"a"}})
	require.NoError(t, reconciler(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)}).Reconcile(t.Context()))
	got, _ := st.GetExposure(t.Context(), "a", "web")
	require.Equal(t, orchapi.ExposurePending, got.Phase)
	require.Empty(t, got.Host)
}

func TestTheStoredDeadlineIsNeverBeforeTheEndOfTheHostsLease(t *testing.T) {
	s, st := fixture(t)
	e, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	h := newHosts(lanHosts("h1", "h2", "h3")...)
	h.clock = clk
	// The renewal reaches the host as late as the call may take.
	h.onPut = func(context.Context, string, string) error { clk.Advance(exposures.CallTimeout); return nil }
	require.NoError(t, reconciler(st, h, clk).Reconcile(t.Context()))
	got, err := st.GetExposure(t.Context(), "a", "web")
	require.NoError(t, err)
	hostEnds := h.received[e.Address].Add(exposures.LeaseSeconds * time.Second)
	require.False(t, got.LeaseUntil.Before(hostEnds), "stored %v, the host holds the address until %v", got.LeaseUntil, hostEnds)

	// Control: with a renewal that takes no time the deadline is the same bound, not later than
	// the call's allowance.
	h.onPut = nil
	clk.Advance(5 * time.Second)
	require.NoError(t, reconciler(st, h, clk).Reconcile(t.Context()))
	got, _ = st.GetExposure(t.Context(), "a", "web")
	require.Equal(t, clk.Now().Add(lease), got.LeaseUntil)
}

func TestAHostThatDoesNotAnswerDelaysNoOtherExposure(t *testing.T) {
	s, st := fixture(t)
	hosts := lanHosts("h1", "h2", "h3")
	byHost := map[string][]string{}
	for i := range 8 {
		e, err := s.ApplyExposure(t.Context(), "ccm", "a", "svc"+string(rune('a'+i)), spec("a", "10.1.0.5"))
		require.NoError(t, err)
		h, _ := exposures.Choose(e.Address, "a", hosts)
		byHost[h] = append(byHost[h], e.Address)
	}
	require.Greater(t, len(byHost), 1, "the exposures spread over more than one host")
	stuck := "h1"
	for h := range byHost {
		stuck = h
		break
	}
	// Both sides wait for the other to be under way, so only renewals made at the same time meet.
	stuckBegan, otherBegan := make(chan struct{}), make(chan struct{})
	var onceStuck, onceOther sync.Once
	var mu sync.Mutex
	met := 0
	h := newHosts(hosts...)
	h.onPut = func(ctx context.Context, id, _ string) error {
		begin, wait, once := otherBegan, stuckBegan, &onceOther
		if id == stuck {
			begin, wait, once = stuckBegan, otherBegan, &onceStuck
		}
		first := false
		once.Do(func() { close(begin); first = true })
		if !first && id != stuck {
			return nil
		}
		select {
		case <-wait:
			mu.Lock()
			met++
			mu.Unlock()
		case <-ctx.Done():
			return ctx.Err()
		}
		if id == stuck {
			return errors.New("never answers")
		}
		return nil
	}
	err := reconciler(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)}).Reconcile(t.Context())
	require.Error(t, err, "the exposures on the stuck host report it")
	require.GreaterOrEqual(t, met, 2, "the stuck host's renewal and another host's were under way together")
	for host, addrs := range byHost {
		for _, a := range addrs {
			if host == stuck {
				require.Empty(t, h.holders(a))
			} else {
				require.Equal(t, []string{host}, h.holders(a), "exposure %s renewed on %s", a, host)
			}
		}
	}
}

func TestADeletedExposureLeavesOnlyWhenItsHostConfirmsOrItsLeaseAndSkewPass(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	e, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1", "h2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	r := reconciler(st, h, clk)
	require.NoError(t, r.Reconcile(ctx))
	holder := h.holders(e.Address)[0]
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "web"))

	// The host cannot be asked: the record, and so the address, stays until the lease and Skew pass.
	h.mu.Lock()
	h.delErr[holder] = errors.New("unreachable")
	h.mu.Unlock()
	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(ctx))
	_, err = st.GetExposure(ctx, "a", "web")
	require.NoError(t, err, "its lease may still run")
	clk.Advance(lease + exposures.Skew)
	require.NoError(t, r.Reconcile(ctx))
	_, err = st.GetExposure(ctx, "a", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound, "the deadline and Skew have passed")

	// Control: a host that answers lets it go at once.
	e2, err := s.ApplyExposure(ctx, "ccm", "a", "db", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, e.Address, e2.Address, "the freed address is the lowest again")
	h.mu.Lock()
	delete(h.delErr, holder)
	h.mu.Unlock()
	require.NoError(t, r.Reconcile(ctx))
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "db"))
	require.NoError(t, r.Reconcile(ctx))
	_, err = st.GetExposure(ctx, "a", "db")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	require.Empty(t, h.holders(e2.Address), "the host no longer holds it")
}

func TestAnExposureWhoseNetworkIsGoneIsReleasedLikeADeletedOne(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	e, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1", "h2")...)
	clk := &exposurestest.Clock{T: time.Unix(2000, 0)}
	require.NoError(t, reconciler(st, h, clk).Reconcile(ctx))
	require.Len(t, h.holders(e.Address), 1)
	require.NoError(t, reconciler(st, h, clk).Reconcile(ctx))
	_, err = st.GetExposure(ctx, "a", "web")
	require.NoError(t, err, "control: while the network exists the exposure stays")

	gone := exposurestest.Networks{{Name: "b", Prefix: "10.2.0.0/24"}}
	require.NoError(t, reconcilerOn(st, h, clk, gone).Reconcile(ctx))
	_, err = st.GetExposure(ctx, "a", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	require.Empty(t, h.holders(e.Address))
}

func TestAFailedNetworkListReleasesNothing(t *testing.T) {
	s, st := fixture(t)
	_, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	h := newHosts(lanHosts("h1")...)
	r := reconcilerOn(st, h, &exposurestest.Clock{T: time.Unix(2000, 0)}, failingNetworks{})
	require.Error(t, r.Reconcile(t.Context()))
	_, err = st.GetExposure(t.Context(), "a", "web")
	require.NoError(t, err, "an unreadable list is not an empty one")
}

type failingNetworks struct{ exposurestest.Networks }

func (failingNetworks) List(context.Context) ([]orchapi.Network, error) {
	return nil, errors.New("etcd down")
}
