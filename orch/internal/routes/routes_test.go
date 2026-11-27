package routes_test

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/machines"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/routes"
	routesetcd "github.com/garamsh/naru/orch/internal/routes/etcd"
)

const zone = "m.example.test"

// machine is a Machine placed on h-a at addr with a web and a db service, in phase.
func machine(id, addr string, phase machineapi.Phase) machines.Record {
	return machines.Record{
		Caller: "garam", Name: "n-" + id, ID: id,
		Spec: machineapi.Spec{Services: []machineapi.Service{
			{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP},
			{Name: "db", Port: 5432, Protocol: machineapi.ServiceTCP, ProxyProtocol: "v2"},
		}},
		Status:         machineapi.Status{Phase: phase},
		Binding:        &machines.Binding{Host: "h-a", Epoch: 1},
		PrivateAddress: netip.MustParseAddr(addr),
	}
}

func TestTable_NamesEachServiceOfAPlacedMachineAndEachAliasOfOne(t *testing.T) {
	a := machine("00000000000000aa", "10.200.0.5", machineapi.PhaseRunning)
	b := machine("00000000000000bb", "10.200.0.6", machineapi.PhaseStopped)
	got, dropped := routes.Table(zone, []machines.Record{a, b}, []machines.Alias{
		{Hostname: "shop.example.com", MachineID: a.ID, Service: "db", Certificate: "CERT", Key: "KEY"},
		{Hostname: "app." + zone, MachineID: b.ID},
	})
	addrA, addrB := a.PrivateAddress, b.PrivateAddress
	want := []hostapi.Route{
		{Hostname: "00000000000000aa." + zone, Machine: a.ID, Address: addrA, Port: 8080, Protocol: hostapi.RouteHTTP, Running: true},
		{Hostname: "00000000000000bb." + zone, Machine: b.ID, Address: addrB, Port: 8080, Protocol: hostapi.RouteHTTP},
		{Hostname: "app." + zone, Machine: b.ID, Address: addrB, Port: 8080, Protocol: hostapi.RouteHTTP},
		{Hostname: "db--00000000000000aa." + zone, Machine: a.ID, Address: addrA, Port: 5432, Protocol: hostapi.RouteTCP, ProxyProtocol: "v2", Running: true},
		{Hostname: "db--00000000000000bb." + zone, Machine: b.ID, Address: addrB, Port: 5432, Protocol: hostapi.RouteTCP, ProxyProtocol: "v2"},
	}
	// The alias outside the zone carries a pair that does not load, so it is dropped: Check is the
	// proxy's rule.
	assert.Equal(t, want, got, "the default service under <id>.<zone>, the others under <service>--<id>.<zone>, an alias at its service, sorted")
	require.Len(t, dropped, 1)
	assert.Equal(t, "shop.example.com", dropped[0].Hostname)
	assert.Contains(t, dropped[0].Reason, "certificate")
}

func TestTable_LeavesOutAMachineDeletingUnplacedOrWithNoAddressAndItsAliases(t *testing.T) {
	deleting := machine("00000000000000aa", "10.200.0.5", machineapi.PhaseDeleting)
	unplaced := machine("00000000000000bb", "10.200.0.6", machineapi.PhasePending)
	unplaced.Binding = nil
	unaddressed := machine("00000000000000cc", "10.200.0.7", machineapi.PhaseStarting)
	unaddressed.PrivateAddress = netip.Addr{}
	live := machine("00000000000000dd", "10.200.0.8", machineapi.PhaseRunning)
	got, dropped := routes.Table(zone, []machines.Record{deleting, unplaced, unaddressed, live}, []machines.Alias{
		{Hostname: "a." + zone, MachineID: deleting.ID}, {Hostname: "b." + zone, MachineID: unplaced.ID},
		{Hostname: "c." + zone, MachineID: unaddressed.ID}, {Hostname: "gone." + zone, MachineID: "00000000000000ee"},
	})
	assert.Empty(t, dropped)
	var names []string
	for _, r := range got {
		names = append(names, r.Hostname)
	}
	assert.Equal(t, []string{"00000000000000dd." + zone, "db--00000000000000dd." + zone}, names, "the control: the live Machine is routed")
}

