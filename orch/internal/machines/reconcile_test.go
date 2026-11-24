package machines_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/machines"
	machinesetcd "github.com/garamsh/naru/orch/internal/machines/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const gib = 1 << 30

// fleetLog keeps the fleet's events the reconciler appends.
type fleetLog struct {
	mu     sync.Mutex
	events []orchapi.Event
}

func (f *fleetLog) AppendEvent(_ context.Context, ev orchapi.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
	return nil
}

func (f *fleetLog) of(typ string) []orchapi.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []orchapi.Event
	for _, e := range f.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// rig is the reconciler over one etcd member, the fake hosts and a clock the test moves, with the
// Machine API's service writing the records as a caller would.
type rig struct {
	t     *testing.T
	svc   machines.Service
	store *machinesetcd.Store
	kv    *clientv3.Client
	hosts *orchtest.FakeMachineHosts
	clock *orchtest.Clock
	fleet *fleetLog
	rec   *machines.Reconciler
}

// host is a host in kr-seoul with 16 CPUs, the memory given and a TiB of disk.
func host(id string, memory uint64) machines.Host {
	return machines.Host{ID: id, Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 16, Memory: memory, Disk: 1 << 40}}
}

func newRig(t *testing.T, hosts ...machines.Host) *rig {
	t.Helper()
	kv := orchtest.Start(t, orchtest.Options{Members: 1}).Members[0].Etcd()
	r := &rig{t: t, store: machinesetcd.New(kv), kv: kv, hosts: orchtest.NewFakeMachineHosts(hosts...), clock: orchtest.NewClock(), fleet: &fleetLog{}}
	r.svc = machines.NewService(r.store, r.clock, "m.example.test")
	r.rec = machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: r.hosts, Clock: r.clock, Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib}, Fleet: r.fleet,
	})
	return r
}

// spec takes 2 CPUs, 4 GiB of memory and 30 GiB of disk.
func spec() machineapi.Spec {
	return machineapi.Spec{
		Image: "registry.example.test/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CPU:   2, MemoryBytes: 4 * gib, RootDiskBytes: 10 * gib,
		Volumes:    []machineapi.Volume{{Name: "data", SizeBytes: 20 * gib, MountPath: "/data"}},
		Command:    []string{"/app"},
		Args:       []string{"--serve"},
		Env:        map[string]string{"TOKEN": "s3cret"},
		User:       "app",
		WorkingDir: "/srv",
		Files:      []machineapi.File{{Path: "/etc/app.conf", Mode: 0o600, Content: []byte("key=value")}},
		NetworkPolicy: machineapi.NetworkPolicy{Egress: []machineapi.Rule{
			{Prefix: "0.0.0.0/0", Protocol: "tcp", Ports: []machineapi.PortRange{{First: 443, Last: 443}}},
		}},
	}
}

func (r *rig) create(name string, s machineapi.Spec) machineapi.Machine {
	r.t.Helper()
	m, _, err := r.svc.Create(r.t.Context(), "garam", name, s)
	require.NoError(r.t, err)
	return m
}

func (r *rig) pass() {
	r.t.Helper()
	require.NoError(r.t, r.rec.Reconcile(r.t.Context()))
}

func (r *rig) record(name string) machines.Record {
	r.t.Helper()
	rec, _, err := r.store.Machine(r.t.Context(), "garam", name)
	require.NoError(r.t, err)
	return rec
}

func (r *rig) allocation(host string) hostapi.Resources {
	r.t.Helper()
	all, err := r.store.Allocations(r.t.Context())
	require.NoError(r.t, err)
	return all[host].Resources
}

// phases are the phases of the caller's log for the Machine id, in order, and whether it ends deleted.
func (r *rig) phases(id string) ([]machineapi.Phase, bool) {
	r.t.Helper()
	page, err := r.svc.Events(r.t.Context(), "garam", "")
	require.NoError(r.t, err)
	var out []machineapi.Phase
	deleted := false
	for _, e := range page.Events {
		if e.ID != id {
			continue
		}
		if e.Type == machineapi.EventDeleted {
			deleted = true
			continue
		}
		if len(out) == 0 || out[len(out)-1] != e.Status.Phase {
			out = append(out, e.Status.Phase)
		}
	}
	return out, deleted
}

