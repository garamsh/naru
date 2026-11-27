package machines_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// placedOnA is a rig of h-a, which takes the Machine web as the larger host, and h-b, with web
// running on h-a and a file on its volume.
func placedOnA(t *testing.T) (*rig, machineapi.Machine) {
	t.Helper()
	r := newRig(t, host("h-a", 64*gib), host("h-b", 32*gib))
	m := r.create("web", spec())
	r.pass()
	require.Equal(t, "h-a", r.record("web").Binding.Host)
	require.NoError(t, r.svc.WriteFile(t.Context(), "garam", "web", "/data/state", []byte("kept")))
	return r, m
}

func (r *rig) move(req orchapi.MoveRequest) orchapi.Machine {
	r.t.Helper()
	m, err := r.svc.Move(r.t.Context(), "operator", "garam", "web", req)
	require.NoError(r.t, err)
	return m
}

// moved checks the Machine runs on h-b at epoch 2 with its volume's file, and h-a holds nothing of it.
func (r *rig) moved(m machineapi.Machine) {
	r.t.Helper()
	rec := r.record("web")
	assert.Nil(r.t, rec.Move, "the move ended")
	require.NotNil(r.t, rec.Binding)
	assert.Equal(r.t, machines.Binding{Host: "h-b", Epoch: 2}, *rec.Binding)
	assert.Equal(r.t, machineapi.PhaseRunning, rec.Status.Phase)
	assert.Empty(r.t, r.hosts.MicroVMs("h-a"), "the source's copy is gone")
	assert.Equal(r.t, "running", r.hosts.MicroVMs("h-b")["m-"+m.ID+"-2"].State)
	got, err := r.svc.ReadFile(r.t.Context(), "garam", "web", "/data/state")
	require.NoError(r.t, err)
	assert.Equal(r.t, []byte("kept"), got, "the volume came with it")
	assert.Equal(r.t, hostapi.Resources{}, r.allocation("h-a"), "the source's capacity is freed")
	assert.Equal(r.t, hostapi.Resources{CPU: 2, Memory: 4 * gib, Disk: 30 * gib}, r.allocation("h-b"))
}

func TestMove_StopsCopiesAndStartsTheMachineOnAnotherHost(t *testing.T) {
	r, m := placedOnA(t)
	got := r.move(orchapi.MoveRequest{})
	require.NotNil(t, got.Move)
	assert.Equal(t, orchapi.MoveRequested, got.Move.Step)
	assert.Equal(t, "h-a", got.Move.Source)
	r.pass()

	r.moved(m)
	creates := r.hosts.Creates("h-b")
	require.Len(t, creates, 1)
	assert.True(t, creates[0].NoStart, "the target is made stopped and started after the commit")
	assert.Equal(t, []hostapi.DiskSpec{{Name: "data", SizeBytes: 20 * gib, MountPath: "/data", Imported: true}}, creates[0].Disks, "on the imported volume")
	assert.Equal(t, []uint32{0}, r.hosts.Graces("h-a"), "the source was stopped first")
	phases, _ := r.phases(m.ID)
	assert.Equal(t, []machineapi.Phase{machineapi.PhasePending, machineapi.PhaseStarting, machineapi.PhaseRunning, machineapi.PhaseMoving, machineapi.PhaseRunning}, phases)
	ev := r.fleet.of(machines.EventMoved)
	require.Len(t, ev, 1)
	assert.Equal(t, map[string]string{"id": m.ID, "from": "h-a", "to": "h-b", "by": "operator"}, ev[0].Detail)
}

func TestMove_LeavesAMachineDesiredStoppedStopped(t *testing.T) {
	r, m := placedOnA(t)
	_, err := r.svc.Stop(t.Context(), "garam", "web", machineapi.StopRequest{})
	require.NoError(t, err)
	r.pass()
	r.move(orchapi.MoveRequest{})
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, "h-b", rec.Binding.Host)
	assert.Equal(t, machineapi.PhaseStopped, rec.Status.Phase)
	assert.Equal(t, "created", r.hosts.MicroVMs("h-b")["m-"+m.ID+"-2"].State, "never started")
	assert.Empty(t, r.hosts.MicroVMs("h-a"))
}

// killing fails every write after the first n, as a leader that dies there leaves the store.
type killing struct {
	machines.Store
	mu     sync.Mutex
	writes int
	n      int
}

var errKilled = errors.New("the leader died")

func (k *killing) write() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.writes++
	if k.writes > k.n {
		return errKilled
	}
	return nil
}

func (k *killing) Update(ctx context.Context, rec machines.Record, rev int64, ev *machineapi.Event) (int64, error) {
	if err := k.write(); err != nil {
		return 0, err
	}
	return k.Store.Update(ctx, rec, rev, ev)
}

