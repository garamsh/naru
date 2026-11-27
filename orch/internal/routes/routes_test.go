package routes_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
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

// fakeSource answers one revision's Machines and aliases.
type fakeSource struct {
	mu   sync.Mutex
	recs []machines.Record
	rev  int64
}

func (s *fakeSource) Routing(context.Context) ([]machines.Record, []machines.Alias, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recs, nil, s.rev, nil
}

// fakeHosts is the hosts: proxies take each table and answer loaded, reporting the max age in
// maxAge (none when absent), others is admitted with no proxy, and a host in refuse refuses every
// push.
type fakeHosts struct {
	mu       sync.Mutex
	proxies  []string
	maxAge   map[string]time.Duration
	admitted []string
	loaded   map[string]int64
	pushed   map[string][]hostapi.RoutesRequest
	refuse   map[string]error
	deadline map[string]time.Duration
}

func (h *fakeHosts) Proxies(context.Context) ([]routes.Proxy, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]routes.Proxy, len(h.proxies))
	for i, p := range h.proxies {
		out[i] = routes.Proxy{ID: p, MaxAge: h.maxAge[p]}
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

type rig struct {
	src   *fakeSource
	hosts *fakeHosts
	acks  *routesetcd.Store
	clock *orchtest.Clock
	push  *routes.Reconciler
}

func newRig(t *testing.T) *rig {
	t.Helper()
	kv := orchtest.Start(t, orchtest.Options{Members: 1}).Members[0].Etcd()
	r := &rig{
		src: &fakeSource{recs: []machines.Record{machine("00000000000000aa", "10.200.0.5", machineapi.PhaseRunning)}, rev: 40},
		hosts: &fakeHosts{proxies: []string{"p1", "p2"}, admitted: []string{"p1", "p2", "m1"}, maxAge: map[string]time.Duration{}, loaded: map[string]int64{},
			pushed: map[string][]hostapi.RoutesRequest{}, refuse: map[string]error{}, deadline: map[string]time.Duration{}},
		acks: routesetcd.New(kv), clock: orchtest.NewClock(),
	}
	r.push = routes.New(routes.Config{Source: r.src, Hosts: r.hosts, Acks: r.acks, Zone: zone, Clock: r.clock})
	return r
}

func (r *rig) acked(t *testing.T) map[string]routes.Ack {
	t.Helper()
	a, err := r.acks.Acks(t.Context())
	require.NoError(t, err)
	return a
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
	assert.True(t, acks["p1"].At.Equal(r.clock.Now()))
	assert.Equal(t, routes.DefaultMaxAge, acks["p1"].MaxAge, "a host that reports no max age is taken to have the proxy's default")
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
	assert.Equal(t, routes.Ack{MaxAge: routes.DefaultMaxAge}, acks["p1"], "tracked before its push, so a delete waits on it")
	assert.Equal(t, int64(40), acks["p2"].Loaded, "the control: the other host is served")
}

func TestDelivered_WaitsForEveryAdmittedProxyHostOrTheMaxAgeAndTheAllowance(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"], r.hosts.loaded["p2"] = 40, 39
	require.NoError(t, r.push.Reconcile(t.Context()))

	ok, err := r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	assert.False(t, ok, "p2's proxy forwards on revision 39, which may name the address")

	r.hosts.loaded["p2"] = 41
	require.NoError(t, r.push.Reconcile(t.Context()))
	ok, err = r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	assert.True(t, ok, "every proxy host has loaded a table at or above it")
}

func TestDelivered_AHostThatDoesNotAcknowledgeIsWaitedOutByTheLongestMaxAgeItReportedAndTheAllowance(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"] = 40
	r.hosts.refuse["p2"] = errors.New("no answer")
	r.hosts.maxAge["p2"] = 2 * time.Hour
	_ = r.push.Reconcile(t.Context())
	r.hosts.maxAge["p2"] = 30 * time.Minute
	_ = r.push.Reconcile(t.Context())
	assert.Equal(t, 2*time.Hour, r.acked(t)["p2"].MaxAge, "a proxy restarted with a shorter max age may still forward on a table its longer one kept")

	r.clock.Advance(2*time.Hour + hostapi.RouteAllowance - time.Millisecond)
	ok, err := r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	assert.False(t, ok, "p2 may still forward on a table it took up to the allowance after it was sent")
	r.clock.Advance(time.Millisecond)
	ok, err = r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	assert.True(t, ok, "past the max age and the allowance, p2 forwards on no table that named the address")
	assert.Equal(t, 4*time.Second, hostapi.RouteAllowance, "the 2 s of a push and 2 s of skew")
}

func TestDelivered_AHostRemovedFromTheInventoryLeavesTheSet(t *testing.T) {
	r := newRig(t)
	at := r.clock.Now()
	r.hosts.loaded["p1"] = 40
	r.hosts.refuse["p2"] = errors.New("no answer")
	_ = r.push.Reconcile(t.Context())
	ok, err := r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	require.False(t, ok, "the control: p2 is waited on while it is admitted")

	r.hosts.admitted = []string{"p1", "m1"}
	ok, err = r.push.Delivered(t.Context(), 40, at)
	require.NoError(t, err)
	assert.True(t, ok, "a removed host is out of every peer list, so out of the mesh its proxy forwards over")
}

func TestDelivered_NoHostThatWasEverSentATableIsWaitedOn(t *testing.T) {
	r := newRig(t)
	ok, err := r.push.Delivered(t.Context(), 40, r.clock.Now())
	require.NoError(t, err)
	assert.True(t, ok, "a host that holds no table forwards on none")
}
