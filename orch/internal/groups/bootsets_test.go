package groups_test

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

func TestBootSetName(t *testing.T) {
	for in, want := range map[string]string{
		"v0.4.0": "naru-node-v0-4-0", "V1.2.3-RC.1+Build": "naru-node-v1-2-3-rc-1-build", "dev": "naru-node-dev",
	} {
		got, err := groups.BootSetName(in)
		require.NoError(t, err)
		assert.Equal(t, want, got, in)
	}
	_, err := groups.BootSetName(strings.Repeat("v", 60))
	assert.Error(t, err, "a name longer than 63 characters")
}

func TestResolveBootSet(t *testing.T) {
	name, offers := groups.ResolveBootSet("v0.4.0", []string{"naru", "naru-node-v0-4-0"})
	assert.Equal(t, "naru-node-v0-4-0", name)
	assert.True(t, offers, "the derived name is offered")

	name, offers = groups.ResolveBootSet("v0.4.0", []string{"naru"})
	assert.Equal(t, "naru-node-v0-4-0", name)
	assert.False(t, offers, "the derived name is not offered")

	name, offers = groups.ResolveBootSet("naru", []string{"naru"})
	assert.Equal(t, "naru", name, "an exact name a host offers is taken as it is")
	assert.True(t, offers)
}

func TestPlace_TakesTheBootSetNamedByTheVersion(t *testing.T) {
	s := spec("web", 1)
	s.Template.Form = groups.FormOS
	s.Template.NodeVersion = "v0.4.0"
	h := okHost("a")
	h.NodeVersions = []string{"naru"}
	_, err := groups.Place(s, groups.WantOf(s, "web-1"), []groups.Host{h}, nil)
	require.ErrorIs(t, err, groups.ErrNoHost, "control: no boot set for the version")

	h.NodeVersions = []string{"naru", "naru-node-v0-4-0"}
	got, err := groups.Place(s, groups.WantOf(s, "web-1"), []groups.Host{h}, nil)
	require.NoError(t, err)
	assert.Equal(t, "a", got.ID)
}

type bsStore struct{ specs []groups.GroupSpec }

func (s bsStore) ListGroups(context.Context) ([]groups.GroupSpec, error) { return s.specs, nil }
func (bsStore) ListMembers(context.Context) ([]groups.Member, error)     { return nil, nil }
func (bsStore) ListNetworks(context.Context) ([]groups.Network, error)   { return nil, nil }
func (bsStore) PutMember(context.Context, groups.Member) (groups.Member, error) {
	return groups.Member{}, errors.New("not used")
}

type bsEvents struct {
	mu sync.Mutex
	es []groups.Event
}

func (e *bsEvents) Record(_ context.Context, ev groups.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.es = append(e.es, ev)
	return nil
}

func (e *bsEvents) kind(k groups.EventKind) []groups.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []groups.Event
	for _, ev := range e.es {
		if ev.Kind == k {
			out = append(out, ev)
		}
	}
	return out
}

type bsClock struct{}

func (bsClock) Now() time.Time                       { return t0 }
func (bsClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// bsHosts records the uploads; gate, when set, holds each one until it is released.
type bsHosts struct {
	hosts   []groups.Host
	fail    map[string]error
	gate    chan struct{}
	mu      sync.Mutex
	added   []string
	now     atomic.Int32
	highest atomic.Int32
}

func (h *bsHosts) List(context.Context) ([]groups.Host, error) { return h.hosts, nil }

func (h *bsHosts) AddBootSet(_ context.Context, id string, set groups.BootSet) error {
	n := h.now.Add(1)
	defer h.now.Add(-1)
	for {
		m := h.highest.Load()
		if n <= m || h.highest.CompareAndSwap(m, n) {
			break
		}
	}
	if h.gate != nil {
		<-h.gate
	}
	if err := h.fail[id]; err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.added = append(h.added, id+":"+set.Name)
	return nil
}

func (h *bsHosts) sent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.added...)
	sort.Strings(out)
	return out
}

