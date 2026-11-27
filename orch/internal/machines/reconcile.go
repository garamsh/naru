package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

const (
	// DefaultInterval is the pause between passes, --machine-interval's default (ADR 0109 item 4).
	DefaultInterval = 5 * time.Second
	// DefaultHostTimeout bounds the work of one pass on one host.
	DefaultHostTimeout = 10 * time.Minute
	// RestartEvery is how often, at most, a Machine that stopped on its own while desired running
	// is started again.
	RestartEvery = time.Minute
	// MaxSnapshotRefusals is how many refusals of its disk snapshots fail a snapshot.
	MaxSnapshotRefusals = 3
	// parallel bounds how many Machines one host's work drives at once.
	parallel = 4
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

// The types of the fleet's events the reconciler appends. A Machine's has caller/name for subject;
// a host's thing has host/name.
const (
	// EventRestart is a Machine that stopped on its own started again; each attempt is one.
	EventRestart = "machine.restart"
	// EventOlderEpoch is the microVM of an older epoch of a live Machine stopped, and left on its
	// host with its disks for the move that settles it.
	EventOlderEpoch = "machine.older-epoch"
	// EventUnknownMicroVM is a microVM named as a Machine's whose id has neither a record nor a
	// tombstone: it is left as it is, disks and all (ADR 0109 item 7). One per microVM per leader.
	EventUnknownMicroVM = "machine.unknown-microvm"
	// EventUnknownDiskSnapshot is a disk snapshot named as a snapshot's whose id has neither a
	// snapshot nor a deletion: it is left as it is. One per disk snapshot per leader.
	EventUnknownDiskSnapshot = "machine.unknown-disk-snapshot"
	// EventAllocationDrift is a host whose allocation is not the sum of its bindings. One per change.
	EventAllocationDrift = "machine.allocation-drift"
)

// ReconcilerConfig is what the reconciler runs over.
type ReconcilerConfig struct {
	Store Store
	Hosts Hosts
	Clock Clock
	// Interval is the pause between passes, and the longest a pass waits for a host's work;
	// DefaultInterval when zero.
	Interval time.Duration
	// HostTimeout bounds the work of one pass on one host; DefaultHostTimeout when zero.
	HostTimeout time.Duration
	Reserve     Reserve
	// Wake, when not nil, returns a channel that is sent on after each Machine API write the
	// reconciler acts on, for a pass at once; the ticker still runs when the channel closes.
	Wake func(ctx context.Context) <-chan struct{}
	// Fleet is the fleet's event log.
	Fleet Appender
	// Routes tells whether every proxy routes a Machine to its binding, which a move waits for
	// before it deletes its source; NoRoutes when nil.
	Routes RoutesDelivered
	Log    *slog.Logger
}

// Reconciler is the leader's Machine reconciler (ADR 0109 item 4, ADR 0110): it places each
// Machine on a host and makes the host run it as its record asks.
type Reconciler struct {
	cfg ReconcilerConfig

	mu sync.Mutex
	// busy are the hosts whose work an earlier pass left running; a pass gives them none.
	busy map[string]bool
	// noted are the unknown microVMs and disk snapshots already in the fleet's log, as host/name.
	noted map[string]bool
	// drift is each host's last allocation mismatch in the fleet's log.
	drift map[string]string
	// holds is the reason each draining host's drain holds, as last logged.
	holds map[string]string
	// moving are the Machines, by id, whose move an earlier pass left running; a pass runs none.
	moving map[string]bool
}

// NewReconciler returns the reconciler of cfg.
func NewReconciler(cfg ReconcilerConfig) *Reconciler {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.HostTimeout <= 0 {
		cfg.HostTimeout = DefaultHostTimeout
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Routes == nil {
		cfg.Routes = NoRoutes
	}
	return &Reconciler{cfg: cfg, busy: map[string]bool{}, noted: map[string]bool{}, drift: map[string]string{}, holds: map[string]string{}, moving: map[string]bool{}}
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
	// bySnapshot is each caller's snapshots by name, and snapByID every snapshot by its id.
	bySnapshot map[string]map[string]*SnapshotEntry
	snapByID   map[string]*SnapshotEntry
	deletions  map[string]Deletion
	tombstones map[string]Tombstone
	// epochs is the highest epoch of each id among the microVMs the hosts report.
	epochs map[string]uint64
	// busy are the hosts whose earlier work still ran when the pass began to read: what it read of
	// them may be older than that work's writes, so it gives them none.
	busy map[string]bool
	// shares are each host's Machines and pending snapshots, and machines every Machine as it was
	// when the host work began, which each host's work reads while the others write their own.
	shares   map[string]*share
	machines map[string]frozen

	mu     sync.Mutex
	allocs map[string]Allocation
	// errs are the host work's failures while the pass waits for it; late says it no longer does,
	// and a failure after that is logged.
	errs []error
	late bool
}

// Reconcile runs one pass. It plans from what it read at its start alone (ADR 0109 item 4). What
// needs no host is done first; then each host that answers gets its own work, with its own
// deadline, and the pass waits for it at most an interval: a host whose work outlasts that keeps it,
// and gets none from the passes that follow until it ends.
func (r *Reconciler) Reconcile(ctx context.Context) error {
	r.mu.Lock()
	busy := maps.Clone(r.busy)
	r.mu.Unlock()
	p, err := r.observe(ctx)
	if err != nil {
		return err
	}
	p.busy = busy
	r.audit(ctx, p)
	cs := r.candidates(p)
	errs := []error{r.place(ctx, p, cs), r.drain(ctx, p, cs), r.reserve(ctx, p, cs)}
	errs = append(errs, r.unattended(ctx, p)...)
	errs = append(errs, r.age(ctx, p)...)
	r.dispatch(ctx, p)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.late = true
	return errors.Join(append(errs, p.errs...)...)
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
	dels, err := r.cfg.Store.Deletions(ctx)
	if err != nil {
		return nil, fmt.Errorf("read deletions: %w", err)
	}
	tombs, err := r.cfg.Store.Tombstones(ctx)
	if err != nil {
		return nil, fmt.Errorf("read tombstones: %w", err)
	}
	p := &pass{
		hosts: map[string]Host{}, byID: map[string]*Entry{}, snaps: snaps, bySnapshot: map[string]map[string]*SnapshotEntry{},
		snapByID: map[string]*SnapshotEntry{}, deletions: map[string]Deletion{}, tombstones: tombs, epochs: map[string]uint64{}, allocs: allocs,
	}
	for _, h := range hosts {
		p.hosts[h.ID] = h
		for _, vm := range h.MicroVMs {
			if id, epoch, ok := parseMicroVM(vm.Name); ok {
				p.epochs[id] = max(p.epochs[id], epoch)
			}
		}
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
		p.snapByID[s.ID] = s
	}
	for _, d := range dels {
		p.deletions[d.ID] = d
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
		// A move's two microVMs are both counted through the allocations it holds.
		if m := e.Move; m != nil {
			bound[m.Source.MicroVM(e.ID)] = true
			if m.Target != nil {
				bound[m.Target.MicroVM(e.ID)] = true
			}
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

// audit compares each host's allocation with the sum of what its bindings take, and records a
// mismatch, once per change, in the log and the fleet's log. Placement keeps using the allocation,
// which the guards compare.
func (r *Reconciler) audit(ctx context.Context, p *pass) {
	sums := map[string]amounts{}
	for _, e := range p.entries {
		if e.Binding != nil {
			sums[e.Binding.Host] = sums[e.Binding.Host].plus(need(e.Spec))
		}
		// A move holds its target's capacity until its commit, and its source's from then until its
		// clean up.
		if m := e.Move; m != nil && m.Target != nil {
			held := m.Target.Host
			if m.Step.committed() {
				held = m.Source.Host
			}
			sums[held] = sums[held].plus(need(e.Spec))
		}
	}
	hosts := map[string]bool{}
	for h := range sums {
		hosts[h] = true
	}
	for h := range p.allocs {
		hosts[h] = true
	}
	for _, h := range slices.Sorted(maps.Keys(hosts)) {
		want, have := sums[h].resources(), p.allocs[h].Resources
		mismatch := ""
		if want != have {
			mismatch = fmt.Sprintf("allocation %+v, bindings %+v", have, want)
		}
		r.mu.Lock()
		changed := r.drift[h] != mismatch
		r.drift[h] = mismatch
		r.mu.Unlock()
		if changed && mismatch != "" {
			r.cfg.Log.Error("a host's allocation is not the sum of its bindings", "host", h, "allocation", fmt.Sprintf("%+v", have), "bindings", fmt.Sprintf("%+v", want))
			r.fleetEvent(ctx, EventAllocationDrift, h, map[string]string{"allocation": fmt.Sprintf("%+v", have), "bindings": fmt.Sprintf("%+v", want)})
		}
	}
}

// place binds each Machine that has no host and is not deleted to the one placement chooses, or
// records why none fits.
func (r *Reconciler) place(ctx context.Context, p *pass, cs []candidate) error {
	var errs []error
	for _, e := range p.entries {
		if e.Binding != nil || e.Status.Phase == machineapi.PhaseDeleting {
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

// bind binds e to host, adding what it needs to the host's allocation in the same transaction. The
// epoch is above any a host reports for the id, so a placement never takes the name of a microVM
// an earlier one left, as one an etcd restore forgot (ADR 0109 item 3).
func (r *Reconciler) bind(ctx context.Context, p *pass, e *Entry, host string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := e.Record
	next.Binding = &Binding{Host: host, Epoch: p.nextEpoch(e)}
	next.Epoch = next.Binding.Epoch
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

// unattended does for each Machine and snapshot what needs no host: a deleted Machine that was
// never placed goes, a placed one whose host does not answer shows unknown, and a pending snapshot
// whose Machine is gone or being deleted fails.
func (r *Reconciler) unattended(ctx context.Context, p *pass) []error {
	var errs []error
	for _, e := range p.entries {
		switch {
		case e.Binding == nil && e.Status.Phase == machineapi.PhaseDeleting:
			errs = append(errs, r.finish(ctx, p, e))
		case e.Binding == nil || e.Status.Phase == machineapi.PhaseDeleting || e.Move != nil:
		default:
			if _, ok := p.answering(e.Binding.Host); ok {
				continue
			}
			// Nothing is inferred of a host that does not answer, and nothing is placed elsewhere.
			host := e.Binding.Host
			errs = append(errs, r.set(ctx, e, func(rec *Record) {
				rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseUnknown, ReasonNotAnswering, ReasonNotAnswering+": "+host
			}))
		}
	}
	for i := range p.snaps {
		s := &p.snaps[i]
		if s.Snapshot.Snapshot.Phase != machineapi.SnapshotPending {
			continue
		}
		if e := p.byID[s.Snapshot.Snapshot.MachineID]; e == nil || e.Status.Phase == machineapi.PhaseDeleting {
			errs = append(errs, r.setSnapshot(ctx, s, nil, func(n *Snapshot) {
				n.Snapshot.Phase, n.Snapshot.Reason = machineapi.SnapshotFailed, ReasonMachineDeleted
			}))
		}
	}
	return errs
}

// age drops the tombstones, and the deletions done, that are TombstoneAge old.
func (r *Reconciler) age(ctx context.Context, p *pass) []error {
	now := r.cfg.Clock.Now()
	var errs []error
	for id, t := range p.tombstones {
		if now.Sub(t.Removed) >= TombstoneAge {
			errs = append(errs, r.cfg.Store.DropTombstone(ctx, id))
		}
	}
	for id, d := range p.deletions {
		if !d.Done.IsZero() && now.Sub(d.Done) >= TombstoneAge {
			errs = append(errs, r.cfg.Store.DropDeletion(ctx, id))
		}
	}
	return errs
}

// share is one host's part of a pass: the Machines bound there and the pending snapshots of
// those that are not being deleted.
type share struct {
	entries []*Entry
	snaps   []*SnapshotEntry
}

// frozen is a Machine as the host work began.
type frozen struct {
	caller, name string
	binding      *Binding
}

// divide gives each host its share of the pass, and freezes every Machine.
func (p *pass) divide() {
	p.shares, p.machines = map[string]*share{}, map[string]frozen{}
	of := func(host string) *share {
		if p.shares[host] == nil {
			p.shares[host] = &share{}
		}
		return p.shares[host]
	}
	for _, e := range p.entries {
		f := frozen{caller: e.Caller, name: e.Name}
		if e.Binding != nil {
			b := *e.Binding
			f.binding = &b
			// A Machine with a move is the move's: its host's work leaves it.
			if e.Move == nil {
				of(b.Host).entries = append(of(b.Host).entries, e)
			}
		}
		p.machines[e.ID] = f
	}
	for i := range p.snaps {
		s := &p.snaps[i]
		e := p.byID[s.Snapshot.Snapshot.MachineID]
		if s.Snapshot.Snapshot.Phase == machineapi.SnapshotPending && e != nil && e.Binding != nil && e.Status.Phase != machineapi.PhaseDeleting && e.Move == nil {
			of(e.Binding.Host).snaps = append(of(e.Binding.Host).snaps, s)
		}
	}
}

// dispatch starts each answering host's work that no earlier pass left running, and waits for it
// at most an interval.
func (r *Reconciler) dispatch(ctx context.Context, p *pass) {
	p.divide()
	// The moves are chosen before any host's work begins, which writes the entries it holds; an
	// entry with a move is in no host's share.
	var moves []*Entry
	for _, e := range p.entries {
		if e.Move != nil && (e.Move.Step != MoveRequested || e.Move.Cancel || e.Status.Phase == machineapi.PhaseDeleting) {
			moves = append(moves, e)
		}
	}
	var wg sync.WaitGroup
	for _, id := range slices.Sorted(maps.Keys(p.hosts)) {
		if !p.hosts[id].Answering {
			continue
		}
		r.mu.Lock()
		busy := r.busy[id] || p.busy[id]
		if !busy {
			r.busy[id] = true
		}
		r.mu.Unlock()
		if busy {
			r.cfg.Log.Warn("a host's work from an earlier pass still runs; this pass gives it none", "host", id)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			hctx, cancel := context.WithTimeout(ctx, r.cfg.HostTimeout)
			err := errors.Join(r.work(hctx, p, id)...)
			cancel()
			r.mu.Lock()
			delete(r.busy, id)
			r.mu.Unlock()
			if err == nil {
				return
			}
			err = fmt.Errorf("host %s: %w", id, err)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.late {
				r.cfg.Log.Error("machine host work failed after its pass", "err", err.Error())
				return
			}
			p.errs = append(p.errs, err)
		}()
	}
	for _, e := range moves {
		r.mu.Lock()
		running := r.moving[e.ID]
		r.moving[e.ID] = true
		r.mu.Unlock()
		if running {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.move(ctx, p, e)
			r.mu.Lock()
			delete(r.moving, e.ID)
			r.mu.Unlock()
			if err == nil {
				return
			}
			err = fmt.Errorf("move %s/%s: %w", e.Caller, e.Name, err)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.late {
				r.cfg.Log.Error("a move failed after its pass", "err", err.Error())
				return
			}
			p.errs = append(p.errs, err)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	t := time.NewTimer(r.cfg.Interval)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		r.cfg.Log.Warn("the pass ends with host work still running", "after", r.cfg.Interval.String())
	case <-ctx.Done():
	}
}

// work is one pass's work on one answering host: its Machines, several at once, then its
// snapshots, then what it holds that no record names.
func (r *Reconciler) work(ctx context.Context, p *pass, host string) []error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	sem := make(chan struct{}, parallel)
	sh := p.shares[host]
	if sh == nil {
		sh = &share{}
	}
	for _, e := range sh.entries {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := r.drive(ctx, p, e); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s/%s: %w", e.Caller, e.Name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	errs = append(errs, r.snapshot(ctx, p, host, sh.snaps)...)
	errs = append(errs, r.sweep(ctx, p, host)...)
	errs = append(errs, r.disks(ctx, p, host)...)
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

// setSnapshot writes the change to s when it changes anything, with del when it is not nil.
func (r *Reconciler) setSnapshot(ctx context.Context, s *SnapshotEntry, del *Deletion, change func(*Snapshot)) error {
	next := s.Snapshot
	change(&next)
	if reflect.DeepEqual(next, s.Snapshot) {
		return nil
	}
	rev, err := r.cfg.Store.UpdateSnapshot(ctx, next, s.Rev, del)
	if err != nil {
		return fmt.Errorf("write snapshot %s/%s: %w", s.Caller, s.Snapshot.Snapshot.Name, err)
	}
	s.Snapshot, s.Rev = next, rev
	return nil
}

func (r *Reconciler) event(typ machineapi.EventType, rec Record) machineapi.Event {
	return machineapi.Event{Type: typ, Machine: rec.Name, ID: rec.ID, Status: rec.Status, Time: r.cfg.Clock.Now().UTC()}
}

// fleet appends a Machine's event to the fleet's log.
func (r *Reconciler) fleet(ctx context.Context, typ string, rec Record, detail map[string]string) {
	detail["id"] = rec.ID
	r.fleetEvent(ctx, typ, rec.Caller+"/"+rec.Name, detail)
}

// fleetEvent appends an event to the fleet's log; a failure is logged, and the pass goes on.
func (r *Reconciler) fleetEvent(ctx context.Context, typ, subject string, detail map[string]string) {
	if r.cfg.Fleet == nil {
		return
	}
	ev := orchapi.Event{Time: r.cfg.Clock.Now().UTC(), Type: typ, Subject: subject, Detail: detail}
	if err := r.cfg.Fleet.AppendEvent(ctx, ev); err != nil {
		r.cfg.Log.Error("append a fleet event", "type", typ, "subject", subject, "err", err.Error())
	}
}

// note reports whether the host's thing named is not yet in the fleet's log, and marks it so.
func (r *Reconciler) note(host, name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := host + "/" + name
	if r.noted[k] {
		return false
	}
	r.noted[k] = true
	return true
}

// hostError is the host API's refusal in err, if it is one.
func hostError(err error) (*hostapi.Error, bool) {
	var he *hostapi.Error
	return he, errors.As(err, &he)
}
