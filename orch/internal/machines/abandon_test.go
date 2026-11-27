package machines_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// lostOnA is placedOnA with h-a abandoned and a pass that marks web lost.
func lostOnA(t *testing.T) (*rig, machineapi.Machine) {
	t.Helper()
	r, m := placedOnA(t)
	r.hosts.Abandon("h-a")
	r.pass()
	require.Equal(t, machineapi.PhaseLost, r.record("web").Status.Phase)
	return r, m
}

func TestLost_AMachineOnAnAbandonedHostIsLostWithItsVolumesListed(t *testing.T) {
	r, m := placedOnA(t)
	r.hosts.Abandon("h-a")
	r.pass()

	rec := r.record("web")
	assert.Equal(t, machineapi.PhaseLost, rec.Status.Phase)
	assert.Equal(t, machines.ReasonHostLost, rec.Status.Reason)
	require.Len(t, rec.Abandoned, 1)
	assert.Equal(t, "h-a", rec.Abandoned[0].Host)
	assert.Equal(t, uint64(1), rec.Abandoned[0].Epoch)
	assert.Equal(t, []string{"data"}, rec.Abandoned[0].Volumes)
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "nothing on the abandoned host is touched")
	got, err := r.svc.AbandonedVolumes(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []orchapi.AbandonedVolumes{{Caller: "garam", Name: "web", ID: m.ID, Host: "h-a", Epoch: 1, Volumes: []string{"data"}, At: rec.Abandoned[0].At}}, got)

	r.clock.Advance(1)
	r.pass()
	assert.Equal(t, rec.Abandoned, r.record("web").Abandoned, "a pass again records nothing more")
	phases, _ := r.phases(m.ID)
	assert.Equal(t, machineapi.PhaseLost, phases[len(phases)-1], "the caller's log shows it lost")
}

func TestLost_ANewLeaderMarksTheMachinesOfAHostAbandonedWhileTheOldOneDied(t *testing.T) {
	r, _ := placedOnA(t)
	r.hosts.Abandon("h-a")
	old := machines.NewReconciler(machines.ReconcilerConfig{Store: &killing{Store: r.store, n: 0}, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet})
	require.ErrorIs(t, old.Reconcile(t.Context()), errKilled)
	require.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase, "control: the old leader wrote nothing")

	r.rec = machines.NewReconciler(machines.ReconcilerConfig{Store: r.store, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet})
	r.pass()
	assert.Equal(t, machineapi.PhaseLost, r.record("web").Status.Phase)
}

func TestLost_NothingIsPlacedOrMovedOntoAnAbandonedHostThatAnswers(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.hosts.Abandon("h-a")
	r.create("web", spec())
	r.pass()
	assert.Nil(t, r.record("web").Binding, "the record alone makes the host ineligible")
	assert.Empty(t, r.hosts.Creates("h-a"))

	r2, _ := placedOnA(t)
	r2.hosts.Abandon("h-b")
	r2.move(orchapi.MoveRequest{})
	r2.pass()
	mv := r2.record("web").Move
	require.NotNil(t, mv)
	assert.Equal(t, machines.MoveRequested, mv.Step, "no target is reserved on it")
	assert.Equal(t, machineapi.PhaseRunning, r2.record("web").Status.Phase, "control: the source still runs")
}

func TestLost_AStartOrStopOfALostMachineKeepsItLost(t *testing.T) {
	r, _ := lostOnA(t)
	_, err := r.svc.Stop(t.Context(), "garam", "web", machineapi.StopRequest{})
	require.NoError(t, err)
	assert.Equal(t, machineapi.PhaseLost, r.record("web").Status.Phase)
	r.pass()
	assert.Equal(t, machineapi.PhaseLost, r.record("web").Status.Phase)
}

func TestLost_ADeleteOfALostMachineRemovesItWithoutItsHost(t *testing.T) {
	r, m := lostOnA(t)
	_, err := r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	_, _, err = r.store.Machine(t.Context(), "garam", "web")
	assert.ErrorIs(t, err, machines.ErrNotFound)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-a"))
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "the abandoned host is asked nothing")
}

func TestLost_AMoveWithAbandonVolumesStartsTheMachineElsewhereWithNewVolumes(t *testing.T) {
	r, m := lostOnA(t)
	_, err := r.svc.Move(t.Context(), "operator", "garam", "web", orchapi.MoveRequest{})
	assert.ErrorIs(t, err, machines.ErrLost, "its volumes cannot move with it")
	got := r.moveWith(orchapi.MoveRequest{AbandonVolumes: true})
	assert.True(t, got.Move.AbandonVolumes)
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machines.Binding{Host: "h-b", Epoch: 2}, *rec.Binding)
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase)
	assert.Equal(t, "running", r.hosts.MicroVMs("h-b")["m-"+m.ID+"-2"].State)
	_, err = r.svc.ReadFile(t.Context(), "garam", "web", "/data/state")
	assert.Error(t, err, "the volume is new")
	creates := r.hosts.Creates("h-b")
	require.Len(t, creates, 1)
	assert.False(t, creates[0].Disks[0].Imported)
	exports, _ := r.hosts.Transfers("h-a")
	assert.Zero(t, exports)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-a"))
	require.Len(t, rec.Abandoned, 1, "the old volumes stay listed")
	ev := r.fleet.of(machines.EventVolumesAbandoned)
	require.Len(t, ev, 1)
	assert.Equal(t, map[string]string{"id": m.ID, "host": "h-a", "epoch": "1", "volumes": "data"}, ev[0].Detail)

	_, err = r.svc.Move(t.Context(), "operator", "garam", "web", orchapi.MoveRequest{AbandonVolumes: true})
	assert.ErrorIs(t, err, machines.ErrNotLost, "a Machine that is not lost keeps its volumes")
}