func (k *killing) Allocate(ctx context.Context, rec machines.Record, rev int64, host string, a machines.Allocation, ev *machineapi.Event) (int64, error) {
	if err := k.write(); err != nil {
		return 0, err
	}
	return k.Store.Allocate(ctx, rec, rev, host, a, ev)
}

func TestMove_ANewLeaderFinishesAMoveTheOldOneLeftAtAnyStep(t *testing.T) {
	for n := 0; ; n++ {
		r, m := placedOnA(t)
		r.move(orchapi.MoveRequest{})
		k := &killing{Store: r.store, n: n}
		old := machines.NewReconciler(machines.ReconcilerConfig{Store: k, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet})
		err := old.Reconcile(t.Context())
		if k.writes <= n {
			// The old leader finished with no write failed: every step was killed once.
			require.NoError(t, err)
			require.Greater(t, n, 5, "the move has a write per step")
			r.moved(m)
			return
		}
		require.ErrorIs(t, err, errKilled, "after %d writes", n)
		step := "none"
		if mv := r.record("web").Move; mv != nil {
			step = string(mv.Step)
		}
		// A new leader is a new reconciler over the same store and hosts.
		r.rec = machines.NewReconciler(machines.ReconcilerConfig{Store: r.store, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet})
		r.pass()
		t.Run(step, func(t *testing.T) { r.t = t; r.moved(m) })
	}
}

func TestMove_CopiesAVolumeAgainAfterTheTargetRefusedItsStream(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusConflict, Code: hostapi.CodeDigestMismatch, Message: "the stream's digest is not the request's"})
	r.move(orchapi.MoveRequest{})
	r.pass()

	rec := r.record("web")
	require.NotNil(t, rec.Move)
	assert.Equal(t, machines.MoveStopped, rec.Move.Step, "the move waits at the copy")
	assert.Contains(t, rec.Move.Reason, "the copy of the volume data failed and is tried again")
	assert.Equal(t, "h-a", rec.Binding.Host, "the Machine is still the source's")
	assert.Equal(t, machineapi.PhaseMoving, rec.Status.Phase)

	r.hosts.RefuseImports("h-b", nil)
	r.pass()
	r.moved(m)
	exports, imports := r.hosts.Transfers("h-a")
	assert.Equal(t, 2, exports, "a new grant for the second copy")
	assert.Zero(t, imports)
}

func TestMove_ACancelBeforeTheCommitStartsTheSourceAgainAndFreesTheTarget(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	r.move(orchapi.MoveRequest{})
	r.pass()
	r.pass()
	require.Equal(t, hostapi.Resources{CPU: 2, Memory: 4 * gib, Disk: 30 * gib}, r.allocation("h-b"), "the target's capacity is held")
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "the host's work leaves the stopped source to the move")
	assert.Empty(t, r.fleet.of(machines.EventAllocationDrift), "the move's reservation is accounted")
	assert.Empty(t, r.fleet.of(machines.EventRestart))
	got, err := r.svc.CancelMove(t.Context(), "garam", "web")
	require.NoError(t, err)
	assert.True(t, got.Move.Cancelled)
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machines.Binding{Host: "h-a", Epoch: 1}, *rec.Binding)
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase)
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "the source runs again")
	assert.Empty(t, r.hosts.MicroVMs("h-b"))
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"), "the target's capacity is freed")
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, "stopped", ev[0].Detail["step"])
	assert.Equal(t, "the operator cancelled the move", ev[0].Detail["reason"])
}

func TestMove_ATargetThatRefusesTheMachineEndsTheMoveOnTheSource(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.Refuse("h-b", &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "image: not found"})
	r.move(orchapi.MoveRequest{})
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, "h-a", rec.Binding.Host)
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"))
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, "the target refused the Machine: image: not found", ev[0].Detail["reason"])
}

func TestMove_ADeleteBeforeTheCommitEndsTheMoveAndThenTheMachine(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	r.move(orchapi.MoveRequest{})
	r.pass()
	_, err := r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	r.pass()

	_, _, err = r.store.Machine(t.Context(), "garam", "web")
	assert.ErrorIs(t, err, machines.ErrNotFound, "the Machine is gone")
	assert.Empty(t, r.hosts.MicroVMs("h-a"))
	assert.Empty(t, r.hosts.MicroVMs("h-b"))
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-a"))
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"))
	_, deleted := r.phases(m.ID)
	assert.True(t, deleted)
}

func TestMove_WaitsWhileNoOtherHostFitsAndSaysWhy(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 4*gib))
	r.create("web", spec())
	r.pass()
	r.move(orchapi.MoveRequest{})
	r.pass()

	rec := r.record("web")
	require.NotNil(t, rec.Move)
	assert.Equal(t, machines.MoveRequested, rec.Move.Step)
	assert.Contains(t, rec.Move.Reason, "memory")
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase, "the Machine runs on where it is")

	_, err := r.svc.CancelMove(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	assert.Nil(t, r.record("web").Move, "a cancel ends a move that reserved nothing")
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, "requested", ev[0].Detail["step"])
	assert.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase)
}

