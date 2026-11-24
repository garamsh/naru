package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

const (
	// DefaultInterval is the pause between passes, --machine-interval's default (ADR 0109 item 4).
	DefaultInterval = 5 * time.Second
	// RestartEvery is how often, at most, a Machine that stopped on its own while desired running
	// is started again.
	RestartEvery = time.Minute
	// parallel bounds how many Machines one pass drives on their hosts at once.
	parallel = 16
)

// The reasons a caller reads in a Machine's status.
const (
	ReasonNotAnswering    = "the Machine's host does not answer"
	ReasonSnapshotPending = "the snapshot is not ready"
	ReasonSnapshotGone    = "the snapshot was deleted"
	ReasonSnapshotFailed  = "the snapshot failed"
	ReasonMachineDeleted  = "the Machine was deleted"
	ReasonRestartWait     = "the Machine stopped on its own; it is started again at most once a minute"
)

// The types of the fleet's events the reconciler appends, with the Machine as caller/name for
// subject.
const (
	// EventRestart is a Machine that stopped on its own started again; each attempt is one.
	EventRestart = "machine.restart"
	// EventOlderEpoch is the microVM of an older epoch of a live Machine stopped, and left on its
	// host with its disks for the move that settles it.
	EventOlderEpoch = "machine.older-epoch"
)

// ReconcilerConfig is what the reconciler runs over.
type ReconcilerConfig struct {
	Store Store
	Hosts Hosts
	Clock Clock
	// Interval is the pause between passes; DefaultInterval when zero.
	Interval time.Duration
	Reserve  Reserve
	// Wake, when not nil, returns a channel that is sent on after each write of a Machine or a
	// snapshot, for a pass at once; the ticker still runs when the channel closes.
	Wake func(ctx context.Context) <-chan struct{}
	// Fleet is the fleet's event log.
	Fleet Appender
	Log   *slog.Logger
}

// Reconciler is the leader's Machine reconciler (ADR 0109 item 4, ADR 0110): it places each
// Machine on a host and makes the host run it as its record asks.
type Reconciler struct {
	cfg ReconcilerConfig
}

// NewReconciler returns the reconciler of cfg.
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Reconciler{cfg: cfg}
}

// Run runs a pass every interval and after each wake until ctx ends.
func (r *Reconciler) Run(ctx context.Context) error {
	var wake <-chan struct{}
	if r.cfg.Wake != nil {
		wake = r.cfg.Wake(ctx)
	}
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		if err := r.Reconcile(ctx); err != nil && ctx.Err() == nil {
			r.cfg.Log.Error("machine pass failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		}
	}
}

// pass is what one pass observed, and the allocations it keeps current as it writes them.
type pass struct {
	hosts   map[string]Host
	entries []*Entry
	byID    map[string]*Entry
	snaps   []SnapshotEntry
	// bySnapshot is each caller's snapshots by name.
	bySnapshot map[string]map[string]*SnapshotEntry

	mu     sync.Mutex
	allocs map[string]Allocation
}

// Reconcile runs one pass. It plans from what it read at its start alone (ADR 0109 item 4).
func (r *Reconciler) Reconcile(ctx context.Context) error {
	p, err := r.observe(ctx)
	if err != nil {
		return err
	}
	errs := []error{r.place(ctx, p)}
	errs = append(errs, r.drive(ctx, p)...)
	errs = append(errs, r.snapshot(ctx, p)...)
	errs = append(errs, r.sweep(ctx, p)...)
	errs = append(errs, r.deleteDiskSnapshots(ctx, p)...)
	return errors.Join(errs...)
}

func (r *Reconciler) observe(ctx context.Context) (*pass, error) {
	hosts, err := r.cfg.Hosts.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list hosts: %w", err)
	}
	entries, err := r.cfg.Store.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Machines: %w", err)
	}
	snaps, err := r.cfg.Store.AllSnapshots(ctx)
	if err != nil {
		return nil, fmt.Errorf("read snapshots: %w", err)
	}
	allocs, err := r.cfg.Store.Allocations(ctx)
	if err != nil {
		return nil, fmt.Errorf("read allocations: %w", err)
	}
	p := &pass{hosts: map[string]Host{}, byID: map[string]*Entry{}, snaps: snaps, bySnapshot: map[string]map[string]*SnapshotEntry{}, allocs: allocs}
	for _, h := range hosts {
		p.hosts[h.ID] = h
	}
	for i := range entries {
		e := &entries[i]
		p.entries = append(p.entries, e)
		p.byID[e.ID] = e
	}
	for i := range p.snaps {
		s := &p.snaps[i]
		if p.bySnapshot[s.Caller] == nil {
			p.bySnapshot[s.Caller] = map[string]*SnapshotEntry{}
		}
		p.bySnapshot[s.Caller][s.Snapshot.Snapshot.Name] = s
	}
	return p, nil
}