func TestLost_AnAbortedMoveWithAbandonVolumesLeavesTheMachineLost(t *testing.T) {
	r, _ := lostOnA(t)
	r.hosts.Refuse("h-b", &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "image: not found"})
	r.moveWith(orchapi.MoveRequest{AbandonVolumes: true})
	r.pass()
	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machineapi.PhaseLost, rec.Status.Phase)
	assert.Equal(t, "h-a", rec.Binding.Host)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"))
}

func TestLost_AMoveWhoseSourceIsAbandonedBeforeTheCommitLeavesTheMachineLost(t *testing.T) {
	r, m := placedOnA(t)
	busyTarget(r, "h-b")
	r.move(orchapi.MoveRequest{})
	r.pass()
	r.hosts.Abandon("h-a")
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machineapi.PhaseLost, rec.Status.Phase)
	assert.Equal(t, machines.Binding{Host: "h-a", Epoch: 1}, *rec.Binding)
	require.Len(t, rec.Abandoned, 1)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"), "the target's reservation is released")
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "the abandoned source is not started")
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, "the source h-a was abandoned", ev[0].Detail["reason"])
}

func TestLost_AMoveWhoseTargetIsAbandonedBeforeTheCommitIsAborted(t *testing.T) {
	r, m := placedOnA(t)
	busyTarget(r, "h-b")
	r.move(orchapi.MoveRequest{})
	r.pass()
	r.hosts.Abandon("h-b")
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase)
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "the source runs again")
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-b"))
	ev := r.fleet.of(machines.EventMoveAborted)
	require.Len(t, ev, 1)
	assert.Equal(t, "the target h-b was abandoned", ev[0].Detail["reason"])
}

func TestLost_AMoveWhoseTargetIsAbandonedAfterTheCommitLeavesTheMachineLostThere(t *testing.T) {
	r, m := placedOnA(t)
	r.move(orchapi.MoveRequest{})
	r.rec = machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet,
		Routes: func(context.Context, string, machines.Binding) (bool, error) { return false, nil },
	})
	r.pass()
	require.Equal(t, machines.MoveStarted, r.record("web").Move.Step)
	r.hosts.Abandon("h-b")
	r.pass()

	rec := r.record("web")
	assert.Nil(t, rec.Move)
	assert.Equal(t, machineapi.PhaseLost, rec.Status.Phase)
	assert.Equal(t, machines.Binding{Host: "h-b", Epoch: 2}, *rec.Binding)
	require.Len(t, rec.Abandoned, 1)
	assert.Equal(t, "h-b", rec.Abandoned[0].Host)
	assert.Equal(t, hostapi.Resources{}, r.allocation("h-a"), "the source's capacity is freed")
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State, "its copy is left as an older epoch")
	assert.Len(t, r.fleet.of(machines.EventMoveLost), 1)
}

func TestLost_AnAbandonedEpochFoundOnAHostThatAnswersAgainIsStoppedReportedAndKept(t *testing.T) {
	r, m := lostOnA(t)
	r.moveWith(orchapi.MoveRequest{AbandonVolumes: true})
	r.pass()
	require.Equal(t, "h-b", r.record("web").Binding.Host)
	r.hosts.Readmit("h-a")
	r.pass()
	r.pass()

	old := r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"]
	assert.Equal(t, "stopped", old.State, "it does not run beside the Machine's new epoch")
	ev := r.fleet.of(machines.EventAbandonedVolumesFound)
	require.Len(t, ev, 1, "once, not each pass")
	assert.Equal(t, map[string]string{"host": "h-a", "microvm": "m-" + m.ID + "-1", "id": m.ID, "epoch": "1", "volumes": "data"}, ev[0].Detail)
	assert.Len(t, r.record("web").Abandoned, 1, "still listed until an operator deletes them")
}

func TestLost_AnOperatorDeletesAbandonedVolumes(t *testing.T) {
	r, _ := lostOnA(t)
	require.ErrorIs(t, r.svc.DeleteAbandonedVolumes(t.Context(), "garam", "web", "h-a", 2), machines.ErrNotFound)
	require.NoError(t, r.svc.DeleteAbandonedVolumes(t.Context(), "garam", "web", "h-a", 1))
	got, err := r.svc.AbandonedVolumes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
	r.pass()
	assert.Empty(t, r.record("web").Abandoned, "a pass over the lost Machine does not list them again")
}

func (r *rig) moveWith(req orchapi.MoveRequest) orchapi.Machine {
	r.t.Helper()
	m, err := r.svc.Move(r.t.Context(), "operator", "garam", "web", req)
	require.NoError(r.t, err)
	return m
}