func TestReconcile_RunsAMachineOnAHostAsItsRecordAsks(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	r.pass()

	rec := r.record("web")
	require.NotNil(t, rec.Binding, "the Machine is placed")
	assert.Equal(t, machines.Binding{Host: "h-a", Epoch: 1}, *rec.Binding)
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase)
	assert.Equal(t, rec.Status.Generation, rec.Status.ObservedGeneration)
	creates := r.hosts.Creates("h-a")
	require.Len(t, creates, 1)
	c := creates[0]
	assert.Equal(t, "m-"+m.ID+"-1", c.Name, "the microVM names the id and the epoch, and no caller")
	assert.False(t, c.Fenced, "a Machine is not fenced per microVM")
	require.NotNil(t, c.RestartOnBoot)
	assert.True(t, *c.RestartOnBoot, "it comes back after its host reboots")
	assert.Equal(t, spec().Image, c.Image)
	assert.Equal(t, uint32(2), c.CPU)
	assert.Equal(t, uint64(4*gib), c.Memory)
	assert.Equal(t, uint64(10*gib), c.RootDiskBytes)
	assert.Equal(t, []hostapi.DiskSpec{{Name: "data", SizeBytes: 20 * gib, MountPath: "/data"}}, c.Disks, "each volume a disk at its path")
	assert.Equal(t, "s3cret", c.Env["TOKEN"], "the host is given the value the caller set")
	assert.Equal(t, []string{"/app"}, c.Command)
	assert.Equal(t, []string{"--serve"}, c.Args)
	assert.Equal(t, "app", c.User)
	assert.Equal(t, "/srv", c.WorkingDir)
	assert.Equal(t, []hostapi.File{{Path: "/etc/app.conf", Mode: 0o600, Content: []byte("key=value")}}, c.Files)
	assert.Equal(t, hostapi.Policy{Egress: []hostapi.Rule{{Prefix: "0.0.0.0/0", Protocol: "tcp", Ports: []hostapi.PortRange{{First: 443, Last: 443}}}}}, c.NetworkPolicy)
	assert.Equal(t, hostapi.Resources{CPU: 2, Memory: 4 * gib, Disk: 30 * gib}, r.allocation("h-a"), "the host's allocation holds what the Machine takes")
	phases, _ := r.phases(m.ID)
	assert.Equal(t, []machineapi.Phase{machineapi.PhasePending, machineapi.PhaseStarting, machineapi.PhaseRunning}, phases)

	r.pass()
	assert.Len(t, r.hosts.Creates("h-a"), 1, "a pass over a running Machine changes nothing")
}

func TestReconcile_StopsAndStartsTheMicroVMToTheDesiredState(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	r.pass()
	vm := "m-" + m.ID + "-1"

	_, err := r.svc.Stop(t.Context(), "garam", "web", machineapi.StopRequest{GraceSeconds: 30})
	require.NoError(t, err)
	assert.Equal(t, machineapi.PhaseStopping, r.record("web").Status.Phase, "the stop shows at once")
	r.pass()
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-a")[vm].State)
	assert.Equal(t, machineapi.PhaseStopped, r.record("web").Status.Phase)
	assert.Equal(t, hostapi.Resources{CPU: 2, Memory: 4 * gib, Disk: 30 * gib}, r.allocation("h-a"), "a stopped Machine keeps its place")

	_, err = r.svc.Start(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")[vm].State, "a start the reconciler has not acted on is at once")
	assert.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase)
	assert.Empty(t, r.fleet.of(machines.EventRestart), "a start asked for is no restart")
}

func TestReconcile_RestartsAMachineThatStoppedOnItsOwnAtMostOnceAMinute(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	r.pass()
	vm := "m-" + m.ID + "-1"

	r.hosts.Exit("h-a", vm, "faulted")
	r.pass()
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")[vm].State, "the first stop on its own is restarted at once")
	assert.Len(t, r.fleet.of(machines.EventRestart), 1, "each attempt is an event")

	r.hosts.Exit("h-a", vm, "faulted")
	r.clock.Advance(machines.RestartEvery - time.Second)
	r.pass()
	assert.Equal(t, "faulted", r.hosts.MicroVMs("h-a")[vm].State, "not again within the minute")
	rec := r.record("web")
	assert.Equal(t, machineapi.PhaseFailed, rec.Status.Phase)
	assert.Equal(t, machines.ReasonRestartWait, rec.Status.Reason)

	r.clock.Advance(time.Second)
	r.pass()
	assert.Equal(t, "running", r.hosts.MicroVMs("h-a")[vm].State, "again once the minute has passed")
	assert.Len(t, r.fleet.of(machines.EventRestart), 2)
	assert.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase)
}