// answering returns the host, when it is admitted and answered this pass.
func (p *pass) answering(id string) (Host, bool) {
	h, ok := p.hosts[id]
	return h, ok && h.Answering
}

// candidates are the admitted hosts with their accounts: a microVM of a current binding is counted
// through its host's allocation, and every other one by what it takes.
func (r *Reconciler) candidates(p *pass) []candidate {
	bound := map[string]bool{}
	for _, e := range p.entries {
		if e.Binding != nil {
			bound[e.Binding.MicroVM(e.ID)] = true
		}
	}
	out := make([]candidate, 0, len(p.hosts))
	for _, id := range slices.Sorted(maps.Keys(p.hosts)) {
		h := p.hosts[id]
		allocatable, left := account(h, r.cfg.Reserve, p.allocs[id].Resources, bound)
		out = append(out, candidate{Host: h, allocatable: allocatable, left: left})
	}
	return out
}

// place binds each Machine that has no host to the one placement chooses, or records why none
// fits. A Machine deleted before it was placed is removed.
func (r *Reconciler) place(ctx context.Context, p *pass) error {
	cs := r.candidates(p)
	var errs []error
	for _, e := range p.entries {
		if e.Binding != nil {
			continue
		}
		if e.Status.Phase == machineapi.PhaseDeleting {
			if err := r.remove(ctx, p, e); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		onto, reason := "", ""
		if from := e.Spec.FromSnapshot; from != nil {
			s := p.bySnapshot[e.Caller][from.Snapshot]
			switch {
			case s == nil:
				reason = ReasonSnapshotGone
			case s.Snapshot.Snapshot.Phase == machineapi.SnapshotFailed:
				reason = ReasonSnapshotFailed
			case s.Snapshot.Snapshot.Phase != machineapi.SnapshotReady || s.Host == "":
				reason = ReasonSnapshotPending
			default:
				onto = s.Host
			}
			if reason != "" {
				phase := machineapi.PhasePending
				if reason != ReasonSnapshotPending {
					phase = machineapi.PhaseFailed
				}
				errs = append(errs, r.set(ctx, e, func(rec *Record) {
					rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = phase, reason, reason
				}))
				continue
			}
		}
		pl := place(e.ID, e.Spec, cs, onto)
		if pl.host == "" {
			errs = append(errs, r.set(ctx, e, func(rec *Record) {
				rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhasePending, pl.reason, pl.operatorReason
			}))
			continue
		}
		if err := r.bind(ctx, p, e, pl.host); err != nil {
			errs = append(errs, err)
			continue
		}
		n := need(e.Spec)
		for i := range cs {
			if cs[i].ID == pl.host {
				cs[i].left = cs[i].left.minus(n)
			}
		}
	}
	return errors.Join(errs...)
}

// bind binds e to host under its first epoch, adding what it needs to the host's allocation in the
// same transaction.
func (r *Reconciler) bind(ctx context.Context, p *pass, e *Entry, host string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := e.Record
	next.Binding = &Binding{Host: host, Epoch: 1}
	next.Status.Reason, next.OperatorReason = "", ""
	alloc := p.allocs[host]
	alloc.Resources = of(alloc.Resources).plus(need(e.Spec)).resources()
	rev, err := r.cfg.Store.Bind(ctx, next, e.Rev, alloc)
	if err != nil {
		return fmt.Errorf("bind %s/%s to %s: %w", e.Caller, e.Name, host, err)
	}
	e.Record, e.Rev = next, rev
	alloc.Rev = rev
	p.allocs[host] = alloc
	return nil
}

// remove removes a deleted Machine whose microVM is gone, freeing its name and its host's
// allocation, with its deleted event.
func (r *Reconciler) remove(ctx context.Context, p *pass, e *Entry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev := r.event(machineapi.EventDeleted, e.Record)
	var alloc *Allocation
	if e.Binding != nil {
		a := p.allocs[e.Binding.Host]
		a.Resources = of(a.Resources).minus(need(e.Spec)).resources()
		alloc = &a
	}
	rev, err := r.cfg.Store.Remove(ctx, e.Record, e.Rev, alloc, ev)
	if err != nil {
		return fmt.Errorf("remove %s/%s: %w", e.Caller, e.Name, err)
	}
	if alloc != nil {
		alloc.Rev = rev
		p.allocs[e.Binding.Host] = *alloc
	}
	return nil
}

// drive brings each bound Machine's microVM to its record, on its host, several Machines at once.
func (r *Reconciler) drive(ctx context.Context, p *pass) []error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, parallel)
	for _, e := range p.entries {
		if e.Binding == nil {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := r.driveOne(ctx, p, e); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s/%s: %w", e.Caller, e.Name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errs
}

func (r *Reconciler) driveOne(ctx context.Context, p *pass, e *Entry) error {
	b := *e.Binding
	name := b.MicroVM(e.ID)
	h, ok := p.answering(b.Host)
	if !ok {
		if e.Status.Phase == machineapi.PhaseDeleting {
			return nil
		}
		// Nothing is inferred of a host that does not answer, and nothing is placed elsewhere.
		return r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseUnknown, ReasonNotAnswering, ReasonNotAnswering+": "+b.Host
		})
	}
	if e.Status.Phase == machineapi.PhaseDeleting {
		if err := r.cfg.Hosts.Delete(ctx, b.Host, name); err != nil {
			return fmt.Errorf("delete %s on %s: %w", name, b.Host, err)
		}
		return r.remove(ctx, p, e)
	}
	vm, ok := h.microVM(name)
	if !ok {
		return r.create(ctx, p, e)
	}
	return r.converge(ctx, e, vm)
}

