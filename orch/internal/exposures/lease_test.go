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
)

type fakeHosts struct {
	mu     sync.Mutex
	hosts  []exposures.Host
	held   map[string]map[string]bool // host -> address
	delErr map[string]error           // host -> error of Delete
}

func newHosts(h ...exposures.Host) *fakeHosts {
	return &fakeHosts{hosts: h, held: map[string]map[string]bool{}, delErr: map[string]error{}}
}

func (f *fakeHosts) List(context.Context) ([]exposures.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exposures.Host(nil), f.hosts...), nil
}

func (f *fakeHosts) Put(_ context.Context, id, address string, _ exposures.Lease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

func reconciler(st *exposurestest.Store, h *fakeHosts, clk *exposurestest.Clock) *exposures.Reconciler {
	return exposures.NewReconciler(exposures.Config{Hosts: h, Exposures: st, Clock: clk, Log: slog.New(slog.DiscardHandler)})
}

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
	got, err := st.GetExposure(t.Context(), "web")
	require.NoError(t, err)
	require.Equal(t, orchapi.ExposureAnnounced, got.Phase)
	require.Equal(t, holders[0], got.Host)
	require.Equal(t, clk.Now().Add(exposures.LeaseSeconds*time.Second), got.LeaseUntil)

	clk.Advance(5 * time.Second)
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ = st.GetExposure(t.Context(), "web")
	require.Equal(t, clk.Now().Add(exposures.LeaseSeconds*time.Second), got.LeaseUntil, "renewed")
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
	got, _ := st.GetExposure(t.Context(), "web")
	require.Equal(t, old, got.Host, "the old lease may still run: the address stays")

	clk.Advance((exposures.LeaseSeconds-5)*time.Second + exposures.Skew)
	require.NoError(t, r.Reconcile(t.Context()))
	got, _ = st.GetExposure(t.Context(), "web")
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
	got, _ := st.GetExposure(t.Context(), "web")
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
	got, _ := st.GetExposure(t.Context(), "web")
	require.Equal(t, orchapi.ExposurePending, got.Phase)
	require.Empty(t, got.Host)
}