func TestTable_DropsEveryRouteOfANameGivenTwice(t *testing.T) {
	a := machine("00000000000000aa", "10.200.0.5", machineapi.PhaseRunning)
	a.Spec.Services = a.Spec.Services[:1]
	twin := a
	twin.Name = "other"
	got, dropped := routes.Table(zone, []machines.Record{a, twin}, nil)
	assert.Empty(t, got)
	assert.Equal(t, []routes.Dropped{{"00000000000000aa." + zone, "the name is given twice"}, {"00000000000000aa." + zone, "the name is given twice"}}, dropped)
}

// fakeSource answers one revision's Machines and aliases; during, when not nil, runs inside each
// read, as a write and a delete's question would land while the read is in flight.
type fakeSource struct {
	mu     sync.Mutex
	recs   []machines.Record
	rev    int64
	during func()
}

func (s *fakeSource) Routing(context.Context) ([]machines.Record, []machines.Alias, int64, error) {
	s.mu.Lock()
	recs, rev, during := s.recs, s.rev, s.during
	s.mu.Unlock()
	if during != nil {
		during()
	}
	return recs, nil, rev, nil
}

// fakeHosts is the hosts: proxies take each table and answer loaded, reporting the max age in
// maxAge seconds (none when absent); ungranted report a proxy without the grant; others is
// admitted with no proxy; a host in refuse refuses every push; probe, when not nil, runs inside
// each probe, which takes as long as it does.
type fakeHosts struct {
	mu        sync.Mutex
	proxies   []string
	ungranted []string
	noProxy   []string
	maxAge    map[string]int64
	admitted  []string
	loaded    map[string]int64
	pushed    map[string][]hostapi.RoutesRequest
	refuse    map[string]error
	deadline  map[string]time.Duration
	probe     func()
}

func (h *fakeHosts) Proxies(context.Context) (routes.Probe, error) {
	h.mu.Lock()
	probe := h.probe
	out := routes.Probe{Ungranted: slices.Clone(h.ungranted), NoProxy: slices.Clone(h.noProxy)}
	for _, p := range h.proxies {
		out.Proxies = append(out.Proxies, routes.Proxy{ID: p, MaxAgeSeconds: h.maxAge[p]})
	}
	h.mu.Unlock()
	if probe != nil {
		probe()
	}
	return out, nil
}

func (h *fakeHosts) Admitted(context.Context) ([]string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.admitted...), nil
}

func (h *fakeHosts) Push(ctx context.Context, host string, t hostapi.RoutesRequest) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d, ok := ctx.Deadline(); ok {
		h.deadline[host] = time.Until(d)
	}
	h.pushed[host] = append(h.pushed[host], t)
	if err := h.refuse[host]; err != nil {
		return 0, err
	}
	return h.loaded[host], nil
}

// fleet keeps the events appended.
type fleet struct {
	mu     sync.Mutex
	events []orchapi.Event
}

func (f *fleet) AppendEvent(_ context.Context, ev orchapi.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fleet) of(typ string) []orchapi.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orchapi.Event
	for _, e := range f.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

type rig struct {
	src   *fakeSource
	hosts *fakeHosts
	acks  *routesetcd.Store
	clock *orchtest.Clock
	fleet *fleet
	push  *routes.Reconciler
}