// create makes the Machine's microVM on its host, which starts it, and stops it after when it is
// desired stopped.
func (r *Reconciler) create(ctx context.Context, p *pass, e *Entry) error {
	b := *e.Binding
	req, err := r.request(p, e)
	if err != nil {
		return r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseFailed, err.Error(), err.Error()
		})
	}
	if err := r.set(ctx, e, func(rec *Record) {
		rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseStarting, "", ""
	}); err != nil {
		return err
	}
	vm, err := r.cfg.Hosts.Create(ctx, b.Host, req)
	if refused(err) {
		var he *hostapi.Error
		errors.As(err, &he)
		reason := "the host refused the Machine: " + he.Message
		return r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseFailed, reason, reason
			rec.Status.ObservedGeneration = rec.Status.Generation
		})
	}
	if err != nil {
		return fmt.Errorf("create %s on %s: %w", req.Name, b.Host, err)
	}
	return r.converge(ctx, e, vm)
}

// refused is a host's answer that the same request will meet again: a 4xx other than a conflict
// or a missing name.
func refused(err error) bool {
	var he *hostapi.Error
	return errors.As(err, &he) && he.Status >= 400 && he.Status < 500 &&
		he.Status != http.StatusConflict && he.Status != http.StatusNotFound && he.Status != http.StatusTooManyRequests
}