type bsReleases struct {
	err     error
	fetched []string
}

func (r *bsReleases) Fetch(_ context.Context, version string) (groups.BootSet, error) {
	r.fetched = append(r.fetched, version)
	if r.err != nil {
		return groups.BootSet{}, r.err
	}
	return groups.BootSet{Name: "naru-node-v0-4-0"}, nil
}

func osGroup() groups.GroupSpec {
	s := spec("web", 1)
	s.Template.Form = groups.FormOS
	s.Template.NodeVersion = "v0.4.0"
	return s
}

func reconciler(specs []groups.GroupSpec, hosts *bsHosts, rel groups.ReleaseStore) (*groups.BootSetReconciler, *bsEvents) {
	ev := &bsEvents{}
	return groups.NewBootSets(groups.BootSetConfig{Hosts: hosts, Store: bsStore{specs}, Releases: rel, Clock: bsClock{}, Events: ev}), ev
}

func bare(id string) groups.Host {
	h := okHost(id)
	h.NodeVersions = []string{"naru"}
	return h
}

func TestBootSets_AddsToEligibleHostsOnly(t *testing.T) {
	notReady, draining, otherRegion, noLabel, has := bare("not-ready"), bare("draining"), bare("other-region"), bare("no-label"), bare("has-it")
	notReady.Ready = false
	draining.Draining = true
	otherRegion.Region = "kr-2"
	noLabel.Labels = nil
	has.NodeVersions = []string{"naru", "naru-node-v0-4-0"}
	s := osGroup()
	s.RequiredLabels = map[string]string{"disk": "ssd"}
	hosts := &bsHosts{hosts: []groups.Host{bare("a"), notReady, draining, otherRegion, noLabel, has}}
	rel := &bsReleases{}
	r, ev := reconciler([]groups.GroupSpec{s}, hosts, rel)

	require.NoError(t, r.Reconcile(context.Background()))

	assert.Equal(t, []string{"a:naru-node-v0-4-0"}, hosts.sent())
	assert.Equal(t, []string{"v0.4.0"}, rel.fetched, "the release is verified once, however many hosts take it")
	added := ev.kind(groups.EventBootSetAdded)
	require.Len(t, added, 1)
	assert.Equal(t, "a", added[0].Host)
}

func TestBootSets_ImageFormAndNoVersionNeedNothing(t *testing.T) {
	image := spec("img", 1)
	osNoVersion := osGroup()
	osNoVersion.Template.NodeVersion = ""
	hosts := &bsHosts{hosts: []groups.Host{bare("a")}}
	rel := &bsReleases{}
	r, _ := reconciler([]groups.GroupSpec{image, osNoVersion}, hosts, rel)
	require.NoError(t, r.Reconcile(context.Background()))
	assert.Empty(t, rel.fetched)
	assert.Empty(t, hosts.sent())
}

func TestBootSets_NamedBuiltInIsNotFetched(t *testing.T) {
	s := osGroup()
	s.Template.NodeVersion = "naru"
	hosts := &bsHosts{hosts: []groups.Host{bare("a")}}
	rel := &bsReleases{}
	r, _ := reconciler([]groups.GroupSpec{s}, hosts, rel)
	require.NoError(t, r.Reconcile(context.Background()))
	assert.Empty(t, rel.fetched, "a name the host offers needs no release")
}

func TestBootSets_AtMostTwoHostsAtOnce(t *testing.T) {
	var hs []groups.Host
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		hs = append(hs, bare(id))
	}
	hosts := &bsHosts{hosts: hs, gate: make(chan struct{})}
	r, _ := reconciler([]groups.GroupSpec{osGroup()}, hosts, &bsReleases{})

	done := make(chan error, 1)
	go func() { done <- r.Reconcile(context.Background()) }()
	require.Eventually(t, func() bool { return hosts.now.Load() == 2 }, 5*time.Second, time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.EqualValues(t, 2, hosts.now.Load(), "a third upload does not start while two run")
	for range hs {
		hosts.gate <- struct{}{}
	}
	require.NoError(t, <-done)
	assert.EqualValues(t, 2, hosts.highest.Load())
	assert.Len(t, hosts.sent(), 5, "every host gets the set")
}