func newRig(t *testing.T) *rig {
	t.Helper()
	kv := orchtest.Start(t, orchtest.Options{Members: 1}).Members[0].Etcd()
	r := &rig{
		src: &fakeSource{recs: []machines.Record{machine("00000000000000aa", "10.200.0.5", machineapi.PhaseRunning)}, rev: 40},
		hosts: &fakeHosts{proxies: []string{"p1", "p2"}, admitted: []string{"p1", "p2", "m1"}, maxAge: map[string]int64{}, loaded: map[string]int64{},
			pushed: map[string][]hostapi.RoutesRequest{}, refuse: map[string]error{}, deadline: map[string]time.Duration{}},
		acks: routesetcd.New(kv), clock: orchtest.NewClock(), fleet: &fleet{},
	}
	r.push = routes.New(routes.Config{Source: r.src, Hosts: r.hosts, Acks: r.acks, Zone: zone, Clock: r.clock, Fleet: r.fleet})
	return r
}

func (r *rig) acked(t *testing.T) map[string]routes.Ack {
	t.Helper()
	a, err := r.acks.Acks(t.Context())
	require.NoError(t, err)
	return a
}

func (r *rig) delivered(t *testing.T, rev int64, at time.Time) bool {
	t.Helper()
	ok, err := r.push.Delivered(t.Context(), rev, at)
	require.NoError(t, err)
	return ok
}

func TestReconcile_PutsTheWholeTableToEveryProxyHostAndKeepsWhatEachLoaded(t *testing.T) {
	r := newRig(t)
	r.hosts.loaded["p1"], r.hosts.loaded["p2"] = 40, 31
	require.NoError(t, r.push.Reconcile(t.Context()))

	for _, h := range []string{"p1", "p2"} {
		require.Len(t, r.hosts.pushed[h], 1, h)
		got := r.hosts.pushed[h][0]
		assert.Equal(t, int64(40), got.Revision, "the revision the table was read at")
		assert.True(t, got.SentAt.Equal(r.clock.Now()), "orch's time of sending")
		assert.Len(t, got.Routes, 2)
		assert.InDelta(t, hostapi.MaxRouteDelay, r.hosts.deadline[h], float64(100*time.Millisecond), "a push is bounded by the delay a host allows")
	}
	assert.Empty(t, r.hosts.pushed["m1"], "a host that runs no proxy is sent no table")
	acks := r.acked(t)
	assert.Equal(t, int64(40), acks["p1"].Loaded)
	assert.Equal(t, int64(31), acks["p2"].Loaded, "the revision the proxy loaded, which may be behind")
	assert.True(t, acks["p1"].LastSent.Equal(r.clock.Now()), "the time it was last sent a table")
	assert.Equal(t, wire.ProxyRouteMaxAge, acks["p1"].MaxAge, "a host that reports no max age is taken to have the proxy's default")
	assert.NotContains(t, acks, "m1")

	r.hosts.loaded["p2"] = 12
	require.NoError(t, r.push.Reconcile(t.Context()))
	assert.Equal(t, int64(31), r.acked(t)["p2"].Loaded, "an ack is never lowered")
}

func TestReconcile_AHostThatRefusesIsTrackedAndTheOthersAreServed(t *testing.T) {
	r := newRig(t)
	r.hosts.loaded["p2"] = 40
	r.hosts.refuse["p1"] = errors.New("late table")
	err := r.push.Reconcile(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "p1")
	acks := r.acked(t)
	assert.Equal(t, routes.Ack{MaxAge: wire.ProxyRouteMaxAge, LastSent: acks["p1"].LastSent}, acks["p1"], "tracked before its push, so a delete waits on it")
	assert.True(t, acks["p1"].LastSent.Equal(r.clock.Now()))
	assert.Equal(t, int64(40), acks["p2"].Loaded, "the control: the other host is served")
}

func TestReconcile_SendsATableWithATimeBeforeItsReadAfterASlowProbe(t *testing.T) {
	r := newRig(t)
	r.hosts.probe = func() { r.clock.Advance(10 * time.Second) }
	var read time.Time
	r.src.during = func() { read = r.clock.Now(); r.clock.Advance(time.Second) }
	require.NoError(t, r.push.Reconcile(t.Context()))
	require.False(t, read.IsZero(), "the setup: the table was read")
	for _, h := range []string{"p1", "p2"} {
		require.Len(t, r.hosts.pushed[h], 1)
		assert.False(t, r.hosts.pushed[h][0].SentAt.After(read), "%s: a table older than a commit after its read is sent with a time before the commit", h)
	}
}