func TestReconcile_KeepsAMachineWhoseHostDoesNotAnswerUnknownAndPlacesItNowhereElse(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib))
	m := r.create("web", spec())
	r.pass()
	bound := r.record("web").Binding.Host
	other := map[string]string{"h-a": "h-b", "h-b": "h-a"}[bound]

	r.hosts.Fail(bound)
	r.pass()
	rec := r.record("web")
	assert.Equal(t, machineapi.PhaseUnknown, rec.Status.Phase)
	assert.Equal(t, machines.ReasonNotAnswering, rec.Status.Reason)
	assert.Equal(t, machines.Binding{Host: bound, Epoch: 1}, *rec.Binding, "still bound where it was")
	assert.Empty(t, r.hosts.Creates(other), "no second copy is made elsewhere")

	_, err := r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	assert.Equal(t, machineapi.PhaseDeleting, r.record("web").Status.Phase, "a delete waits for the host, with the name held")

	r.hosts.Recover(bound)
	r.pass()
	_, _, err = r.store.Machine(t.Context(), "garam", "web")
	require.ErrorIs(t, err, machines.ErrNotFound)
	assert.NotContains(t, r.hosts.MicroVMs(bound), "m-"+m.ID+"-1")
}

func TestReconcile_DeletesTheMicroVMThenFreesTheNameAndTheAllocation(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	keep := r.create("api", spec())
	r.pass()
	_, err := r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)

	r.pass()
	_, _, err = r.store.Machine(t.Context(), "garam", "web")
	require.ErrorIs(t, err, machines.ErrNotFound, "the name is free")
	ids, err := r.kv.Get(t.Context(), machinesetcd.IDPrefix+m.ID)
	require.NoError(t, err)
	assert.Empty(t, ids.Kvs, "and the id")
	vms := r.hosts.MicroVMs("h-a")
	assert.NotContains(t, vms, "m-"+m.ID+"-1", "the microVM and its disks are gone")
	assert.Contains(t, vms, "m-"+keep.ID+"-1")
	assert.Equal(t, hostapi.Resources{CPU: 2, Memory: 4 * gib, Disk: 30 * gib}, r.allocation("h-a"), "the allocation gives back what it took")
	_, deleted := r.phases(m.ID)
	assert.True(t, deleted, "its end is an event")
}

func TestReconcile_RemovesAMachineDeletedBeforeItWasPlaced(t *testing.T) {
	r := newRig(t)
	m := r.create("web", spec())
	r.pass()
	assert.Equal(t, "no host answers", r.record("web").Status.Reason)
	_, err := r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	_, _, err = r.store.Machine(t.Context(), "garam", "web")
	require.ErrorIs(t, err, machines.ErrNotFound)
	_, deleted := r.phases(m.ID)
	assert.True(t, deleted)
}

func TestReconcile_PlacesOnlyWhatFitsAndSaysWhyTheRestWaits(t *testing.T) {
	// 10 GiB less the 2 GiB reserve fits two Machines of 4 GiB, not three.
	r := newRig(t, host("h-a", 10*gib))
	for _, n := range []string{"a", "b", "c"} {
		r.create(n, spec())
	}
	r.pass()
	var waiting []machines.Record
	for _, n := range []string{"a", "b", "c"} {
		if rec := r.record(n); rec.Binding == nil {
			waiting = append(waiting, rec)
		}
	}
	require.Len(t, waiting, 1, "two fit")
	w := waiting[0]
	assert.Equal(t, machineapi.PhasePending, w.Status.Phase)
	assert.Equal(t, "no host fits the memory", w.Status.Reason, "the caller reads the condition")
	assert.Equal(t, "no host has 4.0 GiB of memory free; the most free is 0 bytes", w.OperatorReason, "the operator also the figures")
	assert.Equal(t, hostapi.Resources{CPU: 4, Memory: 8 * gib, Disk: 60 * gib}, r.allocation("h-a"))
	all, err := r.svc.All(t.Context())
	require.NoError(t, err)
	for _, m := range all {
		if m.Machine.Name == w.Name {
			assert.Equal(t, w.OperatorReason, m.Reason, "the operator API answers the figures")
		}
	}

	_, err = r.svc.Delete(t.Context(), "garam", map[bool]string{true: "a", false: "b"}[w.Name != "a"])
	require.NoError(t, err)
	r.pass()
	r.pass()
	assert.NotNil(t, r.record(w.Name).Binding, "the freed room takes the waiting Machine")
}