func TestBootSets_SendsNothingBeforeTheReleaseVerifies(t *testing.T) {
	hosts := &bsHosts{hosts: []groups.Host{bare("a"), bare("b")}}
	r, ev := reconciler([]groups.GroupSpec{osGroup()}, hosts, &bsReleases{err: errors.New("signature check failed: bad")})

	require.NoError(t, r.Reconcile(context.Background()))

	assert.Empty(t, hosts.sent(), "no host is called when the release does not verify")
	assert.EqualValues(t, 0, hosts.highest.Load())
	failed := ev.kind(groups.EventBootSetFailed)
	require.Len(t, failed, 2)
	assert.Contains(t, failed[0].Message, "signature check failed")
	assert.Empty(t, ev.kind(groups.EventBootSetAdded))

	ok, _ := reconciler([]groups.GroupSpec{osGroup()}, hosts, &bsReleases{})
	require.NoError(t, ok.Reconcile(context.Background()))
	assert.Len(t, hosts.sent(), 2, "control: the same hosts are called once the release verifies")
}

func TestBootSets_RecordsAHostThatRefusesTheSet(t *testing.T) {
	hosts := &bsHosts{hosts: []groups.Host{bare("a"), bare("b")}, fail: map[string]error{"b": errors.New("409 conflict")}}
	r, ev := reconciler([]groups.GroupSpec{osGroup()}, hosts, &bsReleases{})
	require.NoError(t, r.Reconcile(context.Background()))
	assert.Equal(t, []string{"a:naru-node-v0-4-0"}, hosts.sent())
	failed := ev.kind(groups.EventBootSetFailed)
	require.Len(t, failed, 1)
	assert.Equal(t, "b", failed[0].Host)
	assert.Contains(t, failed[0].Message, "409 conflict")
}

func TestEngine_CreatesTheMemberOnTheBootSetTheVersionNames(t *testing.T) {
	w := newWorld(t)
	w.addHost("a", "kr-1", nil, "naru", "naru-node-v0-4-0")
	s := osGroup()
	s.Template.Disks = []groups.Disk{{Name: "data", SizeBytes: 1 << 30}}
	w.setGroup(s)
	e := w.engine()
	w.settle(e, 5)
	m, _ := w.machine(w.live("web")[0].ID)
	require.NotNil(t, m)
	assert.Equal(t, "naru-node-v0-4-0", m.spec.BootSet)
}

// bsTick is a clock the test moves.
type bsTick struct{ now time.Time }

func (c *bsTick) Now() time.Time                       { return c.now }
func (c *bsTick) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

func tickingReconciler(hosts *bsHosts, rel groups.ReleaseStore, clock *bsTick) (*groups.BootSetReconciler, *bsEvents) {
	ev := &bsEvents{}
	return groups.NewBootSets(groups.BootSetConfig{Hosts: hosts, Store: bsStore{[]groups.GroupSpec{osGroup()}}, Releases: rel,
		Clock: clock, Events: ev, Interval: time.Minute}), ev
}

func TestBootSets_BacksOffAHostThatKeepsFailing(t *testing.T) {
	clock := &bsTick{now: t0}
	rel := &bsReleases{err: errors.New("store down")}
	hosts := &bsHosts{hosts: []groups.Host{bare("a")}}
	r, _ := tickingReconciler(hosts, rel, clock)
	pass := func(after time.Duration) int {
		clock.now = clock.now.Add(after)
		before := len(rel.fetched)
		require.NoError(t, r.Reconcile(context.Background()))
		return len(rel.fetched) - before
	}

	assert.Equal(t, 1, pass(0), "the first pass tries")
	assert.Equal(t, 0, pass(30*time.Second), "within the interval it waits")
	assert.Equal(t, 1, pass(30*time.Second), "after one interval it tries again")
	assert.Equal(t, 0, pass(90*time.Second), "after two failures it waits two intervals")
	assert.Equal(t, 1, pass(30*time.Second), "and tries when they are over")
	for range 10 {
		pass(time.Hour)
	}
	assert.Equal(t, 0, pass(9*time.Minute), "the wait stops growing at ten minutes")
	assert.Equal(t, 1, pass(time.Minute), "and ten minutes is enough")

	rel.err = nil
	pass(10 * time.Minute)
	assert.Equal(t, []string{"a:naru-node-v0-4-0"}, hosts.sent(), "control: a host that is not failing is served")
}

