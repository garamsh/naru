package etcd_test

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestRemove_FreesTheNameTheIDAndTheAllocationWithItsEvent(t *testing.T) {
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
	_, err = s.Remove(ctx, r, at, &machines.Allocation{Rev: at}, machineapi.Event{Type: machineapi.EventDeleted, Machine: "web", ID: r.ID, Time: time.Now()})
	require.NoError(t, err)

	_, _, err = s.Machine(ctx, "garam", "web")
	require.ErrorIs(t, err, machines.ErrNotFound)
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
		_, err := s.UpdateSnapshot(ctx, e.Snapshot, e.Rev-1)
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

func TestWatch_WakesOnAWriteOfAMachineOrASnapshotAndNothingElse(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	s := machinesetcd.New(kv)
	ctx := t.Context()
	wake := machinesetcd.Watch(ctx, c.Members[0].Etcd())
	quiet := func(what string) {
		select {
		case <-wake:
			t.Fatalf("a wake after %s", what)
		case <-time.After(200 * time.Millisecond):
		}
	}
	woken := func(what string) {
		select {
		case <-wake:
		case <-time.After(5 * time.Second):
			t.Fatalf("no wake after %s", what)
		}
	}
	quiet("nothing")
	_, err := kv.Put(ctx, machinesetcd.AllocationPrefix+"h-a", "{}")
	require.NoError(t, err)
	quiet("an allocation")
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	woken("a Machine")
	quiet("the one wake of the Machine")
	_, mrev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.NoError(t, s.CreateSnapshot(ctx, machines.Snapshot{Caller: "garam", ID: "00000000000000aa", Snapshot: machineapi.Snapshot{Name: "s", Machine: "web", MachineID: r.ID}}, mrev))
	woken("a snapshot")
}