func TestReconcile_CountsAMicroVMItDidNotPlaceAgainstTheHost(t *testing.T) {
	r := newRig(t, host("h-a", 10*gib))
	r.hosts.Put("h-a", hostapi.MicroVM{Name: "web-1", State: "running", CPU: 2, Memory: 5 * gib, RootDiskBytes: gib})
	r.create("web", spec())
	r.pass()
	assert.Nil(t, r.record("web").Binding, "8 GiB less a group member's 5 leaves too little")
	assert.Contains(t, r.hosts.MicroVMs("h-a"), "web-1", "and the member is not its to touch")
}

func TestReconcile_DeletesAMicroVMOfARemovedMachineAndLeavesOneItDoesNotKnow(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	old := r.create("old", spec())
	r.pass()
	_, err := r.svc.Delete(t.Context(), "garam", "old")
	require.NoError(t, err)
	r.pass()
	late := "m-" + old.ID + "-1"
	require.NotContains(t, r.hosts.MicroVMs("h-a"), late, "control: the delete deleted it")
	// A create that timed out before the removal lands after it.
	r.hosts.Put("h-a", hostapi.MicroVM{Name: late, State: "running"})
	unknown := "m-00000000000000ff-3"
	r.hosts.Put("h-a", hostapi.MicroVM{Name: unknown, State: "running"})

	r.pass()
	vms := r.hosts.MicroVMs("h-a")
	assert.NotContains(t, vms, late, "a microVM of a removed Machine, which its tombstone names, is deleted")
	require.Contains(t, vms, unknown, "one whose id orch neither has nor removed is left, disks and all")
	assert.Equal(t, "running", vms[unknown].State, "running")
	events := r.fleet.of(machines.EventUnknownMicroVM)
	require.Len(t, events, 1, "with an event")
	assert.Equal(t, "h-a/"+unknown, events[0].Subject)
	r.pass()
	assert.Len(t, r.fleet.of(machines.EventUnknownMicroVM), 1, "once")
}

func TestReconcile_DropsATombstoneOnceItIsAWeekOld(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	old := r.create("old", spec())
	r.pass()
	_, err := r.svc.Delete(t.Context(), "garam", "old")
	require.NoError(t, err)
	r.pass()
	tombs, err := r.store.Tombstones(t.Context())
	require.NoError(t, err)
	require.Contains(t, tombs, old.ID)

	r.clock.Advance(machines.TombstoneAge - time.Minute)
	r.pass()
	tombs, err = r.store.Tombstones(t.Context())
	require.NoError(t, err)
	assert.Contains(t, tombs, old.ID, "kept within the week")
	r.clock.Advance(time.Minute)
	r.pass()
	tombs, err = r.store.Tombstones(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, tombs, old.ID, "dropped after it")
}

func TestReconcile_StopsAnOlderEpochOfALiveMachineAndLeavesIt(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib))
	m := r.create("web", spec())
	r.pass()
	bound := r.record("web").Binding.Host
	other := map[string]string{"h-a": "h-b", "h-b": "h-a"}[bound]
	stale := "m-" + m.ID + "-1"
	r.hosts.Put(other, hostapi.MicroVM{Name: stale, State: "running", CPU: 2, Memory: 4 * gib})
	for _, name := range []string{"m-web", "m-00000000000000ff", "m-00000000000000ff-0", "web-1"} {
		r.hosts.Put(other, hostapi.MicroVM{Name: name, State: "running"})
	}

	r.pass()
	vms := r.hosts.MicroVMs(other)
	require.Contains(t, vms, stale, "an older epoch of a live Machine is left")
	assert.Equal(t, "stopped", vms[stale].State, "stopped")
	events := r.fleet.of(machines.EventOlderEpoch)
	require.Len(t, events, 1, "with an event")
	assert.Equal(t, "garam/web", events[0].Subject)
	assert.Equal(t, map[string]string{"host": other, "microvm": stale, "id": m.ID}, events[0].Detail)
	for _, name := range []string{"m-web", "m-00000000000000ff", "m-00000000000000ff-0", "web-1"} {
		assert.Equal(t, "running", vms[name].State, "%s is no name the reconciler gives", name)
	}
	assert.Empty(t, r.fleet.of(machines.EventUnknownMicroVM), "nor one it reports")
	assert.Equal(t, "running", r.hosts.MicroVMs(bound)[stale].State, "the bound one runs on")

	r.pass()
	assert.Len(t, r.fleet.of(machines.EventOlderEpoch), 1, "a stopped one is left alone after")
}

