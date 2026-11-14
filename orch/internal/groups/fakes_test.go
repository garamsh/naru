package groups_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/groups"
)

var t0 = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

type fakeMachine struct {
	spec      groups.MachineSpec
	state     groups.MachineState
	deadline  time.Time
	lapsedAt  time.Time
	createdAt time.Time
}

type fakeHost struct {
	host       groups.Host
	machines   map[string]*fakeMachine
	down       bool
	failCreate bool
}

// world is one in-memory stand-in for the hosts, the store, the events and the clock. It
// changes only through the interfaces the engine uses, plus the methods a test calls to
// break something.
type world struct {
	t       *testing.T
	mu      sync.Mutex
	now     time.Time
	hosts   map[string]*fakeHost
	groups  []groups.GroupSpec
	members map[string]groups.Member
	rev     int64
	events  []groups.Event

	// check runs after every call that changes a machine.
	check func()
	// afterWait runs inside Clock.After, with the number of waits so far.
	afterWait func(n int)
	waits     int
}

func newWorld(t *testing.T) *world {
	return &world{t: t, now: t0, hosts: map[string]*fakeHost{}, members: map[string]groups.Member{}}
}

func (w *world) engine() *groups.Engine {
	return groups.New(groups.Config{Hosts: w, Store: w, Clock: w, Events: w, Interval: time.Second})
}

func (w *world) addHost(id, region string, labels map[string]string, versions ...string) *fakeHost {
	h := &fakeHost{
		host: groups.Host{
			ID: id, Region: region, Labels: labels, Ready: true,
			Capacity: groups.Capacity{CPUs: 64, MemoryMiB: 65536}, NodeVersions: versions,
		},
		machines: map[string]*fakeMachine{},
	}
	w.hosts[id] = h
	return h
}

func (w *world) setGroup(spec groups.GroupSpec) {
	w.groups = slices.DeleteFunc(w.groups, func(g groups.GroupSpec) bool { return g.Name == spec.Name })
	w.groups = append(w.groups, spec)
}

// reconcile runs one pass as Run does, prints its plan, and checks that every action left
// exactly one event.
func (w *world) reconcile(e *groups.Engine) []groups.Action {
	w.t.Helper()
	ctx := context.Background()
	o, err := e.Observe(ctx)
	require.NoError(w.t, err)
	plan := groups.Plan(groups.Evaluate(o), o)
	for _, a := range plan {
		w.t.Logf("%s plan: %s", w.now.Format("15:04:05"), a)
	}
	before := len(w.events)
	require.NoError(w.t, e.Apply(ctx, plan))
	got := w.events[before:]
	require.Len(w.t, got, len(plan), "one event per action")
	for i, a := range plan {
		require.Equal(w.t, a.Member.ID, got[i].Member, "event %d names the action's member", i)
	}
	return plan
}

// settle reconciles until a pass plans nothing but holds, at most limit times.
func (w *world) settle(e *groups.Engine, limit int) int {
	w.t.Helper()
	for i := 1; i <= limit; i++ {
		acts := w.reconcile(e)
		if !slices.ContainsFunc(acts, func(a groups.Action) bool { return a.Kind != groups.ActionHold }) {
			return i
		}
	}
	w.t.Fatalf("not settled after %d passes", limit)
	return 0
}

func (w *world) advance(d time.Duration) {
	w.now = w.now.Add(d)
	for _, h := range w.hosts {
		for _, m := range h.machines {
			if m.spec.Fenced && m.state == groups.MachineRunning && !m.deadline.IsZero() && w.now.After(m.deadline) {
				m.state, m.lapsedAt = groups.MachineStopped, m.deadline
			}
		}
	}
}

// kill removes a machine behind the engine's back.
func (w *world) kill(id string) {
	for _, h := range w.hosts {
		delete(h.machines, id)
	}
}

func (w *world) machine(id string) (*fakeMachine, string) {
	for hid, h := range w.hosts {
		if m, ok := h.machines[id]; ok {
			return m, hid
		}
	}
	return nil, ""
}

// running counts the running machines of a group.
func (w *world) running(group string) int {
	n := 0
	for _, h := range w.hosts {
		for id, m := range h.machines {
			if m.state == groups.MachineRunning && len(id) > len(group) && id[:len(group)+1] == group+"-" {
				n++
			}
		}
	}
	return n
}