// The states the host API reports of a microVM.
const (
	vmCreated = "created"
	vmRunning = "running"
	vmPaused  = "paused"
	vmStopped = "stopped"
	vmFaulted = "faulted"
)

// converge starts or stops the microVM to the desired state, and records the phase it reached.
// A desired state the reconciler has not yet acted on is acted on at once; a Machine that stopped
// on its own after that is started again at most once per RestartEvery.
func (r *Reconciler) converge(ctx context.Context, e *Entry, vm hostapi.MicroVM) error {
	b := *e.Binding
	name := b.MicroVM(e.ID)
	fresh := e.Status.ObservedGeneration < e.Status.Generation
	running := vm.State == vmRunning || vm.State == vmPaused
	var err error
	switch {
	case e.Spec.State == machineapi.StateStopped && running:
		if err := r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseStopping, "", ""
			rec.Status.ObservedGeneration = rec.Status.Generation
		}); err != nil {
			return err
		}
		vm, err = r.cfg.Hosts.Stop(ctx, b.Host, name, e.StopGraceSeconds)
	case e.Spec.State != machineapi.StateStopped && !running:
		now := r.cfg.Clock.Now()
		if !fresh && vm.State != vmCreated && now.Sub(e.Restarted) < RestartEvery {
			return r.set(ctx, e, func(rec *Record) {
				rec.Status.Phase, rec.Status.Reason = stopped(vm), ReasonRestartWait
				rec.OperatorReason = ReasonRestartWait
				if vm.Failure != "" {
					rec.OperatorReason += ": " + vm.Failure
				}
			})
		}
		if err := r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseStarting, "", ""
			if !fresh && vm.State != vmCreated {
				rec.Restarted = now
			}
			rec.Status.ObservedGeneration = rec.Status.Generation
		}); err != nil {
			return err
		}
		if !fresh && vm.State != vmCreated {
			r.fleet(ctx, EventRestart, e.Record, map[string]string{"host": b.Host, "microvm": name, "state": vm.State, "failure": vm.Failure})
		}
		vm, err = r.cfg.Hosts.Start(ctx, b.Host, name)
	}
	if err != nil {
		return fmt.Errorf("%s on %s: %w", name, b.Host, err)
	}
	return r.set(ctx, e, func(rec *Record) {
		rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = phaseOf(vm), "", ""
		rec.Status.ObservedGeneration = rec.Status.Generation
	})
}

// phaseOf is the phase of a microVM in the state the host reports.
func phaseOf(vm hostapi.MicroVM) machineapi.Phase {
	switch vm.State {
	case vmRunning, vmPaused:
		return machineapi.PhaseRunning
	case vmCreated:
		return machineapi.PhaseStarting
	}
	return stopped(vm)
}

func stopped(vm hostapi.MicroVM) machineapi.Phase {
	if vm.State == vmFaulted {
		return machineapi.PhaseFailed
	}
	return machineapi.PhaseStopped
}

