package etcd_test

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
	machinesetcd "github.com/garamsh/naru/orch/internal/machines/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// Two plans that read the same allocation commit one binding: the last room is given once.
func TestBind_CommitsOneOfTwoBindingsPlannedFromTheSameAllocation(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	a, b := record("garam", "a", "000000000000000a"), record("garam", "b", "000000000000000b")
	for _, r := range []machines.Record{a, b} {
		require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	}
	_, arev, err := s.Machine(ctx, "garam", "a")
	require.NoError(t, err)
	_, brev, err := s.Machine(ctx, "garam", "b")
	require.NoError(t, err)
	allocs, err := s.Allocations(ctx)
	require.NoError(t, err)
	plan := allocs["h-a"] // none yet: revision 0

	a.Binding, b.Binding = &machines.Binding{Host: "h-a", Epoch: 1}, &machines.Binding{Host: "h-a", Epoch: 1}
	take := hostapi.Resources{CPU: 2, Memory: 4 << 30, Disk: 30 << 30}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, c := range []struct {
		r   machines.Record
		rev int64
	}{{a, arev}, {b, brev}} {
		wg.Go(func() {
			_, errs[i] = s.Bind(ctx, c.r, c.rev, machines.Allocation{Resources: take, Rev: plan.Rev})
		})
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
		} else {
			assert.ErrorIs(t, err, machines.ErrRaced)
		}
	}
	assert.Equal(t, 1, won, "one commits")
	allocs, err = s.Allocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, take, allocs["h-a"].Resources, "once")

	bound := 0
	for _, n := range []string{"a", "b"} {
		r, _, err := s.Machine(ctx, "garam", n)
		require.NoError(t, err)
		if r.Binding != nil {
			bound++
		}
	}
	assert.Equal(t, 1, bound, "the loser's record is not bound")
}

func TestBind_RefusesARecordThatChangedSinceItWasRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	deleting := r
	deleting.Status.Phase = machineapi.PhaseDeleting
	require.NoError(t, update(ctx, s, deleting, rev, time.Now()))

	r.Binding = &machines.Binding{Host: "h-a", Epoch: 1}
	_, err = s.Bind(ctx, r, rev, machines.Allocation{Resources: hostapi.Resources{CPU: 1}})
	require.ErrorIs(t, err, machines.ErrRaced, "a Machine marked deleting since the plan read it")
	allocs, err := s.Allocations(ctx)
	require.NoError(t, err)
	assert.Empty(t, allocs, "and nothing is allocated")
}

func TestRemove_FreesTheNameTheIDAndTheAllocationWithItsEventAndLeavesATombstone(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	r.Binding = &machines.Binding{Host: "h-a", Epoch: 1}
	at, err := s.Bind(ctx, r, rev, machines.Allocation{Resources: hostapi.Resources{CPU: 2, Memory: 4 << 30}})
	require.NoError(t, err)

	_, err = s.Remove(ctx, r, at, &machines.Allocation{Resources: hostapi.Resources{CPU: 1}, Rev: at - 1}, machineapi.Event{Type: machineapi.EventDeleted, Machine: "web", ID: r.ID, Time: time.Now()})
	require.ErrorIs(t, err, machines.ErrRaced, "an allocation that changed since it was read")
	removed := time.Now().UTC().Round(0)
	_, err = s.Remove(ctx, r, at, &machines.Allocation{Rev: at}, machineapi.Event{Type: machineapi.EventDeleted, Machine: "web", ID: r.ID, Time: removed})
	require.NoError(t, err)

	_, _, err = s.Machine(ctx, "garam", "web")
	require.ErrorIs(t, err, machines.ErrNotFound)
	tombs, err := s.Tombstones(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[string]machines.Tombstone{r.ID: {ID: r.ID, Removed: removed}}, tombs, "the removal leaves its tombstone, dated by its event")
	require.NoError(t, s.DropTombstone(ctx, r.ID))
	tombs, err = s.Tombstones(ctx)
	require.NoError(t, err)
	assert.Empty(t, tombs)
	again := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, again, 0, event(again, time.Now())), "the name and the id are free")
	allocs, err := s.Allocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, hostapi.Resources{}, allocs["h-a"].Resources)
	evs, _, err := s.Events(ctx, "garam", nil)
	require.NoError(t, err)
	types := []machineapi.EventType{}
	for _, e := range evs {
		types = append(types, e.Type)
	}
	assert.Equal(t, []machineapi.EventType{machineapi.EventStatus, machineapi.EventDeleted, machineapi.EventStatus}, types)
}