func TestReconcile_TracksAHostThatJoinsBeforeTheTableIsReadSoADeleteWaitsOnIt(t *testing.T) {
	r := newRig(t)
	r.hosts.loaded["p1"], r.hosts.loaded["p2"] = 41, 41
	require.NoError(t, r.push.Reconcile(t.Context()))
	r.hosts.proxies = append(r.hosts.proxies, "p3")
	r.hosts.admitted = append(r.hosts.admitted, "p3")
	var during bool
	// The Machine is marked deleting at revision 41 while revision 40's table is being read, and
	// its delete asks at once.
	r.src.during = func() { during = r.delivered(t, 41, r.clock.Now()) }
	r.hosts.refuse["p3"] = errors.New("cut off after it took the table")
	_ = r.push.Reconcile(t.Context())
	assert.False(t, during, "p3 may take the table read before the delete, so the delete waits on it")
	assert.Contains(t, r.acked(t), "p3")
}

func TestReconcile_ProxyHostsWithoutTheGrantAreSentNothingAndNotedOnce(t *testing.T) {
	r := newRig(t)
	r.hosts.ungranted = []string{"u1"}
	r.hosts.admitted = append(r.hosts.admitted, "u1")
	require.NoError(t, r.push.Reconcile(t.Context()))
	require.NoError(t, r.push.Reconcile(t.Context()))
	assert.Empty(t, r.hosts.pushed["u1"], "a host's report of a proxy is no grant")
	assert.NotContains(t, r.acked(t), "u1")
	got := r.fleet.of(routes.EventProxyNotGranted)
	require.Len(t, got, 1, "one event for the leader")
	assert.Equal(t, "u1", got[0].Subject)
	assert.Len(t, r.hosts.pushed["p1"], 2, "control: the granted hosts are served")
}

func TestReconcile_TakesAMaxAgeOutsideWhatAProxyMayBeGivenAsTheLongest(t *testing.T) {
	r := newRig(t)
	r.hosts.maxAge["p1"] = math.MaxInt64
	r.hosts.maxAge["p2"] = 5400
	require.NoError(t, r.push.Reconcile(t.Context()))
	require.NoError(t, r.push.Reconcile(t.Context()))
	acks := r.acked(t)
	assert.Equal(t, wire.ProxyMaxRouteMaxAge, acks["p1"].MaxAge, "a value that would overflow is taken as the longest, not the default")
	assert.Equal(t, 90*time.Minute, acks["p2"].MaxAge, "control: one within the bound is kept")
	got := r.fleet.of(routes.EventMaxAgeClamped)
	require.Len(t, got, 1, "once for the leader")
	assert.Equal(t, "p1", got[0].Subject)

	r.hosts.maxAge["p2"] = -1
	require.NoError(t, r.push.Reconcile(t.Context()))
	assert.Equal(t, wire.ProxyMaxRouteMaxAge, r.acked(t)["p2"].MaxAge, "a negative one too")
}

func TestReconcile_ForgetsAHostThatNoLongerProxiesOnceItsLastTableExpired(t *testing.T) {
	for name, leave := range map[string]func(*fakeHosts){
		"it stopped running a proxy": func(h *fakeHosts) { h.proxies, h.noProxy = []string{"p1"}, []string{"p2"} },
		"its grant was revoked":      func(h *fakeHosts) { h.proxies, h.ungranted = []string{"p1"}, []string{"p2"} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.hosts.loaded["p1"], r.hosts.loaded["p2"] = 40, 40
			require.NoError(t, r.push.Reconcile(t.Context()))
			sent := r.clock.Now()
			leave(r.hosts)
			r.src.rev, r.hosts.loaded["p1"] = 50, 50

			r.clock.Advance(wire.ProxyRouteMaxAge + hostapi.RouteRelease - time.Millisecond)
			require.NoError(t, r.push.Reconcile(t.Context()))
			assert.Contains(t, r.acked(t), "p2", "its proxy may still forward on the table it was last sent")
			assert.False(t, r.delivered(t, 50, r.clock.Now()))

			r.clock.Advance(time.Millisecond)
			require.NoError(t, r.push.Reconcile(t.Context()))
			assert.NotContains(t, r.acked(t), "p2", "past its last table's max age and the release, it holds no table")
			assert.True(t, r.delivered(t, 50, r.clock.Now()), "so a delete no longer waits on it")
			assert.True(t, r.acked(t)["p1"].LastSent.After(sent), "control: the host that still proxies is tracked on")
		})
	}
}

