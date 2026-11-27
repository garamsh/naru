package etcd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
	"github.com/garamsh/naru/orch/internal/prefixes"
)

// raceKV runs race once, right after the first read of a host, as a writer that got in between.
type raceKV struct {
	clientv3.KV
	race func()
}

func (r *raceKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	resp, err := r.KV.Get(ctx, key, opts...)
	if race := r.race; race != nil {
		r.race = nil
		race()
	}
	return resp, err
}

func TestARetriedHostUpdateAppliesItsChangeToTheValueItReRead(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := &raceKV{KV: c.Members[0].Etcd()}
	s := invetcd.New(kv)

	const key = "/naru/orch/hosts/h1"
	draining, err := json.Marshal(inventory.Host{ID: "h1", Region: "kr-seoul", Draining: true, Labels: map[string]string{"rack": "a"}})
	require.NoError(t, err)
	// An undrain stores a document with no draining field and no labels at all.
	undrained := `{"id":"h1","region":"kr-seoul"}`
	_, err = kv.KV.Put(t.Context(), key, string(draining))
	require.NoError(t, err)

	attempts := 0
	kv.race = func() {
		_, err := kv.KV.Put(t.Context(), key, undrained)
		require.NoError(t, err)
	}
	seen := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	got, err := s.UpdateHost(t.Context(), "h1", func(h *inventory.Host) { attempts++; h.LastSeen = seen }, nil)
	require.NoError(t, err)

	require.Equal(t, 2, attempts, "the race must have forced exactly one retry")
	require.False(t, got.Draining)
	require.Nil(t, got.Labels)
	require.True(t, got.LastSeen.Equal(seen))

	stored, err := s.GetHost(t.Context(), "h1")
	require.NoError(t, err)
	require.False(t, stored.Draining, "the stored host kept the draining flag of the first attempt")
	require.Nil(t, stored.Labels, "the stored host kept the labels of the first attempt")
}

func TestAnEnrollmentWriteNeverResurrectsAPendingRecordNextToAnAdmittedHost(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	p := inventory.Pending{HostID: "h1", PublicKey: []byte("k"), FirstSeen: now}
	ev := orchapi.Event{Time: now, Type: inventory.EventAdmitted, Subject: "h1"}

	require.NoError(t, s.PutPending(ctx, p, nil, true))
	require.ErrorIs(t, s.PutPending(ctx, p, nil, true), inventory.ErrConflict, "a create finds it pending")
	require.NoError(t, s.PutPending(ctx, p, nil, false), "control: an update of a pending host holds")

	require.NoError(t, s.Admit(ctx, p, inventory.Host{ID: "h1", PublicKey: p.PublicKey}, ev))
	require.ErrorIs(t, s.PutPending(ctx, p, nil, false), inventory.ErrAlreadyAdmitted, "an update after the admission")
	require.ErrorIs(t, s.PutPending(ctx, p, nil, true), inventory.ErrAlreadyAdmitted, "a create after the admission")
	_, err := s.GetPending(ctx, "h1")
	require.ErrorIs(t, err, inventory.ErrNotFound)
}