func TestDeleteSnapshot_LeavesADeletionOfTheDiskSnapshotsOnItsHost(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	src := record("garam", "src", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, src, 0, event(src, time.Now())))
	_, mrev, err := s.Machine(ctx, "garam", "src")
	require.NoError(t, err)
	vols := machineapi.Spec{Volumes: []machineapi.Volume{{Name: "a"}, {Name: "b"}}}
	taken := machines.Snapshot{Caller: "garam", ID: "00000000000000aa", Host: "h-a", Snapshot: machineapi.Snapshot{Name: "taken", Machine: "src", MachineID: src.ID, Spec: vols}}
	untaken := machines.Snapshot{Caller: "garam", ID: "00000000000000bb", Snapshot: machineapi.Snapshot{Name: "untaken", Machine: "src", MachineID: src.ID, Spec: vols}}
	for _, snap := range []machines.Snapshot{taken, untaken} {
		require.NoError(t, s.CreateSnapshot(ctx, snap, mrev))
		_, mrev, err = s.Machine(ctx, "garam", "src")
		require.NoError(t, err)
	}
	all, err := s.AllSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)

	for _, e := range all {
		_, err := s.UpdateSnapshot(ctx, e.Snapshot, e.Rev-1, nil)
		require.ErrorIs(t, err, machines.ErrRaced, "an update of a snapshot that changed since it was read")
		require.NoError(t, s.DeleteSnapshot(ctx, e.Snapshot, e.Rev))
	}
	dels, err := s.Deletions(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machines.Deletion{{ID: "00000000000000aa", Host: "h-a", Disks: []string{"s-00000000000000aa-0", "s-00000000000000aa-1"}}}, dels,
		"one for the snapshot taken on a host, none for one that never was")
	require.NoError(t, s.DropDeletion(ctx, "00000000000000aa"))
	dels, err = s.Deletions(ctx)
	require.NoError(t, err)
	assert.Empty(t, dels)
}

func TestUpdateSnapshot_WritesTheDeletionOfAFailedSnapshotWithIt(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	src := record("garam", "src", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, src, 0, event(src, time.Now())))
	_, mrev, err := s.Machine(ctx, "garam", "src")
	require.NoError(t, err)
	snap := machines.Snapshot{Caller: "garam", ID: "00000000000000aa", Host: "h-a", Snapshot: machineapi.Snapshot{Name: "s", Machine: "src", MachineID: src.ID, Spec: machineapi.Spec{Volumes: []machineapi.Volume{{Name: "a"}}}}}
	require.NoError(t, s.CreateSnapshot(ctx, snap, mrev))
	_, rev, err := s.Snapshot(ctx, "garam", "s")
	require.NoError(t, err)
	del := machines.Deletion{ID: snap.ID, Host: "h-a", Disks: snap.DiskSnapshots()}

	snap.Snapshot.Phase = machineapi.SnapshotFailed
	_, err = s.UpdateSnapshot(ctx, snap, rev-1, &del)
	require.ErrorIs(t, err, machines.ErrRaced)
	dels, err := s.Deletions(ctx)
	require.NoError(t, err)
	assert.Empty(t, dels, "nothing of a refused write")
	_, err = s.UpdateSnapshot(ctx, snap, rev, &del)
	require.NoError(t, err)
	dels, err = s.Deletions(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machines.Deletion{del}, dels)

	del.Done = time.Unix(1700000000, 0).UTC()
	require.NoError(t, s.PutDeletion(ctx, del))
	dels, err = s.Deletions(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machines.Deletion{del}, dels, "kept once done, as the tombstone")
}

// waker reads a Watch channel.
type waker struct {
	t    *testing.T
	wake <-chan struct{}
}

func (w waker) quiet(what string) {
	w.t.Helper()
	select {
	case <-w.wake:
		w.t.Fatalf("a wake after %s", what)
	case <-time.After(300 * time.Millisecond):
	}
}

func (w waker) woken(what string, within time.Duration) {
	w.t.Helper()
	select {
	case _, open := <-w.wake:
		if !open {
			w.t.Fatalf("the channel closed, not woken, after %s", what)
		}
	case <-time.After(within):
		w.t.Fatalf("no wake after %s", what)
	}
}