// After an etcd restore from before a Machine was placed, its record has no binding while its
// microVM runs: the new placement takes an epoch above it, and the old one is stopped and left.
func TestReconcile_BindsAboveEveryEpochAHostReportsForTheID(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib))
	m := r.create("web", spec())
	before := "m-" + m.ID + "-1"
	r.hosts.Put("h-b", hostapi.MicroVM{Name: before, State: "running", CPU: 2, Memory: 4 * gib})

	r.pass()
	b := r.record("web").Binding
	require.NotNil(t, b)
	assert.Equal(t, uint64(2), b.Epoch, "above the epoch h-b reports")
	assert.Contains(t, r.hosts.MicroVMs(b.Host), "m-"+m.ID+"-2", "under a name of its own")
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-b")[before].State, "the one before is stopped")
	assert.Len(t, r.fleet.of(machines.EventOlderEpoch), 1)
}

func TestReconcile_CreatesAGenerationTheHostRefusedOnceAndNotAgain(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.hosts.Refuse("h-a", &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "image: not found"})
	m := r.create("web", spec())
	r.pass()
	r.pass()
	r.pass()
	assert.Len(t, r.hosts.Creates("h-a"), 1, "one create for the generation")
	rec := r.record("web")
	assert.Equal(t, machineapi.PhaseFailed, rec.Status.Phase)
	assert.Equal(t, "the host refused the Machine: image: not found", rec.Status.Reason)
	phases, _ := r.phases(m.ID)
	assert.Equal(t, []machineapi.Phase{machineapi.PhasePending, machineapi.PhaseStarting, machineapi.PhaseFailed}, phases, "and one failure in the caller's log")

	r.hosts.Refuse("h-a", nil)
	_, err := r.svc.Stop(t.Context(), "garam", "web", machineapi.StopRequest{})
	require.NoError(t, err)
	r.pass()
	assert.Len(t, r.hosts.Creates("h-a"), 2, "a new generation is created")
	assert.Equal(t, machineapi.PhaseStopped, r.record("web").Status.Phase)
}

func TestReconcile_RetriesACreateTheHostDidNotTakeAsARefusal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError} {
		r := newRig(t, host("h-a", 64*gib))
		r.hosts.Refuse("h-a", &hostapi.Error{Status: status, Message: "not now"})
		r.create("web", spec())
		assert.Error(t, r.rec.Reconcile(t.Context()), "%d", status)
		assert.Error(t, r.rec.Reconcile(t.Context()), "%d", status)
		assert.Len(t, r.hosts.Creates("h-a"), 2, "%d: each pass creates again", status)
		assert.NotEqual(t, machineapi.PhaseFailed, r.record("web").Status.Phase, "%d: the Machine has not failed", status)
	}
}

// countingHosts counts the passes, each of which lists the hosts once.
type countingHosts struct {
	machines.Hosts
	mu     sync.Mutex
	passes int
}

func (c *countingHosts) List(ctx context.Context) ([]machines.Host, error) {
	c.mu.Lock()
	c.passes++
	c.mu.Unlock()
	return c.Hosts.List(ctx)
}

func (c *countingHosts) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.passes
}

// runOnWatch runs the reconciler on the etcd watch alone for two seconds after a create, and returns
// how many passes it ran.
func runOnWatch(t *testing.T, r *rig) int {
	t.Helper()
	hosts := &countingHosts{Hosts: r.hosts}
	rec := machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: hosts, Clock: r.clock, Interval: time.Hour, Fleet: r.fleet,
		Wake: func(ctx context.Context) <-chan struct{} {
			return machinesetcd.Watch(ctx, r.kv, slog.New(slog.DiscardHandler))
		},
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rec.Run(ctx) }()
	require.Eventually(t, func() bool { return hosts.count() == 1 }, 5*time.Second, 10*time.Millisecond, "the first pass")
	r.create("web", spec())
	time.Sleep(2 * time.Second)
	cancel()
	<-done
	return hosts.count()
}

// The reconciler's own writes (a binding, starting, running) wake no pass: the create wakes one.
func TestReconcile_RunsNoPassOnItsOwnWrites(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	assert.Equal(t, 2, runOnWatch(t, r), "the first pass and the one the create woke")
	assert.Equal(t, machineapi.PhaseRunning, r.record("web").Status.Phase, "control: the pass ran the Machine")
}

// The reproduction of the refused create's loop.
func TestReconcile_MakesOneCreateOfARefusedGenerationOnTheWatch(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.hosts.Refuse("h-a", &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "image: not found"})
	runOnWatch(t, r)
	assert.Len(t, r.hosts.Creates("h-a"), 1, "one create in two seconds")
}