// live returns the group's members that are not forgotten, by ID.
func (w *world) live(group string) []groups.Member {
	var out []groups.Member
	for _, m := range w.members {
		if m.Group == group && m.Phase != groups.PhaseGone {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (w *world) kinds(k groups.EventKind) []groups.Event {
	var out []groups.Event
	for _, e := range w.events {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func (w *world) mutated() {
	if w.check != nil {
		w.check()
	}
}

// Clock.

func (w *world) Now() time.Time { return w.now }

func (w *world) After(d time.Duration) <-chan time.Time {
	w.waits++
	w.advance(d)
	if w.afterWait != nil {
		w.afterWait(w.waits)
	}
	c := make(chan time.Time, 1)
	c <- w.now
	return c
}

// Events.

func (w *world) Record(_ context.Context, e groups.Event) error {
	w.events = append(w.events, e)
	return nil
}

// Store.

func (w *world) ListGroups(context.Context) ([]groups.GroupSpec, error) {
	return slices.Clone(w.groups), nil
}

func (w *world) ListMembers(context.Context) ([]groups.Member, error) {
	out := make([]groups.Member, 0, len(w.members))
	for _, m := range w.members {
		out = append(out, m)
	}
	return out, nil
}

func (w *world) PutMember(_ context.Context, m groups.Member) (groups.Member, error) {
	if w.members[m.ID].Revision != m.Revision {
		return groups.Member{}, groups.ErrConflict
	}
	w.rev++
	m.Revision = w.rev
	w.members[m.ID] = m
	return m, nil
}

// Hosts.

func (w *world) List(context.Context) ([]groups.Host, error) {
	var out []groups.Host
	for _, fh := range w.hosts {
		if fh.down {
			continue
		}
		h := fh.host
		h.Machines = nil
		for id, m := range fh.machines {
			h.Machines = append(h.Machines, groups.Machine{ID: id, State: m.state})
		}
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (w *world) reachable(id string) (*fakeHost, error) {
	h, ok := w.hosts[id]
	if !ok || h.down {
		return nil, fmt.Errorf("host %s unreachable", id)
	}
	return h, nil
}

func (w *world) Create(_ context.Context, hostID string, spec groups.MachineSpec) error {
	h, err := w.reachable(hostID)
	if err != nil {
		return err
	}
	if h.failCreate {
		return errors.New("create refused")
	}
	m := &fakeMachine{spec: spec, state: groups.MachineRunning, createdAt: w.now}
	if spec.Fenced {
		m.state = groups.MachineCreated
	}
	h.machines[spec.ID] = m
	h.host.Capacity.CPUs -= spec.Template.CPUs
	h.host.Capacity.MemoryMiB -= spec.Template.MemoryMiB
	w.mutated()
	return nil
}

func (w *world) Delete(_ context.Context, hostID, id string) error {
	h, err := w.reachable(hostID)
	if err != nil {
		return err
	}
	delete(h.machines, id)
	w.mutated()
	return nil
}

func (w *world) Stop(_ context.Context, hostID, id string) error {
	h, err := w.reachable(hostID)
	if err != nil {
		return err
	}
	if m, ok := h.machines[id]; ok {
		m.state = groups.MachineStopped
	}
	w.mutated()
	return nil
}

func (w *world) Start(_ context.Context, hostID, id string) error {
	h, err := w.reachable(hostID)
	if err != nil {
		return err
	}
	m, ok := h.machines[id]
	if !ok {
		return fmt.Errorf("machine %s not found", id)
	}
	if m.spec.Fenced && (m.deadline.IsZero() || !w.now.Before(m.deadline)) {
		return fmt.Errorf("machine %s: fence not armed", id)
	}
	m.state = groups.MachineRunning
	w.mutated()
	return nil
}

func (w *world) RenewFence(_ context.Context, hostID, id string, deadline time.Time) error {
	h, err := w.reachable(hostID)
	if err != nil {
		return err
	}
	m, ok := h.machines[id]
	if !ok {
		return fmt.Errorf("machine %s not found", id)
	}
	m.deadline = deadline
	return nil
}

func (w *world) InspectFence(_ context.Context, hostID, id string) (time.Time, time.Time, error) {
	h, err := w.reachable(hostID)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	m, ok := h.machines[id]
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("machine %s not found", id)
	}
	return m.deadline, m.lapsedAt, nil
}

func spec(name string, count int) groups.GroupSpec {
	return groups.GroupSpec{
		Name: name, Count: count, Region: "kr-1",
		Template: groups.Template{Form: groups.FormImage, CPUs: 1, MemoryMiB: 512, Version: "v1"},
		Rollout:  groups.Rollout{MaxUnavailable: 1},
	}
}
