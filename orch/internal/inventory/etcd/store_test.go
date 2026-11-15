package etcd_test

import (
	"context"
	"encoding/json"
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