func TestBootSets_ForgetsABackoffOnceTheSetIsNoLongerLacking(t *testing.T) {
	clock := &bsTick{now: t0}
	rel := &bsReleases{err: errors.New("store down")}
	a := bare("a")
	hosts := &bsHosts{hosts: []groups.Host{a}}
	r, _ := tickingReconciler(hosts, rel, clock)
	for range 3 {
		clock.now = clock.now.Add(time.Hour)
		require.NoError(t, r.Reconcile(context.Background()))
	}
	a.NodeVersions = []string{"naru", "naru-node-v0-4-0"}
	hosts.hosts = []groups.Host{a}
	require.NoError(t, r.Reconcile(context.Background()))

	a.NodeVersions = []string{"naru"}
	hosts.hosts = []groups.Host{a}
	n := len(rel.fetched)
	require.NoError(t, r.Reconcile(context.Background()))
	assert.Equal(t, n+1, len(rel.fetched), "a host that lost the set starts again from no backoff")
}

// The same refusal worded differently each pass is one failure: the event carries a class both
// share, and a Failure's class is what names it.
func TestBootSets_FailureEventsCarryTheClassOfTheFailure(t *testing.T) {
	clock := &bsTick{now: t0}
	rel := &bsReleases{err: fmt.Errorf("%w: detail one", &groups.Failure{Class: "signature check failed"})}
	hosts := &bsHosts{hosts: []groups.Host{bare("a")}}
	r, ev := tickingReconciler(hosts, rel, clock)
	require.NoError(t, r.Reconcile(context.Background()))
	rel.err = fmt.Errorf("%w: detail two", &groups.Failure{Class: "signature check failed"})
	clock.now = clock.now.Add(time.Hour)
	require.NoError(t, r.Reconcile(context.Background()))

	failed := ev.kind(groups.EventBootSetFailed)
	require.Len(t, failed, 2)
	assert.Equal(t, failed[0].Class, failed[1].Class)
	assert.Equal(t, "naru-node-v0-4-0: signature check failed", failed[0].Class)
	assert.NotEqual(t, failed[0].Message, failed[1].Message)

	rel.err = errors.New("store down")
	clock.now = clock.now.Add(time.Hour)
	require.NoError(t, r.Reconcile(context.Background()))
	other := ev.kind(groups.EventBootSetFailed)[2]
	assert.NotEqual(t, failed[0].Class, other.Class, "control: another kind of failure is another class")
}

func TestBootSetName_RefusesAVersionThatIsAlreadyABootSetName(t *testing.T) {
	for _, v := range []string{"naru-node-v0-4-0", "Naru-Node-v1"} {
		_, err := groups.BootSetName(v)
		assert.Error(t, err, v)
	}
	_, err := groups.BootSetName("v0.4.0")
	assert.NoError(t, err, "control: a release version")

	name, offers := groups.ResolveBootSet("naru-node-v0-4-0", []string{"naru"})
	assert.Empty(t, name, "a boot set name a host does not offer names nothing")
	assert.False(t, offers)
	name, offers = groups.ResolveBootSet("naru-node-v0-4-0", []string{"naru-node-v0-4-0"})
	assert.Equal(t, "naru-node-v0-4-0", name, "control: one the host offers is taken as it is")
	assert.True(t, offers)
}