// request is the host API's body for the Machine's microVM: created unfenced, with restartOnBoot
// (ADR 0109 item 1), its volumes as disks at their paths, from the snapshot's disk snapshots for a
// restore.
func (r *Reconciler) request(p *pass, e *Entry) (hostapi.CreateRequest, error) {
	s := e.Spec
	restart := true
	req := hostapi.CreateRequest{
		Name: e.Binding.MicroVM(e.ID), Image: s.Image, CPU: s.CPU, Memory: s.MemoryBytes, RootDiskBytes: s.RootDiskBytes,
		Env: s.Env, Command: s.Command, Args: s.Args, User: s.User, WorkingDir: s.WorkingDir, RestartOnBoot: &restart,
	}
	for _, rule := range s.NetworkPolicy.Egress {
		out := hostapi.Rule{Prefix: rule.Prefix, Protocol: rule.Protocol}
		for _, pr := range rule.Ports {
			out.Ports = append(out.Ports, hostapi.PortRange{First: uint32(pr.First), Last: uint32(pr.Last)})
		}
		req.NetworkPolicy.Egress = append(req.NetworkPolicy.Egress, out)
	}
	for _, f := range s.Files {
		req.Files = append(req.Files, hostapi.File{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	var snap *SnapshotEntry
	if from := s.FromSnapshot; from != nil {
		snap = p.bySnapshot[e.Caller][from.Snapshot]
		if snap == nil {
			return hostapi.CreateRequest{}, errors.New(ReasonSnapshotGone)
		}
	}
	for _, v := range s.Volumes {
		d := hostapi.DiskSpec{Name: v.Name, SizeBytes: v.SizeBytes, MountPath: v.MountPath}
		if snap != nil {
			i := slices.IndexFunc(snap.Snapshot.Snapshot.Spec.Volumes, func(w machineapi.Volume) bool { return w.Name == v.Name })
			if i < 0 {
				return hostapi.CreateRequest{}, fmt.Errorf("the snapshot has no volume %s", v.Name)
			}
			d.FromSnapshot = snap.DiskSnapshot(i)
		}
		req.Disks = append(req.Disks, d)
	}
	return req, nil
}

// snapshot takes each pending snapshot on its Machine's host: it records the host first, so a
// deletion that follows finds the disk snapshots, then takes one per volume, then marks it ready.
// A snapshot whose Machine is gone or being deleted fails.
func (r *Reconciler) snapshot(ctx context.Context, p *pass) []error {
	var errs []error
	for i := range p.snaps {
		s := &p.snaps[i]
		if s.Snapshot.Snapshot.Phase != machineapi.SnapshotPending {
			continue
		}
		e := p.byID[s.Snapshot.Snapshot.MachineID]
		if e == nil || e.Status.Phase == machineapi.PhaseDeleting {
			errs = append(errs, r.setSnapshot(ctx, s, func(n *Snapshot) {
				n.Snapshot.Phase, n.Snapshot.Reason = machineapi.SnapshotFailed, ReasonMachineDeleted
			}))
			continue
		}
		if e.Binding == nil {
			continue
		}
		b := *e.Binding
		h, ok := p.answering(b.Host)
		if !ok {
			continue
		}
		name := b.MicroVM(e.ID)
		if _, ok := h.microVM(name); !ok {
			continue
		}
		if s.Host == "" {
			if err := r.setSnapshot(ctx, s, func(n *Snapshot) { n.Host = b.Host }); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if s.Host != b.Host {
			continue
		}
		var err error
		for i, v := range s.Snapshot.Snapshot.Spec.Volumes {
			if err = r.cfg.Hosts.SnapshotDisk(ctx, b.Host, name, v.Name, s.DiskSnapshot(i)); err != nil {
				break
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("snapshot %s/%s: %w", s.Caller, s.Snapshot.Snapshot.Name, err))
			continue
		}
		errs = append(errs, r.setSnapshot(ctx, s, func(n *Snapshot) { n.Snapshot.Phase = machineapi.SnapshotReady }))
	}
	return errs
}

// sweep reaches the microVMs the reconciler named on each host that answers and that no current
// binding names: one of a Machine that is gone is deleted with its disks, and one of an older
// epoch of a live Machine is stopped and left with its disks, with an event.
func (r *Reconciler) sweep(ctx context.Context, p *pass) []error {
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(p.hosts)) {
		h := p.hosts[id]
		if !h.Answering {
			continue
		}
		for _, vm := range h.MicroVMs {
			mid, epoch, ok := parseMicroVM(vm.Name)
			if !ok {
				continue
			}
			e := p.byID[mid]
			switch {
			case e == nil:
				if err := r.cfg.Hosts.Delete(ctx, h.ID, vm.Name); err != nil {
					errs = append(errs, fmt.Errorf("delete %s on %s: %w", vm.Name, h.ID, err))
				}
			case e.Binding != nil && e.Binding.Host == h.ID && e.Binding.Epoch == epoch:
			case vm.State == vmRunning || vm.State == vmPaused:
				if _, err := r.cfg.Hosts.Stop(ctx, h.ID, vm.Name, 0); err != nil {
					errs = append(errs, fmt.Errorf("stop %s on %s: %w", vm.Name, h.ID, err))
					continue
				}
				r.fleet(ctx, EventOlderEpoch, e.Record, map[string]string{"host": h.ID, "microvm": vm.Name})
			}
		}
	}
	return errs
}

// microVMName is a name the reconciler gives: m-<id>-<epoch>.
var microVMName = regexp.MustCompile(`^` + MicroVMPrefix + `([0-9a-f]{16})-([1-9][0-9]*)$`)

func parseMicroVM(name string) (id string, epoch uint64, ok bool) {
	m := microVMName.FindStringSubmatch(name)
	if m == nil {
		return "", 0, false
	}
	epoch, err := strconv.ParseUint(m[2], 10, 64)
	return m[1], epoch, err == nil
}

// deleteDiskSnapshots deletes the disk snapshots of each deleted snapshot on its host, once the host
// answers, and then the deletion.
func (r *Reconciler) deleteDiskSnapshots(ctx context.Context, p *pass) []error {
	dels, err := r.cfg.Store.Deletions(ctx)
	if err != nil {
		return []error{fmt.Errorf("read deletions: %w", err)}
	}
	var errs []error
	for _, d := range dels {
		if _, ok := p.answering(d.Host); !ok {
			continue
		}
		var err error
		for _, disk := range d.Disks {
			if err = r.cfg.Hosts.DeleteDiskSnapshot(ctx, d.Host, disk); err != nil {
				break
			}
		}
		if err == nil {
			err = r.cfg.Store.DropDeletion(ctx, d.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("delete the disk snapshots of %s on %s: %w", d.ID, d.Host, err))
		}
	}
	return errs
}

// set writes the change to e's record when it changes anything, with a status event when it
// changes the status, and keeps e current.
func (r *Reconciler) set(ctx context.Context, e *Entry, change func(*Record)) error {
	next := e.Record
	change(&next)
	if reflect.DeepEqual(next, e.Record) {
		return nil
	}
	var ev *machineapi.Event
	if !reflect.DeepEqual(next.Status, e.Status) {
		x := r.event(machineapi.EventStatus, next)
		ev = &x
	}
	rev, err := r.cfg.Store.Update(ctx, next, e.Rev, ev)
	if err != nil {
		return fmt.Errorf("write %s/%s: %w", e.Caller, e.Name, err)
	}
	e.Record, e.Rev = next, rev
	return nil
}

func (r *Reconciler) setSnapshot(ctx context.Context, s *SnapshotEntry, change func(*Snapshot)) error {
	next := s.Snapshot
	change(&next)
	if reflect.DeepEqual(next, s.Snapshot) {
		return nil
	}
	rev, err := r.cfg.Store.UpdateSnapshot(ctx, next, s.Rev)
	if err != nil {
		return fmt.Errorf("write snapshot %s/%s: %w", s.Caller, s.Snapshot.Snapshot.Name, err)
	}
	s.Snapshot, s.Rev = next, rev
	return nil
}

func (r *Reconciler) event(typ machineapi.EventType, rec Record) machineapi.Event {
	return machineapi.Event{Type: typ, Machine: rec.Name, ID: rec.ID, Status: rec.Status, Time: r.cfg.Clock.Now().UTC()}
}

// fleet appends an event to the fleet's log; a failure is logged, and the pass goes on.
func (r *Reconciler) fleet(ctx context.Context, typ string, rec Record, detail map[string]string) {
	if r.cfg.Fleet == nil {
		return
	}
	detail["id"] = rec.ID
	ev := orchapi.Event{Time: r.cfg.Clock.Now().UTC(), Type: typ, Subject: rec.Caller + "/" + rec.Name, Detail: detail}
	if err := r.cfg.Fleet.AppendEvent(ctx, ev); err != nil {
		r.cfg.Log.Error("append a fleet event", "type", typ, "subject", ev.Subject, "err", err.Error())
	}
}
