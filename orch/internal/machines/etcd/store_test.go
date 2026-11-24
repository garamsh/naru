package etcd_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
	machinesetcd "github.com/garamsh/naru/orch/internal/machines/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func record(caller, name, id string) machines.Record {
	return machines.Record{Caller: caller, Name: name, ID: id, Status: machineapi.Status{Phase: machineapi.PhasePending}}
}

func event(r machines.Record, at time.Time) machineapi.Event {
	return machineapi.Event{Type: machineapi.EventStatus, Machine: r.Name, ID: r.ID, Status: r.Status, Time: at}
}

func newStore(t *testing.T) *machinesetcd.Store {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	return machinesetcd.New(c.Members[0].Etcd())
}

func TestCreate_RefusesATakenNameOrID(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, now)))
	assert.ErrorIs(t, s.Create(ctx, record("garam", "web", "fedcba9876543210"), 0, event(r, now)), machines.ErrRaced, "the caller's name")
	assert.ErrorIs(t, s.Create(ctx, record("functions", "api", "0123456789abcdef"), 0, event(r, now)), machines.ErrRaced, "another caller's id")
	require.NoError(t, s.Create(ctx, record("functions", "web", "fedcba9876543210"), 0, event(r, now)), "control: the name under another caller with a new id")
}

func TestUpdate_RefusesARecordThatChangedSinceItWasRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.NoError(t, s.Update(ctx, r, rev, event(r, time.Now())), "the record as read")
	assert.ErrorIs(t, s.Update(ctx, r, rev, event(r, time.Now())), machines.ErrRaced, "the same revision again")
}

func TestEvents_ACursorOlderThanTheLogKeepsIsExpired(t *testing.T) {
	s := newStore(t)
	machinesetcd.KeepEvents(s, 2)
	ctx := t.Context()
	_, start, err := s.Machines(ctx, "garam")
	require.NoError(t, err)
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	for range 3 {
		_, rev, err := s.Machine(ctx, "garam", "web")
		require.NoError(t, err)
		require.NoError(t, s.Update(ctx, r, rev, event(r, time.Now())))
	}
	all, _, err := s.Events(ctx, "garam", nil)
	require.NoError(t, err)
	require.Len(t, all, 2, "the log keeps its newest 2")

	_, _, err = s.Events(ctx, "garam", &start)
	assert.ErrorIs(t, err, machines.ErrCursorExpired, "a cursor before an event the log dropped")
	_, last, err := s.Machines(ctx, "garam")
	require.NoError(t, err)
	page, _, err := s.Events(ctx, "garam", &last)
	require.NoError(t, err, "a cursor after the drop is served")
	assert.Empty(t, page)
	other := record("functions", "api", "fedcba9876543210")
	require.NoError(t, s.Create(ctx, other, 0, event(other, time.Now())))
	_, _, err = s.Events(ctx, "functions", &start)
	require.NoError(t, err, "another caller's log dropped nothing")
}

func TestEvents_DropsEventsOlderThanTheAgeBound(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	now := time.Now()
	machinesetcd.SetClock(s, func() time.Time { return now })
	_, start, err := s.Machines(ctx, "garam")
	require.NoError(t, err)
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, now.Add(-machinesetcd.MaxEventAge-time.Hour))))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.NoError(t, s.Update(ctx, r, rev, event(r, now)))

	all, _, err := s.Events(ctx, "garam", nil)
	require.NoError(t, err)
	require.Len(t, all, 1, "the event older than the bound is dropped")
	assert.Equal(t, now.UTC(), all[0].Time.UTC())
	_, _, err = s.Events(ctx, "garam", &start)
	assert.ErrorIs(t, err, machines.ErrCursorExpired)
}

func TestEvents_RefusesACursorAfterTheLog(t *testing.T) {
	s := newStore(t)
	_, rev, err := s.Machines(t.Context(), "garam")
	require.NoError(t, err)
	_, _, err = s.Events(t.Context(), "garam", &rev)
	require.NoError(t, err, "the cursor a list gave")
	ahead := rev + 1000
	_, _, err = s.Events(t.Context(), "garam", &ahead)
	assert.ErrorIs(t, err, machines.ErrBadCursor, strconv.FormatInt(ahead, 10))
}

func TestCreate_RefusesARestoreFromASnapshotThatChangedSinceItWasRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	src := record("garam", "src", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, src, 0, event(src, time.Now())))
	_, mrev, err := s.Machine(ctx, "garam", "src")
	require.NoError(t, err)
	snap := machines.Snapshot{Caller: "garam", Snapshot: machineapi.Snapshot{Name: "before", Machine: "src", MachineID: src.ID}}
	require.NoError(t, s.CreateSnapshot(ctx, snap, mrev))
	_, srev, err := s.Snapshot(ctx, "garam", "before")
	require.NoError(t, err)

	restore := record("garam", "restored", "fedcba9876543210")
	restore.Spec.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "before"}
	require.NoError(t, s.DeleteSnapshot(ctx, "garam", "before"))
	assert.ErrorIs(t, s.Create(ctx, restore, srev, event(restore, time.Now())), machines.ErrRaced, "the snapshot deleted since it was read")
	_, _, err = s.Machine(ctx, "garam", "restored")
	assert.ErrorIs(t, err, machines.ErrNotFound, "nothing was written")

	require.NoError(t, s.CreateSnapshot(ctx, snap, mrev))
	_, srev, err = s.Snapshot(ctx, "garam", "before")
	require.NoError(t, err)
	require.NoError(t, s.Create(ctx, restore, srev, event(restore, time.Now())), "control: the snapshot as read")
}

func TestCreateSnapshot_RefusesAMachineThatChangedSinceItWasRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := record("garam", "web", "0123456789abcdef")
	require.NoError(t, s.Create(ctx, r, 0, event(r, time.Now())))
	_, rev, err := s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	r.Status.Phase = machineapi.PhaseDeleting
	require.NoError(t, s.Update(ctx, r, rev, event(r, time.Now())))

	snap := machines.Snapshot{Caller: "garam", Snapshot: machineapi.Snapshot{Name: "before", Machine: "web", MachineID: r.ID}}
	assert.ErrorIs(t, s.CreateSnapshot(ctx, snap, rev), machines.ErrRaced, "the Machine marked deleting since it was read")
	_, _, err = s.Snapshot(ctx, "garam", "before")
	assert.ErrorIs(t, err, machines.ErrNotFound, "nothing was written")
	_, rev, err = s.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.NoError(t, s.CreateSnapshot(ctx, snap, rev), "control: the Machine as read")
}

func TestClaimZone_KeepsTheZoneFirstClaimed(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	got, err := s.ClaimZone(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, got, "none recorded, and an empty zone claims nothing")
	got, err = s.ClaimZone(ctx, "m.example.test")
	require.NoError(t, err)
	assert.Equal(t, "m.example.test", got, "the first claim")
	got, err = s.ClaimZone(ctx, "other.example.test")
	require.NoError(t, err)
	assert.Equal(t, "m.example.test", got, "a later claim of another zone gets the first")
	got, err = s.ClaimZone(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, "m.example.test", got, "an empty zone reads it")
}