func TestReconcile_GivesAHostThatHangsNoMoreWorkAndTheOthersTheirs(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib))
	r.rec = machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: r.hosts, Clock: r.clock, Interval: 300 * time.Millisecond, Fleet: r.fleet,
		Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib},
	})
	release := r.hosts.Hang("h-a")
	defer release()
	// A pass that waited on the hung host would end here, not before.
	time.AfterFunc(4*time.Second, release)
	// Two Machines whose rendezvous ties put one on each host.
	var on map[string]string
	for i := 0; on == nil || len(on) < 2; i++ {
		name := fmt.Sprintf("web-%d", i)
		r.create(name, spec())
		require.NoError(t, r.rec.Reconcile(t.Context()))
		on = map[string]string{}
		for _, n := range []string{"web-0", "web-1", "web-2", "web-3", "web-4", "web-5"} {
			if rec, _, err := r.store.Machine(t.Context(), "garam", n); err == nil && rec.Binding != nil {
				on[rec.Binding.Host] = n
			}
		}
		require.Less(t, i, 6, "the spread put no Machine on both hosts")
	}
	start := time.Now()
	require.NoError(t, r.rec.Reconcile(t.Context()))
	assert.Less(t, time.Since(start), 2*time.Second, "the pass does not wait on the hung host")
	assert.Equal(t, machineapi.PhaseRunning, r.record(on["h-b"]).Status.Phase, "the other host's Machine runs")
	assert.Len(t, r.hosts.Creates("h-a"), 0, "the hung host has made nothing yet")

	release()
	require.Eventually(t, func() bool { return len(r.hosts.Creates("h-a")) == 1 }, 5*time.Second, 50*time.Millisecond, "its work goes on once it answers")
	require.Eventually(t, func() bool {
		require.NoError(t, r.rec.Reconcile(t.Context()))
		return r.record(on["h-a"]).Status.Phase == machineapi.PhaseRunning
	}, 5*time.Second, 100*time.Millisecond)
	assert.Len(t, r.hosts.Creates("h-a"), 1, "and no pass gave it the same work while it hung")
}

func TestReconcile_GivesNoWorkToAHostWhoseWorkEndedWhileThePassRead(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.rec = machines.NewReconciler(machines.ReconcilerConfig{
		Store: r.store, Hosts: r.hosts, Clock: r.clock, Interval: 300 * time.Millisecond, Fleet: r.fleet,
		Reserve: machines.Reserve{CPU: 1, Memory: 2 * gib},
	})
	release := r.hosts.Hang("h-a")
	defer release()
	r.create("web", spec())
	require.NoError(t, r.rec.Reconcile(t.Context()))
	// The hung work makes the microVM after the next pass took the host's answer, which lacks it,
	// and ends before that pass gives out work.
	r.hosts.AfterList = func() {
		r.hosts.AfterList = nil
		release()
		require.Eventually(t, func() bool { return r.record("web").Status.Phase == machineapi.PhaseRunning }, 5*time.Second, 10*time.Millisecond)
		time.Sleep(200 * time.Millisecond)
	}
	require.NoError(t, r.rec.Reconcile(t.Context()))
	assert.Len(t, r.hosts.Creates("h-a"), 1, "a pass gives no work from an answer older than the host's last work")
	require.NoError(t, r.rec.Reconcile(t.Context()))
	assert.Len(t, r.hosts.Creates("h-a"), 1, "and the next pass finds the microVM")
}

func TestReconcile_MakesNoCreateFromAStaleAnswer(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.hosts.Stale("h-a", true)
	r.create("web", spec())
	r.pass()
	assert.Empty(t, r.hosts.Creates("h-a"), "a microVM made since the answer would be missing from it")
	assert.NotNil(t, r.record("web").Binding, "control: the Machine is placed")
	r.hosts.Stale("h-a", false)
	r.pass()
	assert.Len(t, r.hosts.Creates("h-a"), 1)
}

func TestReconcile_ShowsAMachineCreatedStoppedAsStoppingUntilItIsStopped(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	s := spec()
	s.State = machineapi.StateStopped
	m := r.create("web", s)
	r.pass()
	phases, _ := r.phases(m.ID)
	assert.Equal(t, []machineapi.Phase{machineapi.PhasePending, machineapi.PhaseStopping, machineapi.PhaseStopped}, phases)
	assert.Equal(t, "stopped", r.hosts.MicroVMs("h-a")["m-"+m.ID+"-1"].State)
}