func TestWatch_WakesOnTheMachineAPIsWritesAndNotOnTheReconcilersOwn(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := machinesetcd.New(kv)
	ctx := t.Context()
	w := waker{t, machinesetcd.Watch(ctx, kv, slog.New(slog.DiscardHandler))}
	w.quiet("nothing")
	_, err := kv.Put(ctx, machinesetcd.AllocationPrefix+"h-a", "{}")
	require.NoError(t, err)
	w.quiet("an allocation")

	r := record("garam", "web", "0123456789abcdef")
	r.Status.Generation = 1
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	w.woken("a Machine's create", 5*time.Second)
	w.quiet("the one wake of the create")

	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	r.Status.Phase = machineapi.PhaseRunning
	r.Binding = &machines.Binding{Host: "h-a", Epoch: 1}
	rev, err = s.Update(ctx, r, rev, nil)
	require.NoError(t, err)
	w.quiet("a phase and a binding, which the reconciler writes")

	r.Status.Generation = 2
	rev, err = s.Update(ctx, r, rev, nil)
	require.NoError(t, err)
	w.woken("a new generation, which a start or stop writes", 5*time.Second)

	r.Status.Phase = machineapi.PhaseDeleting
	rev, err = s.Update(ctx, r, rev, nil)
	require.NoError(t, err)
	w.woken("the mark of a delete", 5*time.Second)

	snap := machines.Snapshot{Caller: "garam", ID: "00000000000000aa", Snapshot: machineapi.Snapshot{Name: "s", Machine: "web", MachineID: r.ID}}
	require.NoError(t, s.CreateSnapshot(ctx, snap, rev))
	w.woken("a snapshot's create", 5*time.Second)
	_, srev, err := s.Snapshot(ctx, "garam", "s")
	require.NoError(t, err)
	snap.Host = "h-a"
	srev, err = s.UpdateSnapshot(ctx, snap, srev, nil)
	require.NoError(t, err)
	w.quiet("a snapshot's host, which the reconciler writes")
	require.NoError(t, s.DeleteSnapshot(ctx, snap, srev))
	w.woken("a snapshot's delete", 5*time.Second)

	_, err = s.Remove(ctx, r, rev, nil, machineapi.Event{Type: machineapi.EventDeleted, Machine: "web", ID: r.ID, Time: time.Now()})
	require.NoError(t, err)
	w.quiet("a removal, which the reconciler writes")
}

// closingWatcher closes its first watch at once, as a watch whose member lost its leader closes,
// and serves the others from the real watcher.
type closingWatcher struct {
	clientv3.Watcher
	mu     sync.Mutex
	closed int
}

func (c *closingWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed < 2 {
		c.closed++
		ch := make(chan clientv3.WatchResponse)
		close(ch)
		return ch
	}
	return c.Watcher.Watch(ctx, key, opts...)
}

func TestWatch_OpensAClosedWatchAgainAndWakes(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := machinesetcd.New(kv)
	var logs bytes.Buffer
	w := waker{t, machinesetcd.Watch(t.Context(), &closingWatcher{Watcher: kv}, slog.New(slog.NewTextHandler(&logs, nil)))}

	w.woken("the watch opened again", 5*time.Second)
	assert.Contains(t, logs.String(), "the Machine watch closed", "the fallback is logged")
	assert.Contains(t, logs.String(), "the Machine watch is open again")
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(t.Context(), r, 0, event(r, time.Now())))
	w.woken("a Machine's create on the watch opened again", 5*time.Second)
}

// A move's write of its record and another host's allocation commits both or neither, guarded on
// the record and on that allocation as read.
func TestAllocate_WritesTheRecordAndTheHostsAllocationTogether(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	take := hostapi.Resources{CPU: 2, Memory: 4 << 30, Disk: 30 << 30}

	r.Move = &machines.Move{By: "alice", Step: machines.MoveReserved, Source: machines.Binding{Host: "h-a", Epoch: 1}, Target: &machines.Binding{Host: "h-b", Epoch: 2}}
	r.Epoch = 2
	moving := r
	moving.Status.Phase = machineapi.PhaseMoving
	ev := event(moving, time.Now())
	at, err := s.Allocate(ctx, moving, rev, "h-b", machines.Allocation{Resources: take}, &ev)
	require.NoError(t, err)
	got, gotRev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	assert.Equal(t, at, gotRev)
	assert.Equal(t, moving.Move, got.Move, "the move is recorded")
	assert.Equal(t, uint64(2), got.Epoch)
	allocs, err := s.Allocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, take, allocs["h-b"].Resources, "with the target's allocation")
	evs, _, err := s.Events(ctx, "garam", nil)
	require.NoError(t, err)
	assert.Equal(t, machineapi.PhaseMoving, evs[len(evs)-1].Status.Phase, "and its event")

	// The record changed since it was read: neither is written.
	_, err = s.Allocate(ctx, got, rev, "h-b", machines.Allocation{Resources: hostapi.Resources{}, Rev: allocs["h-b"].Rev}, nil)
	require.ErrorIs(t, err, machines.ErrRaced)
	// The allocation changed since it was read: neither is written.
	got.Move = nil
	_, err = s.Allocate(ctx, got, gotRev, "h-b", machines.Allocation{Resources: hostapi.Resources{}}, nil)
	require.ErrorIs(t, err, machines.ErrRaced)
	after, _, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	assert.NotNil(t, after.Move, "the record is as it was")
	allocs, err = s.Allocations(ctx)
	require.NoError(t, err)
	assert.Equal(t, take, allocs["h-b"].Resources, "and so is the allocation")
}
