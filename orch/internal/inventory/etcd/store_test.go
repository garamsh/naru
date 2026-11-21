package etcd_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"
	"time"

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

	require.NoError(t, s.Admit(ctx, p, inventory.Host{ID: "h1", PublicKey: p.PublicKey}, ev, nil))
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

	a, err := s.Reserve(ctx, "h-a", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.0.0/24", a.Prefix)
	b, err := s.Reserve(ctx, "h-b", pick, now.Add(time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, "10.200.1.0/24", b.Prefix)
	again, err := s.Reserve(ctx, "h-a", pick, now.Add(2*time.Minute), now)
	require.NoError(t, err)
	require.Equal(t, a.Prefix, again.Prefix, "a host that asks again keeps its reservation")

	later := now.Add(3 * time.Minute)
	c2, err := s.Reserve(ctx, "h-c", pick, later.Add(time.Minute), later)
	require.NoError(t, err)
	require.Equal(t, "10.200.0.0/24", c2.Prefix, "an expired reservation is taken over")
	all, err := s.ListAllocations(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the expired reservations are dropped, not left beside the new one")

	require.NoError(t, s.Release(ctx, a), "a stale holder's release")
	all, err = s.ListAllocations(ctx)
	require.NoError(t, err)
	require.Equal(t, []inventory.Allocation{c2}, all, "does not drop what another host holds now")
	require.NoError(t, s.Release(ctx, c2))
	all, err = s.ListAllocations(ctx)
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
	require.NoError(t, s.PutProfile(t.Context(), []byte("a: b"), at(now)))
	now = now.Add(2 * time.Hour)
	require.Len(t, times(), 2, "the clock moving on drops nothing by itself")
	require.NoError(t, s.PutProfile(t.Context(), []byte("a: c"), at(now)))
	require.Equal(t, []time.Time{now.Add(-2 * time.Hour), now}, times(), "a profile write dropped the event the age passed, not only an append")

	// The control: an event dated far ahead, as a clock that jumped forward would date it, does not
	// make the recent events old, because the cutoff is the store's clock.
	require.NoError(t, s.AppendEvent(t.Context(), at(now.Add(30*24*time.Hour))))
	require.Len(t, times(), 3, "recent events stay when an event is dated ahead")
}