func TestMove_ACancelAfterTheTargetsCreateDeletesItsCopy(t *testing.T) {
	r, m := placedOnA(t)
	r.move(orchapi.MoveRequest{})
	// The leader dies at the commit: the target holds its never-started copy.
	k := &killing{Store: r.store, n: 5}
	old := machines.NewReconciler(machines.ReconcilerConfig{Store: k, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet})
	require.ErrorIs(t, old.Reconcile(t.Context()), errKilled)
	require.Equal(t, machines.MoveCreated, r.record("web").Move.Step)
	require.Equal(t, "created", r.hosts.MicroVMs("h-b")["m-"+m.ID+"-2"].State)
	_, err := r.svc.CancelMove(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()

	assert.Empty(t, r.hosts.MicroVMs("h-b"), "the target's copy is deleted with its volumes")
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State)
	assert.Equal(t, machines.Binding{Host: "h-a", Epoch: 1}, *r.record("web").Binding)
}

func TestMove_ANewMoveTakesAnEpochAboveOneWhoseHostDidNotAnswer(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	r.move(orchapi.MoveRequest{})
	r.pass()
	require.Equal(t, uint64(2), r.record("web").Move.Target.Epoch)
	r.hosts.Fail("h-b")
	_, err := r.svc.CancelMove(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	require.Nil(t, r.record("web").Move, "aborted with its target away")

	r.hosts.Recover("h-b")
	r.hosts.RefuseImports("h-b", nil)
	r.move(orchapi.MoveRequest{})
	r.pass()
	rec := r.record("web")
	require.Nil(t, rec.Move)
	assert.Equal(t, machines.Binding{Host: "h-b", Epoch: 3}, *rec.Binding, "no epoch the first move took is taken again")
	assert.Equal(t, "running", r.hosts.MicroVMs("h-b")["m-"+m.ID+"-3"].State)
}

func TestMove_RefusesAMoveTheServiceCannotRecord(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 32*gib))
	s := spec()
	s.Shape.Region = "kr-seoul"
	r.create("web", s)
	r.pass()
	_, err := r.svc.Move(t.Context(), "operator", "garam", "nope", orchapi.MoveRequest{})
	assert.ErrorIs(t, err, machines.ErrNotFound)
	_, err = r.svc.Move(t.Context(), "operator", "garam", "web", orchapi.MoveRequest{Shape: &orchapi.MoveShape{Region: "us-east"}})
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "a region the Machine's shape does not allow")
	_, err = r.svc.CancelMove(t.Context(), "garam", "web")
	assert.ErrorIs(t, err, machines.ErrNoMove)

	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	first := r.move(orchapi.MoveRequest{})
	again := r.move(orchapi.MoveRequest{})
	assert.Equal(t, first.Move, again.Move, "a repeat answers the move in progress")
	_, err = r.svc.Move(t.Context(), "operator", "garam", "web", orchapi.MoveRequest{Shape: &orchapi.MoveShape{Labels: map[string]string{"gpu": "yes"}}})
	assert.ErrorIs(t, err, machines.ErrMoving, "another move while one is in progress")

	unplaced := newRig(t)
	unplaced.create("web", spec())
	_, err = unplaced.svc.Move(t.Context(), "operator", "garam", "web", orchapi.MoveRequest{})
	assert.ErrorIs(t, err, machines.ErrNotPlaced)
}

func TestMove_RefusesACancelPastTheCommit(t *testing.T) {
	r, _ := placedOnA(t)
	r.move(orchapi.MoveRequest{})
	routes := make(chan struct{})
	r.rec = machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet,
		Routes: func(context.Context, string, machines.Binding) (bool, error) {
			select {
			case <-routes:
				return true, nil
			default:
				return false, nil
			}
		},
	})
	r.pass()
	rec := r.record("web")
	require.NotNil(t, rec.Move)
	assert.Equal(t, machines.MoveStarted, rec.Move.Step, "the move waits for the routes")
	assert.Equal(t, "a proxy does not yet route the Machine to h-b", rec.Move.Reason)
	assert.Len(t, r.hosts.MicroVMs("h-a"), 1, "the source's copy stays until the routes are delivered")

	r.pass()
	assert.Len(t, r.hosts.MicroVMs("h-a"), 1, "a pass more leaves it")
	assert.Empty(t, r.fleet.of(machines.EventOlderEpoch), "the source's copy is the move's, not an older epoch")
	assert.Empty(t, r.fleet.of(machines.EventAllocationDrift), "the source's capacity is held until the clean up")
	_, err := r.svc.CancelMove(t.Context(), "garam", "web")
	assert.ErrorIs(t, err, machines.ErrMoving)
	close(routes)
	r.pass()
	assert.Nil(t, r.record("web").Move)
	assert.Empty(t, r.hosts.MicroVMs("h-a"))
}