func TestReconcile_StopsWithAGraceAtMostTheHostsBound(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("web", spec())
	r.pass()
	// A grace the API took before it was bounded.
	rec, rev, err := r.store.Machine(t.Context(), "garam", "web")
	require.NoError(t, err)
	rec.Spec.State, rec.StopGraceSeconds = machineapi.StateStopped, 500
	rec.Status.Generation++
	_, err = r.store.Update(t.Context(), rec, rev, nil)
	require.NoError(t, err)
	r.pass()
	assert.Equal(t, []uint32{hostapi.MaxGraceSeconds}, r.hosts.Graces("h-a"))
	assert.Equal(t, machineapi.PhaseStopped, r.record("web").Status.Phase)
}

func TestReconcile_RecordsAHostWhoseAllocationIsNotTheSumOfItsBindings(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("web", spec())
	r.pass()
	assert.Empty(t, r.fleet.of(machines.EventAllocationDrift), "control: none while they agree")
	_, err := r.kv.Put(t.Context(), machinesetcd.AllocationPrefix+"h-a", `{"cpu":1,"memory":1,"disk":1}`)
	require.NoError(t, err)
	r.pass()
	r.pass()
	events := r.fleet.of(machines.EventAllocationDrift)
	require.Len(t, events, 1, "one per change")
	assert.Equal(t, "h-a", events[0].Subject)
}

func TestReconcile_TakesASnapshotOnTheMachinesHostAndDeletesItThereAfter(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	m := r.create("web", spec())
	r.pass()
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "web"})
	require.NoError(t, err)

	r.pass()
	snap, _, err := r.store.Snapshot(t.Context(), "garam", "before")
	require.NoError(t, err)
	assert.Equal(t, machineapi.SnapshotReady, snap.Snapshot.Phase)
	assert.Equal(t, "h-a", snap.Host, "it records its host")
	assert.Equal(t, []string{"s-" + snap.ID + "-0"}, r.hosts.DiskSnapshots("h-a"), "one disk snapshot per volume")
	assert.Equal(t, m.ID, snap.Snapshot.MachineID)

	require.NoError(t, r.svc.DeleteSnapshot(t.Context(), "garam", "before"))
	dels, err := r.store.Deletions(t.Context())
	require.NoError(t, err)
	require.Len(t, dels, 1, "the deletion outlives the record")
	r.pass()
	assert.Empty(t, r.hosts.DiskSnapshots("h-a"), "the disk snapshots go from the host")
	dels, err = r.store.Deletions(t.Context())
	require.NoError(t, err)
	require.Len(t, dels, 1)
	assert.False(t, dels[0].Done.IsZero(), "and the deletion is done, kept as the tombstone")

	// One a leader that overlapped took after the deletion is deleted by the tombstone; one no
	// snapshot or deletion names is left.
	leaked, unknown := "s-"+snap.ID+"-0", "s-00000000000000ff-0"
	r.hosts.PutDiskSnapshot("h-a", hostapi.DiskSnapshot{Name: leaked})
	r.hosts.PutDiskSnapshot("h-a", hostapi.DiskSnapshot{Name: unknown})
	r.pass()
	assert.Equal(t, []string{unknown}, r.hosts.DiskSnapshots("h-a"))
	events := r.fleet.of(machines.EventUnknownDiskSnapshot)
	require.Len(t, events, 1)
	assert.Equal(t, "h-a/"+unknown, events[0].Subject)

	r.clock.Advance(machines.TombstoneAge)
	r.pass()
	dels, err = r.store.Deletions(t.Context())
	require.NoError(t, err)
	assert.Empty(t, dels, "a week after it is done the deletion goes")
}

func TestReconcile_FailsASnapshotTheHostRefusesThreeTimesAndDeletesWhatItTook(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("web", spec())
	r.pass()
	r.hosts.RefuseDisks("h-a", &hostapi.Error{Status: http.StatusUnprocessableEntity, Message: "no room"})
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "web"})
	require.NoError(t, err)
	for i := 1; i < machines.MaxSnapshotRefusals; i++ {
		r.pass()
		snap, _, err := r.store.Snapshot(t.Context(), "garam", "before")
		require.NoError(t, err)
		assert.Equal(t, machineapi.SnapshotPending, snap.Snapshot.Phase, "after %d refusals", i)
	}
	r.pass()
	snap, _, err := r.store.Snapshot(t.Context(), "garam", "before")
	require.NoError(t, err)
	assert.Equal(t, machineapi.SnapshotFailed, snap.Snapshot.Phase)
	assert.Equal(t, "the host refused the snapshot: no room", snap.Snapshot.Reason)
	dels, err := r.store.Deletions(t.Context())
	require.NoError(t, err)
	require.Len(t, dels, 1, "what it took goes")
	assert.Equal(t, []string{"s-" + snap.ID + "-0"}, dels[0].Disks)
}