func TestReservationsNeverOverlapAreTakenOverWhenExpiredAndReleasedOnlyByTheirHolder(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	pick := func(claimed []netip.Prefix) (netip.Prefix, error) {
		return prefixes.Allocate(netip.MustParsePrefix("10.200.0.0/22"), 24, claimed)
	}

	a, err := s.Reserve(ctx, inventory.SpaceMachine, "h-a", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.0.0/24", a.Prefix)
	b, err := s.Reserve(ctx, inventory.SpaceMachine, "h-b", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.1.0/24", b.Prefix)
	again, err := s.Reserve(ctx, inventory.SpaceMachine, "h-a", pick, now.Add(2*time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, a.Prefix, again.Prefix, "a host that asks again keeps its reservation")

	later := now.Add(3 * time.Minute)
	c2, err := s.Reserve(ctx, inventory.SpaceMachine, "h-c", pick, later.Add(time.Minute), later)
	require.NoError(t, err)
	require.Equal(t, "10.200.0.0/24", c2.Prefix, "an expired reservation is taken over")
	all, err := s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.Len(t, all, 1, "the expired reservations are dropped, not left beside the new one")

	require.NoError(t, s.Release(ctx, a), "a stale holder's release")
	all, err = s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.Equal(t, []inventory.Allocation{c2}, all, "does not drop what another host holds now")
	require.NoError(t, s.Release(ctx, c2))
	all, err = s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.Empty(t, all)
}

func TestTheMigrationDropsTheStoredConfigOfEveryHostAndIsIdempotent(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := invetcd.New(kv)
	for id, doc := range map[string]string{
		"h1": `{"id":"h1","region":"r","config":"cGFzc3dvcmQ6IGh1bnRlcjI=","machinePrefix":"10.200.1.0/24"}`,
		"h2": `{"id":"h2","region":"r","config":"cGFzc3dvcmQ6IGh1bnRlcjI="}`,
		"h3": `{"id":"h3","region":"r"}`,
	} {
		_, err := kv.Put(t.Context(), "/naru/orch/hosts/"+id, doc)
		require.NoError(t, err)
	}
	n, err := s.DropStoredConfigs(t.Context())
	require.NoError(t, err)
	// The member's own leader runs the same rewrite as soon as it leads, so it may have taken some
	// of the two records first: this call rewrites those it finds, and none is left either way.
	require.LessOrEqual(t, n, 2)

	resp, err := kv.Get(t.Context(), "/naru/orch/hosts/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 3)
	for _, kv := range resp.Kvs {
		require.NotContains(t, string(kv.Value), "config", "%s", kv.Key)
	}
	h1, err := s.GetHost(t.Context(), "h1")
	require.NoError(t, err)
	require.Equal(t, "10.200.1.0/24", h1.MachinePrefix, "the rest of the record stays")

	n, err = s.DropStoredConfigs(t.Context())
	require.NoError(t, err)
	require.Zero(t, n, "a second run rewrites nothing")
}

func TestAHeartbeatDoesNotWriteBackAConfigAnOlderRecordCarried(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := invetcd.New(kv)
	_, err := kv.Put(t.Context(), "/naru/orch/hosts/h1", `{"id":"h1","config":"c2VjcmV0"}`)
	require.NoError(t, err)
	_, err = s.UpdateHost(t.Context(), "h1", func(h *inventory.Host) { h.LastSeen = time.Unix(5, 0).UTC() }, nil)
	require.NoError(t, err)
	resp, err := kv.Get(t.Context(), "/naru/orch/hosts/h1")
	require.NoError(t, err)
	require.NotContains(t, string(resp.Kvs[0].Value), "config")
}

func TestTheEventLogKeepsTheNewestEventsAndDropsTheOldest(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	const keep, extra = 10, 5
	invetcd.KeepEvents(s, keep)
	require.Equal(t, 20000, invetcd.MaxEvents, "the bound a store keeps by default")
	base := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	invetcd.SetClock(s, func() time.Time { return base })

	for i := range keep + extra {
		require.NoError(t, s.AppendEvent(t.Context(), orchapi.Event{Time: base.Add(time.Duration(i) * time.Second), Type: "t", Subject: "s", Detail: map[string]string{"n": time.Duration(i).String()}}))
	}

	evs, err := s.ListEvents(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, evs, keep, "the log stops at its bound")
	require.True(t, evs[0].Time.Equal(base.Add(extra*time.Second)), "the oldest were the ones dropped")
	require.True(t, evs[len(evs)-1].Time.Equal(base.Add((keep+extra-1)*time.Second)))

	since, err := s.ListEvents(t.Context(), evs[len(evs)-3].Cursor)
	require.NoError(t, err)
	require.Len(t, since, 2)
}

func TestALogFarPastItsBoundIsTrimmedToItInOneAppend(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := invetcd.New(kv)
	const keep = 10
	invetcd.KeepEvents(s, keep)
	invetcd.SetClock(s, func() time.Time { return time.Unix(0, 0) })
	for i := range keep + 300 { // more than keep plus etcd's 128 operations to a transaction
		_, err := kv.Put(t.Context(), fmt.Sprintf("/naru/orch/events/%020d-seed", i), `{"type":"t"}`)
		require.NoError(t, err)
	}

	require.NoError(t, s.AppendEvent(t.Context(), orchapi.Event{Time: time.Unix(1, 0), Type: "t"}))

	evs, err := s.ListEvents(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, evs, keep)
}

func TestTheEventLogDropsEventsOlderThanTheirAgeByTheStoresClockOnEveryWriteThatRecordsOne(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	invetcd.SetClock(s, func() time.Time { return now })
	at := func(t time.Time) orchapi.Event { return orchapi.Event{Time: t, Type: "t"} }
	times := func() []time.Time {
		evs, err := s.ListEvents(t.Context(), "")
		require.NoError(t, err)
		var out []time.Time
		for _, e := range evs {
			out = append(out, e.Time)
		}
		return out
	}
	aged := now.Add(-invetcd.MaxEventAge)
	for _, ev := range []time.Time{aged.Add(-2 * time.Hour), aged.Add(-time.Hour), aged.Add(time.Hour)} {
		require.NoError(t, s.AppendEvent(t.Context(), at(ev)))
	}
	require.Equal(t, []time.Time{aged.Add(time.Hour)}, times(), "each append drops the events past the age, and the newest stays")

	// A write other than an append records an event too, and bounds the log by age as well.
	require.NoError(t, s.PutProfile(t.Context(), []byte("a: b"), at(now), accept))
	now = now.Add(2 * time.Hour)
	require.Len(t, times(), 2, "the clock moving on drops nothing by itself")
	require.NoError(t, s.PutProfile(t.Context(), []byte("a: c"), at(now), accept))
	require.Equal(t, []time.Time{now.Add(-2 * time.Hour), now}, times(), "a profile write dropped the event the age passed, not only an append")

	// The control: an event dated far ahead, as a clock that jumped forward would date it, does not
	// make the recent events old, because the cutoff is the store's clock.
	require.NoError(t, s.AppendEvent(t.Context(), at(now.Add(30*24*time.Hour))))
	require.Len(t, times(), 3, "recent events stay when an event is dated ahead")
}

// admitPlain admits the host h with no allocation, as a host that enrolled with a configuration of its own is.
func admitPlain(t *testing.T, s *invetcd.Store, h string, now time.Time) {
	t.Helper()
	p := inventory.Pending{HostID: h, PublicKey: []byte("k"), FirstSeen: now}
	require.NoError(t, s.PutPending(t.Context(), p, nil, true))
	require.NoError(t, s.Admit(t.Context(), p, inventory.Host{ID: h, PublicKey: p.PublicKey}, orchapi.Event{Time: now, Type: inventory.EventAdmitted, Subject: h}))
}

func TestAClaimedPrefixIsNeverGivenByAReservationAndAClaimNeverTakesAnotherHostsPrefix(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	pick := func(claimed []netip.Prefix) (netip.Prefix, error) {
		return prefixes.Allocate(netip.MustParsePrefix("10.200.0.0/22"), 24, claimed)
	}
	admitPlain(t, s, "control", now)
	admitPlain(t, s, "other", now)

	// The control: with nothing claimed the pool gives the first prefix, which is what the control host holds.
	first, err := s.Reserve(ctx, inventory.SpaceMachine, "joined-0", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.0.0/24", first.Prefix)
	require.NoError(t, s.Release(ctx, first))

	require.NoError(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.0.0/24", HostID: "control"}))
	require.NoError(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.0.0/24", HostID: "control"}), "a host that reports again changes nothing")
	got, err := s.Reserve(ctx, inventory.SpaceMachine, "joined-1", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.1.0/24", got.Prefix, "the prefix the control host holds is not given again")

	require.ErrorIs(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.0.0/24", HostID: "other"}), inventory.ErrAllocationTaken)
	require.ErrorIs(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.2.0/24", HostID: "gone"}), inventory.ErrNotFound, "a host that is not admitted holds nothing")
	all, err := s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.ElementsMatch(t, []inventory.Allocation{{Prefix: "10.200.0.0/24", HostID: "control"}, got}, all)
}

func TestRemovingAHostFreesEveryPrefixItHeld(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	admitPlain(t, s, "control", now)
	admitPlain(t, s, "kept", now)
	for _, a := range []inventory.Allocation{{Prefix: "10.200.0.0/24", HostID: "control"}, {Prefix: "10.200.1.0/24", HostID: "control"}, {Prefix: "10.200.2.0/24", HostID: "kept"}} {
		require.NoError(t, s.ClaimPrefix(ctx, a))
	}

	_, err := s.RemoveHost(ctx, "control", orchapi.Event{Time: now, Type: inventory.EventRemoved, Subject: "control"})
	require.NoError(t, err)

	all, err := s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.Equal(t, []inventory.Allocation{{Prefix: "10.200.2.0/24", HostID: "kept"}}, all, "the other host keeps its prefix")
}

type noPrefixes struct{}

func (noPrefixes) MachinePrefixes(context.Context) (map[string][]string, error) { return nil, nil }

func TestARecorderPassReleasesTheConfirmedPrefixesOfHostsThatAreNotAdmittedAndNoOtherRecord(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := invetcd.New(kv)
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	admitPlain(t, s, "kept", now)
	admitPlain(t, s, "removed", now)
	require.NoError(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.1.0/24", HostID: "kept"}))
	require.NoError(t, s.ClaimPrefix(ctx, inventory.Allocation{Prefix: "10.200.2.0/24", HostID: "removed"}))
	// A reservation of a host that is only pending is not an orphan: its admission is in flight.
	pick := func([]netip.Prefix) (netip.Prefix, error) { return netip.MustParsePrefix("10.200.3.0/24"), nil }
	reservation, err := s.Reserve(ctx, inventory.SpaceMachine, "pending", pick, now.Add(time.Hour), now)
	require.NoError(t, err)
	// The removal that a claim raced: the host's record is gone and its allocation stayed.
	_, err = kv.Delete(ctx, "/naru/orch/hosts/removed")
	require.NoError(t, err)

	r := inventory.NewRecorder(s, noPrefixes{}, fixedClock{now}, time.Minute, slog.New(slog.DiscardHandler))
	require.NoError(t, r.Reconcile(ctx))

	all, err := s.ListAllocations(ctx, inventory.SpaceMachine)
	require.NoError(t, err)
	require.ElementsMatch(t, []inventory.Allocation{{Prefix: "10.200.1.0/24", HostID: "kept"}, reservation}, all,
		"the removed host's prefix is released; an admitted host's and an in-flight reservation stay")
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// profileSet is the event a profile write records.
func profileSet(now time.Time) orchapi.Event {
	return orchapi.Event{Time: now, Type: inventory.EventProfileSet, Subject: "host-profile"}
}

func TestAMeshAddressIsNeverCommittedFromAProfileThatChangedWhileItWasPicked(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	admitPlain(t, s, "h1", now)
	require.NoError(t, s.PutProfile(ctx, []byte("pool: old"), profileSet(now), accept))

	// The first pick reads the old pool, and the profile is replaced before its write: that write is
	// refused and the next pick, of the new pool, is the one kept.
	picks := 0
	pick := func([]netip.Prefix) (netip.Prefix, error) {
		picks++
		if picks == 1 {
			require.NoError(t, s.PutProfile(ctx, []byte("pool: new"), profileSet(now), accept))
			return netip.MustParsePrefix("10.99.0.1/32"), nil
		}
		return netip.MustParsePrefix("10.98.0.1/32"), nil
	}
	h, err := s.GiveMeshAddress(ctx, "h1", pick)
	require.NoError(t, err)
	require.Equal(t, "10.98.0.1", h.MeshAddress)
	require.Equal(t, 2, picks)

	admitPlain(t, s, "h2", now)
	picks = 0
	res, err := s.Reserve(ctx, inventory.SpaceMesh, "h2", func(claimed []netip.Prefix) (netip.Prefix, error) {
		picks++
		if picks == 1 {
			require.NoError(t, s.PutProfile(ctx, []byte("pool: newer"), profileSet(now), accept))
			return netip.MustParsePrefix("10.98.0.9/32"), nil
		}
		return netip.MustParsePrefix("10.97.0.1/32"), nil
	}, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.97.0.1/32", res.Prefix, "a reservation is refused the same way")
	require.Equal(t, inventory.SpaceMesh, res.Space)

	// The control: in the Machine space the profile is not compared, so the first pick is kept.
	picks = 0
	res, err = s.Reserve(ctx, inventory.SpaceMachine, "h2", func([]netip.Prefix) (netip.Prefix, error) {
		picks++
		require.NoError(t, s.PutProfile(ctx, []byte("pool: newest"), profileSet(now), accept))
		return netip.MustParsePrefix("10.200.1.0/24"), nil
	}, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, 1, picks)
	require.Equal(t, "10.200.1.0/24", res.Prefix)
}

func TestAProfileIsNotWrittenOverAMeshAddressGivenWhileItWasChecked(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	admitPlain(t, s, "h1", now)

	err := s.PutProfile(ctx, []byte("pool: new"), profileSet(now), func([]byte) error {
		_, err := s.GiveMeshAddress(ctx, "h1", func([]netip.Prefix) (netip.Prefix, error) { return netip.MustParsePrefix("10.99.0.1/32"), nil })
		return err
	})
	require.ErrorIs(t, err, inventory.ErrConflict, "the check read the addresses before one was given")
	_, err = s.GetProfile(ctx)
	require.ErrorIs(t, err, inventory.ErrNotFound, "and nothing was written")

	require.NoError(t, s.PutProfile(ctx, []byte("pool: new"), profileSet(now), func([]byte) error { return nil }), "the control: a check nothing raced")
	all, err := s.ListAllocations(ctx, inventory.SpaceMesh)
	require.NoError(t, err)
	require.Equal(t, []inventory.Allocation{{Prefix: "10.99.0.1/32", HostID: "h1", Space: inventory.SpaceMesh}}, all)
}

func TestConcurrentMeshAllocationsNeverGiveAnAddressTwiceAndEveryHostRecordAgrees(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	// Each pick sleeps, so that the calls read the records, pick, and write over one another.
	pick := func(claimed []netip.Prefix) (netip.Prefix, error) {
		time.Sleep(5 * time.Millisecond)
		return prefixes.Allocate(netip.MustParsePrefix("10.99.0.0/24"), 32, claimed)
	}
	const n = 8
	for i := range n {
		admitPlain(t, s, fmt.Sprintf("g%d", i), now)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := s.Reserve(ctx, inventory.SpaceMesh, fmt.Sprintf("r%d", i), pick, now.Add(time.Minute), now)
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := s.GiveMeshAddress(ctx, fmt.Sprintf("g%d", i), pick)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	ok := 0
	for err := range errs {
		if err == nil {
			ok++
			continue
		}
		require.ErrorIs(t, err, inventory.ErrConflict, "a call that lost every compare gives up, and fails no other way")
	}
	require.Positive(t, ok, "the calls allocated")

	all, err := s.ListAllocations(ctx, inventory.SpaceMesh)
	require.NoError(t, err)
	held := map[string]string{}
	for _, a := range all {
		require.NotContains(t, held, a.Prefix, "%s is held twice", a.Prefix)
		held[a.Prefix] = a.HostID
	}
	require.Equal(t, ok, len(all), "every call that succeeded left exactly one record, and no other did")
	hosts, err := s.ListHosts(ctx)
	require.NoError(t, err)
	for _, h := range hosts {
		if h.MeshAddress != "" {
			require.Equal(t, h.ID, held[h.MeshAddress+"/32"], "the record of %s agrees with the allocation", h.ID)
		}
	}
}

// accept is a profile check that passes whatever profile it replaces.
func accept([]byte) error { return nil }

func TestAProfileIsNotWrittenOverAnotherWrittenWhileItWasChecked(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := invetcd.New(c.Members[0].Etcd())
	ctx := t.Context()
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	require.NoError(t, s.PutProfile(ctx, []byte("pool: x"), profileSet(now), accept))

	var replaced []byte
	err := s.PutProfile(ctx, []byte("pool: x, registry edited"), profileSet(now), func(current []byte) error {
		replaced = current
		return s.PutProfile(ctx, []byte("pool: y"), profileSet(now), accept)
	})
	require.ErrorIs(t, err, inventory.ErrConflict, "the profile it was checked against changed")
	assert.Equal(t, "pool: x", string(replaced), "the check is given the profile the write replaces")
	got, err := s.GetProfile(ctx)
	require.NoError(t, err)
	assert.Equal(t, "pool: y", string(got), "the other write stands")

	require.NoError(t, s.PutProfile(ctx, []byte("pool: y, registry edited"), profileSet(now), func(current []byte) error {
		replaced = current
		return nil
	}), "the control: the same write against the profile as it is")
	assert.Equal(t, "pool: y", string(replaced))
}

func TestAnAbandonmentWritesTheHostsAbandonedKeyAndARemovalDeletesIt(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := invetcd.New(kv)
	ctx := t.Context()
	now := time.Date(2026, 11, 28, 0, 0, 0, 0, time.UTC)
	admitPlain(t, s, "gone", now)
	admitPlain(t, s, "kept", now)
	present := func(id string) bool {
		resp, err := kv.Get(ctx, invetcd.AbandonedHostPrefix+id)
		require.NoError(t, err)
		return resp.Count == 1
	}

	_, err := s.UpdateHost(ctx, "gone", func(h *inventory.Host) { h.LastSeen = now }, nil)
	require.NoError(t, err)
	assert.False(t, present("gone"), "control: a write that abandons nothing writes no key")
	_, err = s.UpdateHost(ctx, "gone", func(h *inventory.Host) {
		h.Abandonment = &orchapi.HostAbandonment{By: "alice", At: now, Fenced: "off"}
	}, nil)
	require.NoError(t, err)
	assert.True(t, present("gone"), "written with the abandonment")
	assert.False(t, present("kept"))

	_, err = s.RemoveHost(ctx, "gone", orchapi.Event{Time: now, Type: inventory.EventRemoved, Subject: "gone"})
	require.NoError(t, err)
	assert.False(t, present("gone"), "deleted with the host, so one admitted again under its ID is placed on")
}