func TestForget_KeepsAnAckTrackedSinceItWasRead(t *testing.T) {
	r := newRig(t)
	require.NoError(t, r.acks.Track(t.Context(), "p1", time.Hour, r.clock.Now()))
	read := r.acked(t)["p1"]
	require.NoError(t, r.acks.Track(t.Context(), "p1", time.Hour, r.clock.Now().Add(time.Minute)))

	require.NoError(t, r.acks.Forget(t.Context(), "p1", read))
	assert.Contains(t, r.acked(t), "p1", "a host sent a table since the read may forward on it")

	require.NoError(t, r.acks.Forget(t.Context(), "p1", r.acked(t)["p1"]))
	assert.NotContains(t, r.acked(t), "p1", "control: an ack still as read is deleted")
}

func TestDelivered_WaitsForEveryAdmittedProxyHostOrTheMaxAgeAndTheAllowance(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"], r.hosts.loaded["p2"] = 40, 39
	require.NoError(t, r.push.Reconcile(t.Context()))
	assert.False(t, r.delivered(t, 40, at), "p2's proxy forwards on revision 39, which may name the address")

	r.hosts.loaded["p2"] = 41
	require.NoError(t, r.push.Reconcile(t.Context()))
	assert.True(t, r.delivered(t, 40, at), "every proxy host has loaded a table at or above it")
}

func TestDelivered_AHostThatDoesNotAcknowledgeIsWaitedOutByTheLongestMaxAgeItReportedAndTheRelease(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"] = 40
	r.hosts.refuse["p2"] = errors.New("no answer")
	r.hosts.maxAge["p2"] = 7200
	_ = r.push.Reconcile(t.Context())
	r.hosts.maxAge["p2"] = 1800
	_ = r.push.Reconcile(t.Context())
	assert.Equal(t, 2*time.Hour, r.acked(t)["p2"].MaxAge, "a proxy restarted with a shorter max age may still forward on a table its longer one kept")

	r.clock.Advance(2*time.Hour + hostapi.RouteRelease - time.Millisecond)
	assert.False(t, r.delivered(t, 40, at), "p2 may still forward on a table it took up to the release after it was sent")
	r.clock.Advance(time.Millisecond)
	assert.True(t, r.delivered(t, 40, at), "past the max age and the release, p2 forwards on no table that named the address")
	assert.Equal(t, 8*time.Second, hostapi.RouteRelease, "the 2 s of a push, the host's skew counted against both sides, and the skew between orch members")
}

func TestDelivered_AHostRemovedFromTheInventoryLeavesTheSet(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"] = 40
	r.hosts.refuse["p2"] = errors.New("no answer")
	_ = r.push.Reconcile(t.Context())
	require.False(t, r.delivered(t, 40, at), "the control: p2 is waited on while it is admitted")

	r.hosts.admitted = []string{"p1", "m1"}
	assert.True(t, r.delivered(t, 40, at), "a removed host is out of every peer list, so out of the mesh its proxy forwards over")
}

func TestDelivered_NoHostThatWasEverSentATableIsWaitedOn(t *testing.T) {
	r := newRig(t)
	assert.True(t, r.delivered(t, 40, r.clock.Now()), "a host that holds no table forwards on none")
}