func TestReconcile_FailsASnapshotOfAMachineBeingDeleted(t *testing.T) {
	r := newRig(t)
	r.create("web", spec())
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "web"})
	require.NoError(t, err)
	_, err = r.svc.Delete(t.Context(), "garam", "web")
	require.NoError(t, err)
	r.pass()
	snap, _, err := r.store.Snapshot(t.Context(), "garam", "before")
	require.NoError(t, err)
	assert.Equal(t, machineapi.SnapshotFailed, snap.Snapshot.Phase)
	assert.Equal(t, machines.ReasonMachineDeleted, snap.Snapshot.Reason)
}

func TestReconcile_PlacesARestoreOnTheSnapshotsHostWithItsDisks(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib), host("h-b", 64*gib))
	r.create("src", spec())
	r.pass()
	at := r.record("src").Binding.Host
	other := map[string]string{"h-a": "h-b", "h-b": "h-a"}[at]
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "src"})
	require.NoError(t, err)
	restore := spec()
	restore.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "before"}
	r.create("restored", restore)
	snap, _, err := r.store.Snapshot(t.Context(), "garam", "before")
	require.NoError(t, err)

	r.pass()
	assert.Nil(t, r.record("restored").Binding, "placed only in the pass after the snapshot is ready")
	r.pass()
	rec := r.record("restored")
	require.NotNil(t, rec.Binding)
	assert.Equal(t, at, rec.Binding.Host, "on the snapshot's host, though the other has more left")
	assert.Empty(t, r.hosts.Creates(other))
	creates := r.hosts.Creates(at)
	c := creates[len(creates)-1]
	assert.Equal(t, []hostapi.DiskSpec{{Name: "data", SizeBytes: 20 * gib, MountPath: "/data", FromSnapshot: "s-" + snap.ID + "-0"}}, c.Disks, "its volumes from the disk snapshots")
	assert.Equal(t, machineapi.PhaseRunning, rec.Status.Phase)
}

func TestReconcile_KeepsARestoreWaitingUntilItsSnapshotIsReady(t *testing.T) {
	r := newRig(t, host("h-a", 64*gib))
	r.create("src", spec())
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "src"})
	require.NoError(t, err)
	restore := spec()
	restore.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "before"}
	r.create("restored", restore)
	r.hosts.Fail("h-a")

	r.pass()
	rec := r.record("restored")
	assert.Nil(t, rec.Binding)
	assert.Equal(t, machineapi.PhasePending, rec.Status.Phase)
	assert.Equal(t, machines.ReasonSnapshotPending, rec.Status.Reason)
}

func TestReconcile_KeepsARestoreWaitingWhenTheSnapshotsHostIsFull(t *testing.T) {
	// 14 GiB less the reserve holds the source and 8 GiB more.
	r := newRig(t, host("h-a", 14*gib), host("h-b", 64*gib))
	r.hosts.Fail("h-b")
	r.create("src", spec())
	r.pass()
	r.hosts.Recover("h-b")
	require.Equal(t, "h-a", r.record("src").Binding.Host)
	_, _, err := r.svc.PutSnapshot(t.Context(), "garam", "before", machineapi.SnapshotRequest{Machine: "src"})
	require.NoError(t, err)
	r.pass()

	big := spec()
	big.MemoryBytes = 9 * gib
	big.FromSnapshot = &machineapi.SnapshotSource{Snapshot: "before"}
	r.create("restored", big)
	r.pass()
	rec := r.record("restored")
	assert.Nil(t, rec.Binding, "not on h-b, which has room")
	assert.Equal(t, machineapi.PhasePending, rec.Status.Phase)
	assert.Equal(t, "the snapshot's host does not meet it: no host fits the memory", rec.Status.Reason)
	assert.Equal(t, "the snapshot's host h-a does not meet it: no host has 9.0 GiB of memory free; the most free is 8.0 GiB", rec.OperatorReason)
	assert.Empty(t, r.hosts.Creates("h-b"))
}

// The Machine API's bound on a stop's grace is the one the host API takes, since orch passes a
// caller's grace to the host as it is.
func TestGraceBound_IsTheHostAPIs(t *testing.T) {
	assert.Equal(t, hostapi.MaxGraceSeconds, machineapi.MaxGraceSeconds)
}