func TestMove_NarrowsTheTargetByTheShapeAsked(t *testing.T) {
	c := host("h-c", 16*gib)
	c.Labels = map[string]string{"disk": "nvme"}
	r := newRig(t, host("h-a", 64*gib), host("h-b", 32*gib), c)
	r.create("web", spec())
	r.pass()
	r.move(orchapi.MoveRequest{Shape: &orchapi.MoveShape{Labels: map[string]string{"disk": "nvme"}}})
	r.pass()
	assert.Equal(t, "h-c", r.record("web").Binding.Host, "the only host with the label, although h-b has more memory")
}

func TestDrain_MovesOneMachineAtATimeFromAHost(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 16*gib))
	r.create("one", spec())
	r.create("two", spec())
	r.pass()
	require.Equal(t, "h-a", r.record("one").Binding.Host)
	require.Equal(t, "h-a", r.record("two").Binding.Host)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	r.hosts.Drain("h-a", true)
	r.pass()
	r.pass()
	require.NotNil(t, r.record("one").Move, "the first by name moves")
	assert.Equal(t, orchapi.ByDrain, r.record("one").Move.By)
	assert.Nil(t, r.record("two").Move, "the next waits for it, although h-b has room for both")

	r.hosts.RefuseImports("h-b", nil)
	r.pass()
	assert.Equal(t, "h-b", r.record("one").Binding.Host)
	r.pass()
	assert.Equal(t, "h-b", r.record("two").Binding.Host, "then the next")
	assert.Empty(t, r.fleet.of(machines.EventDrainHeld))
}

func TestDrain_HoldsAtAMachineThatFitsOnNoOtherHost(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 4*gib))
	r.create("one", spec())
	r.pass()
	r.hosts.Drain("h-a", true)
	r.pass()
	assert.Nil(t, r.record("one").Move, "it stays")
	assert.Equal(t, "h-a", r.record("one").Binding.Host)
	held := r.fleet.of(machines.EventDrainHeld)
	require.Len(t, held, 1)
	assert.Equal(t, "h-a", held[0].Detail["host"])
	assert.Equal(t, "no host has 4.0 GiB of memory free; the most free is 2.0 GiB", held[0].Detail["reason"])
	r.pass()
	assert.Len(t, r.fleet.of(machines.EventDrainHeld), 1, "one event while the reason is the same")
}

func TestDrain_HoldsAtMostTwoMovesInTheFleet(t *testing.T) {
	// Every Machine is on its own host; a fourth, h-d, too small to be placed on first, is where
	// they go.
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib), host("h-c", 64*gib), host("h-d", 20*gib))
	for _, n := range []string{"one", "two", "three"} {
		r.create(n, spec())
		r.pass()
	}
	hosts := map[string]bool{}
	for _, n := range []string{"one", "two", "three"} {
		hosts[r.record(n).Binding.Host] = true
	}
	require.Len(t, hosts, 3, "one Machine per host")
	r.hosts.RefuseImports("h-d", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	for _, h := range []string{"h-a", "h-b", "h-c"} {
		r.hosts.Drain(h, true)
	}
	r.pass()
	r.pass()
	moving := 0
	for _, n := range []string{"one", "two", "three"} {
		if r.record(n).Move != nil {
			moving++
		}
	}
	assert.Equal(t, 2, moving, "two moves in the fleet, and the third host's waits")
}

func TestMove_AbortsWhenTheSourceRunsAgainAfterItsStop(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.RefuseImports("h-b", &hostapi.Error{Status: http.StatusServiceUnavailable, Code: hostapi.CodeBusy, Message: "busy"})
	r.move(orchapi.MoveRequest{})
	r.pass()
	require.Equal(t, machines.MoveStopped, r.record("web").Move.Step)
	r.pass()
	require.NotNil(t, r.record("web").Move, "control: a stopped source keeps the move waiting at its copy")
	// The source's host reboots, and restartOnBoot starts the Machine there.
	r.hosts.Exit("h-a", "m-"+m.ID+"-1", "running")
	r.hosts.RefuseImports("h-b", nil)
	r.hosts.Stale("h-a", true)
	r.pass()
	require.NotNil(t, r.record("web").Move, "a repeated answer is no evidence the source runs")
	r.hosts.Stale("h-a", false)
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machines.Binding{Host: "h-a", Epoch: 1}, *rec.Binding, "the Machine stays where it runs")
	assert.Empty(t, r.hosts.MicroVMs("h-b"))
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"))
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, machines.ReasonSourceRanAgain, ev[0].Detail["reason"])
}
