package machines

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

// A move takes a Machine from its host to another by stop, a host-to-host copy of its volumes and
// a start (ADR 0109 item 5). Its record, Record.Move, holds each step it reached, and every step
// repeats safely, so a new leader continues a move from the record alone. Before the commit the
// source is still bound and the target has never run, so an abort deletes what the target holds
// and starts the source again; from the commit the move completes forward.

// MoveStep is how far a move got; the steps are orchapi's, in their order.
type MoveStep string

const (
	MoveRequested MoveStep = orchapi.MoveRequested
	MoveReserved  MoveStep = orchapi.MoveReserved
	MoveStopped   MoveStep = orchapi.MoveStopped
	MoveCopied    MoveStep = orchapi.MoveCopied
	MoveCreated   MoveStep = orchapi.MoveCreated
	MoveCommitted MoveStep = orchapi.MoveCommitted
	MoveStarted   MoveStep = orchapi.MoveStarted
)

// committed reports whether the step is at or after the commit, from which the move goes forward.
func (s MoveStep) committed() bool { return s == MoveCommitted || s == MoveStarted }

// Move is a Machine's move in progress.
type Move struct {
	By        string             `json:"by"`
	Requested time.Time          `json:"requested"`
	Shape     *orchapi.MoveShape `json:"shape,omitempty"`
	Step      MoveStep           `json:"step"`
	// Source is the binding the move takes the Machine from, and Target the one it takes it to,
	// once reserved.
	Source Binding  `json:"source"`
	Target *Binding `json:"target,omitempty"`
	// Volumes are the volumes copied to the target, each with the SHA-256 both hosts agreed on.
	Volumes []MovedVolume `json:"volumes,omitempty"`
	// Cancel is an operator's cancellation, carried out before the commit.
	Cancel bool `json:"cancel,omitempty"`
	// Reason is why the move waits, for the operator.
	Reason string `json:"reason,omitempty"`
}

// MovedVolume is a volume whose copy on the target was verified: the target's mvmd made the disk
// only from bytes of this SHA-256, the digest the source read.
type MovedVolume struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// The fleet's events of a move.
const (
	// EventMoved is a move that completed: the Machine runs on the target and the source's copy is gone.
	EventMoved = "machine.moved"
	// EventMoveAborted is a move abandoned before its commit; the Machine stays on its source.
	EventMoveAborted = "machine.move-aborted"
	// EventDrainHeld is a draining host's Machine that fits on no other host: it stays, and the drain
	// of its host holds there. One per change.
	EventDrainHeld = "machine.drain-held"
)

// The limits of a drain (ADR 0110 item 7): a drain starts a move from a host with none in progress,
// and while fewer than drainFleet are in progress in the fleet.
const drainFleet = 2

// copyTimeout bounds one volume's export and import together. The hosts end a stream that stops
// moving; this is the backstop for one that moves too slowly to finish.
const copyTimeout = 6 * time.Hour

// RoutesDelivered reports whether every proxy host has loaded routes that send the Machine id to
// its binding b. A move deletes its source only once they have (ADR 0109 item 5).
type RoutesDelivered func(ctx context.Context, id string, b Binding) (bool, error)

// NoRoutes is the RoutesDelivered of an orch that pushes no routes: it answers yes, since no proxy
// holds a route to the source. #387, which derives the routing table and pushes it to the proxy
// hosts, replaces it with the revisions the proxies report loaded.
func NoRoutes(context.Context, string, Binding) (bool, error) { return true, nil }

// ErrNotPlaced is a move of a Machine that is bound to no host.
var ErrNotPlaced = errors.New("the Machine is not placed on a host")

// ErrMoving is a move of a Machine with another move in progress, or a cancellation of one past its
// commit.
var ErrMoving = errors.New("the Machine has a move in progress")

// ErrNoMove is a cancellation of a Machine that has no move.
var ErrNoMove = errors.New("the Machine has no move in progress")

// narrowed is the Machine's shape with the move's narrowing added; a narrowing that contradicts the
// shape is a FieldError.
func narrowed(s machineapi.Shape, n *orchapi.MoveShape) (machineapi.Shape, error) {
	if n == nil {
		return s, nil
	}
	out := s
	switch {
	case n.Region == "":
	case s.Region != "" && s.Region != n.Region:
		return s, &machineapi.FieldError{Field: "shape.region", Problem: "is not the Machine's region " + s.Region}
	default:
		out.Region = n.Region
	}
	out.Labels = maps.Clone(s.Labels)
	for _, k := range slices.Sorted(maps.Keys(n.Labels)) {
		if v, ok := s.Labels[k]; ok && v != n.Labels[k] {
			return s, &machineapi.FieldError{Field: "shape.labels." + k, Problem: "is not the Machine's " + v}
		}
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[k] = n.Labels[k]
	}
	return out, nil
}

// view is the move as the operator API shows it.
func (m *Move) view() *orchapi.MachineMove {
	if m == nil {
		return nil
	}
	v := &orchapi.MachineMove{By: m.By, Requested: m.Requested, Shape: m.Shape, Step: string(m.Step), Source: m.Source.Host, Cancelled: m.Cancel, Reason: m.Reason}
	if m.Target != nil {
		v.Target = m.Target.Host
	}
	return v
}

// nextEpoch is the epoch of the Machine's next placement: above every epoch a host reports for it
// and every epoch it took, so no two placements share a microVM name although the host that holds
// one does not answer.
func (p *pass) nextEpoch(e *Entry) uint64 {
	n := max(p.epochs[e.ID], e.Epoch)
	if e.Binding != nil {
		n = max(n, e.Binding.Epoch)
	}
	if m := e.Move; m != nil && m.Target != nil {
		n = max(n, m.Target.Epoch)
	}
	return n + 1
}

// drain asks a move of a Machine of each draining host that has none in progress, while the fleet
// has fewer than drainFleet. A host's Machines go in the order of their caller and name; one that
// fits on no other host stays, and the drain of its host holds at it, with an event.
func (r *Reconciler) drain(ctx context.Context, p *pass, cs []candidate) error {
	moving, from := 0, map[string]bool{}
	for _, e := range p.entries {
		if e.Move != nil {
			moving++
			from[e.Move.Source.Host] = true
		}
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(p.hosts)) {
		if !p.hosts[id].Draining || from[id] || moving >= drainFleet {
			continue
		}
		for _, e := range p.entries {
			if e.Binding == nil || e.Binding.Host != id || e.Move != nil || e.Status.Phase == machineapi.PhaseDeleting {
				continue
			}
			pl := place(e.ID, e.Spec, others(cs, id), "")
			if pl.host == "" {
				if r.held(id, e.ID+" "+pl.operatorReason) {
					r.cfg.Log.Warn("a draining host's Machine fits on no other host; the drain holds", "host", id, "machine", e.Caller+"/"+e.Name, "reason", pl.operatorReason)
					r.fleet(ctx, EventDrainHeld, e.Record, map[string]string{"host": id, "reason": pl.operatorReason})
				}
				break
			}
			r.held(id, "")
			src := *e.Binding
			if err := r.set(ctx, e, func(rec *Record) {
				rec.Move = &Move{By: orchapi.ByDrain, Requested: r.cfg.Clock.Now().UTC(), Step: MoveRequested, Source: src}
			}); err != nil {
				errs = append(errs, err)
				break
			}
			moving++
			break
		}
	}
	return errors.Join(errs...)
}

// held reports whether the reason the drain of the host holds changed, and records it; an empty
// reason is a drain that does not hold.
func (r *Reconciler) held(host, reason string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.holds[host] != reason
	r.holds[host] = reason
	return changed && reason != ""
}

// others are the candidates but the host.
func others(cs []candidate, host string) []candidate {
	return slices.DeleteFunc(slices.Clone(cs), func(c candidate) bool { return c.ID == host })
}

// reserve chooses each requested move's target as for a new Machine, its source excluded, and
// binds the target's capacity to the move in the same write as its account (ADR 0110 items 2 and
// 7). A move that fits nowhere waits, with the reason.
func (r *Reconciler) reserve(ctx context.Context, p *pass, cs []candidate) error {
	var errs []error
	for _, e := range p.entries {
		m := e.Move
		if m == nil || m.Step != MoveRequested || m.Cancel || e.Status.Phase == machineapi.PhaseDeleting {
			continue
		}
		shape, err := narrowed(e.Spec.Shape, m.Shape)
		if err != nil {
			errs = append(errs, r.waits(ctx, e, err.Error()))
			continue
		}
		spec := e.Spec
		spec.Shape = shape
		pl := place(e.ID, spec, others(cs, m.Source.Host), "")
		if pl.host == "" {
			errs = append(errs, r.waits(ctx, e, pl.operatorReason))
			continue
		}
		target := Binding{Host: pl.host, Epoch: p.nextEpoch(e)}
		if err := r.allocate(ctx, p, e, target.Host, need(e.Spec), func(rec *Record) {
			rec.Move.Step, rec.Move.Target, rec.Move.Reason = MoveReserved, &target, ""
			rec.Epoch = target.Epoch
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseMoving, "", ""
		}); err != nil {
			errs = append(errs, err)
			continue
		}
		for i := range cs {
			if cs[i].ID == pl.host {
				cs[i].left = cs[i].left.minus(need(e.Spec))
			}
		}
	}
	return errors.Join(errs...)
}

// allocate writes the change to e's record with the host's allocation changed by delta, which a
// release gives negative, in one transaction.
func (r *Reconciler) allocate(ctx context.Context, p *pass, e *Entry, host string, delta amounts, change func(*Record)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := e.Record
	if next.Move != nil {
		m := *next.Move
		next.Move = &m
	}
	change(&next)
	var ev *machineapi.Event
	if !reflect.DeepEqual(next.Status, e.Status) {
		x := r.event(machineapi.EventStatus, next)
		ev = &x
	}
	alloc := p.allocs[host]
	alloc.Resources = of(alloc.Resources).plus(delta).resources()
	rev, err := r.cfg.Store.Allocate(ctx, next, e.Rev, host, alloc, ev)
	if err != nil {
		return fmt.Errorf("write %s/%s with %s's allocation: %w", e.Caller, e.Name, host, err)
	}
	e.Record, e.Rev = next, rev
	alloc.Rev = rev
	p.allocs[host] = alloc
	return nil
}

// setMove writes the change to e's move, copying it first so e's own is left as it was on a failure.
func (r *Reconciler) setMove(ctx context.Context, e *Entry, change func(rec *Record, m *Move)) error {
	return r.set(ctx, e, func(rec *Record) {
		m := *rec.Move
		rec.Move = &m
		change(rec, &m)
	})
}

// waits records why the move waits, when it changed.
func (r *Reconciler) waits(ctx context.Context, e *Entry, reason string) error {
	return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Reason = reason })
}

// move carries e's move on from the step its record holds as far as it can now, one step at a
// time, each step written before the next begins.
func (r *Reconciler) move(ctx context.Context, p *pass, e *Entry) error {
	ranAgain := stoppedBefore(e.Move.Step) && p.runs(e.Move.Source, e.ID)
	for e.Move != nil {
		m := *e.Move
		before := m.Step
		var err error
		switch {
		case !m.Step.committed() && (m.Cancel || e.Status.Phase == machineapi.PhaseDeleting):
			return r.abort(ctx, p, e, abortReason(m, e))
		case ranAgain && !m.Step.committed():
			return r.abort(ctx, p, e, ReasonSourceRanAgain)
		case m.Step == MoveRequested:
			// reserve's: a pass reserves before its moves run.
			return nil
		case m.Step == MoveReserved:
			err = r.stopSource(ctx, p, e)
		case m.Step == MoveStopped:
			err = r.copyVolumes(ctx, p, e)
		case m.Step == MoveCopied:
			err = r.createTarget(ctx, p, e)
		case m.Step == MoveCreated:
			err = r.set(ctx, e, func(rec *Record) {
				mv := *rec.Move
				mv.Step, mv.Reason = MoveCommitted, ""
				rec.Move, rec.Binding = &mv, mv.Target
			})
		case m.Step == MoveCommitted:
			err = r.startTarget(ctx, p, e)
		case m.Step == MoveStarted:
			err = r.cleanUp(ctx, p, e)
		}
		if err != nil {
			return err
		}
		if e.Move != nil && e.Move.Step == before {
			// The step waits for a host, and says why.
			return nil
		}
	}
	return nil
}

// ReasonSourceRanAgain is why a move aborts whose source runs again after its stop, as its host's
// restartOnBoot starts it after a reboot: what it wrote since is on none of the copies.
const ReasonSourceRanAgain = "the source runs again since its stop, so the copies may miss what it wrote"

// stoppedBefore reports whether a move at the step has stopped its source and not yet committed.
func stoppedBefore(s MoveStep) bool {
	return s == MoveStopped || s == MoveCopied || s == MoveCreated
}

// runs reports whether the host, in an answer of this pass and not one its probe repeated,
// reports the binding's microVM of the Machine running or paused.
func (p *pass) runs(b Binding, id string) bool {
	h, ok := p.answering(b.Host)
	if !ok || h.Stale {
		return false
	}
	for _, vm := range h.MicroVMs {
		if vm.Name == b.MicroVM(id) {
			return vm.State == vmRunning || vm.State == vmPaused
		}
	}
	return false
}

func abortReason(m Move, e *Entry) string {
	if m.Cancel {
		return "the operator cancelled the move"
	}
	return ReasonMachineDeleted
}

// stopSource stops the source's microVM with the Machine's grace and takes the stop it reports as
// the acknowledgement: without it the move waits.
func (r *Reconciler) stopSource(ctx context.Context, p *pass, e *Entry) error {
	src := e.Move.Source
	if _, ok := p.answering(src.Host); !ok {
		return r.waits(ctx, e, "the source "+src.Host+" does not answer; the move waits for its stop")
	}
	vm, err := r.cfg.Hosts.Stop(ctx, src.Host, src.MicroVM(e.ID), min(e.StopGraceSeconds, hostapi.MaxGraceSeconds))
	if err != nil {
		return fmt.Errorf("stop %s on %s for its move: %w", src.MicroVM(e.ID), src.Host, err)
	}
	if vm.State == vmRunning || vm.State == vmPaused {
		return r.waits(ctx, e, "the source "+src.Host+" still reports the Machine "+vm.State)
	}
	return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step, m.Reason = MoveStopped, "" })
}

// copyVolumes copies each volume not yet copied from the source to the target: the source exports
// it with a grant bound to the target, and the target imports it under that grant and the SHA-256
// the export answered. Each verified copy is written as it ends, so a repeat copies the rest.
func (r *Reconciler) copyVolumes(ctx context.Context, p *pass, e *Entry) error {
	m := e.Move
	for _, h := range []string{m.Source.Host, m.Target.Host} {
		if _, ok := p.answering(h); !ok {
			return r.waits(ctx, e, "the host "+h+" does not answer; the move waits to copy the volumes")
		}
	}
	for _, v := range e.Spec.Volumes {
		if slices.ContainsFunc(e.Move.Volumes, func(c MovedVolume) bool { return c.Name == v.Name }) {
			continue
		}
		sum, err := r.copyVolume(ctx, e, v.Name)
		if err != nil {
			r.cfg.Log.Warn("a move's copy of a volume failed; it is copied again", "machine", e.Caller+"/"+e.Name, "volume", v.Name, "err", err.Error())
			return r.waits(ctx, e, "the copy of the volume "+v.Name+" failed and is tried again: "+err.Error())
		}
		if err := r.setMove(ctx, e, func(_ *Record, m *Move) {
			m.Volumes = append(slices.Clone(m.Volumes), MovedVolume{Name: v.Name, SHA256: sum})
		}); err != nil {
			return err
		}
	}
	return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step, m.Reason = MoveCopied, "" })
}

// copyVolume copies one volume and returns the SHA-256 both hosts agreed on.
func (r *Reconciler) copyVolume(ctx context.Context, e *Entry, volume string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()
	m := e.Move
	x, err := r.cfg.Hosts.ExportDisk(ctx, m.Source.Host, m.Source.MicroVM(e.ID)+"."+volume, m.Target.Host)
	if err != nil {
		return "", fmt.Errorf("export from %s: %w", m.Source.Host, err)
	}
	d, err := r.cfg.Hosts.ImportDisk(ctx, m.Target.Host, hostapi.ImportRequest{
		Name: m.Target.MicroVM(e.ID) + "." + volume, SourceHost: m.Source.Host, Grant: x.Grant,
		SizeBytes: x.SizeBytes, Filesystem: x.Filesystem, SHA256: x.SHA256,
	})
	if err != nil {
		return "", fmt.Errorf("import to %s: %w", m.Target.Host, err)
	}
	if d.SHA256 != x.SHA256 {
		return "", fmt.Errorf("the target made %s of SHA-256 %s, not the source's %s", d.Name, d.SHA256, x.SHA256)
	}
	return x.SHA256, nil
}

// createTarget makes the microVM on the target under the move's epoch, on the copied volumes, and
// does not start it. A target that refuses it ends the move before its commit.
func (r *Reconciler) createTarget(ctx context.Context, p *pass, e *Entry) error {
	t := *e.Move.Target
	if _, ok := p.answering(t.Host); !ok {
		return r.waits(ctx, e, "the target "+t.Host+" does not answer; the move waits to create the Machine there")
	}
	on := *e
	on.Binding = &t
	req, err := r.request(p, &on)
	if err != nil {
		return r.abort(ctx, p, e, err.Error())
	}
	req.NoStart = true
	for i := range req.Disks {
		req.Disks[i].FromSnapshot, req.Disks[i].Imported = "", true
	}
	if _, err := r.cfg.Hosts.Create(ctx, t.Host, req); err != nil {
		if refused(err) {
			he, _ := hostError(err)
			return r.abort(ctx, p, e, "the target refused the Machine: "+he.Message)
		}
		return fmt.Errorf("create %s on %s for its move: %w", req.Name, t.Host, err)
	}
	return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step, m.Reason = MoveCreated, "" })
}

// startTarget starts the Machine on the target when it is desired running, and records the step
// once the target reports it running; one desired stopped is left as it was made.
func (r *Reconciler) startTarget(ctx context.Context, p *pass, e *Entry) error {
	t := *e.Binding
	if e.Status.Phase == machineapi.PhaseDeleting {
		return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step = MoveStarted })
	}
	if _, ok := p.answering(t.Host); !ok {
		return r.waits(ctx, e, "the target "+t.Host+" does not answer; the move waits to start the Machine there")
	}
	phase := machineapi.PhaseStopped
	if e.Spec.State != machineapi.StateStopped {
		vm, err := r.cfg.Hosts.Start(ctx, t.Host, t.MicroVM(e.ID))
		if err != nil {
			return fmt.Errorf("start %s on %s for its move: %w", t.MicroVM(e.ID), t.Host, err)
		}
		if vm.State != vmRunning && vm.State != vmPaused {
			return r.waits(ctx, e, "the target "+t.Host+" reports the Machine "+vm.State+" after its start")
		}
		phase = machineapi.PhaseRunning
	}
	return r.setMove(ctx, e, func(rec *Record, m *Move) {
		m.Step, m.Reason = MoveStarted, ""
		rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = phase, "", ""
		rec.Status.ObservedGeneration = rec.Status.Generation
	})
}

// cleanUp deletes the source's microVM and its volumes, whose verified copies the target holds,
// once every proxy routes the Machine to the target, and then ends the move with the source's
// capacity freed. A Machine being deleted needs no routes.
func (r *Reconciler) cleanUp(ctx context.Context, p *pass, e *Entry) error {
	src := e.Move.Source
	if e.Status.Phase != machineapi.PhaseDeleting {
		ok, err := r.cfg.Routes(ctx, e.ID, *e.Binding)
		if err != nil {
			return fmt.Errorf("read the proxies' routes of %s/%s: %w", e.Caller, e.Name, err)
		}
		if !ok {
			return r.waits(ctx, e, "a proxy does not yet route the Machine to "+e.Binding.Host)
		}
	}
	if _, ok := p.answering(src.Host); !ok {
		return r.waits(ctx, e, "the source "+src.Host+" does not answer; the move waits to delete the Machine's copy there")
	}
	if err := r.cfg.Hosts.Delete(ctx, src.Host, src.MicroVM(e.ID)); err != nil {
		return fmt.Errorf("delete %s on %s after its move: %w", src.MicroVM(e.ID), src.Host, err)
	}
	m := *e.Move
	if err := r.allocate(ctx, p, e, src.Host, amounts{}.minus(need(e.Spec)), func(rec *Record) { rec.Move = nil }); err != nil {
		return err
	}
	r.fleet(ctx, EventMoved, e.Record, map[string]string{"from": src.Host, "to": e.Binding.Host, "by": m.By})
	return nil
}

// abort ends a move before its commit: it deletes what the target holds of it, the copied volumes
// with the microVM, frees the target's capacity and starts the source again when the Machine is
// desired running. A target that does not answer keeps its never-started copy, which an older epoch
// leaves for the Machine's deletion (ADR 0109 item 7).
func (r *Reconciler) abort(ctx context.Context, p *pass, e *Entry, reason string) error {
	m := *e.Move
	if t := m.Target; t != nil {
		if _, ok := p.answering(t.Host); ok {
			if err := r.cfg.Hosts.Delete(ctx, t.Host, t.MicroVM(e.ID)); err != nil {
				return fmt.Errorf("delete %s on %s for its move's abort: %w", t.MicroVM(e.ID), t.Host, err)
			}
		}
	}
	src := m.Source
	phase := e.Status.Phase
	if _, ok := p.answering(src.Host); ok && e.Status.Phase != machineapi.PhaseDeleting && m.Step != MoveRequested {
		vm := hostapi.MicroVM{State: "stopped"}
		if e.Spec.State != machineapi.StateStopped {
			var err error
			if vm, err = r.cfg.Hosts.Start(ctx, src.Host, src.MicroVM(e.ID)); err != nil {
				return fmt.Errorf("start %s on %s for its move's abort: %w", src.MicroVM(e.ID), src.Host, err)
			}
		}
		phase = phaseOf(vm)
		if vm.State == vmCreated && e.Spec.State == machineapi.StateStopped {
			phase = machineapi.PhaseStopped
		}
	}
	change := func(rec *Record) {
		rec.Move = nil
		if rec.Status.Phase == machineapi.PhaseMoving {
			rec.Status.Phase = phase
		}
	}
	var err error
	if m.Target != nil {
		err = r.allocate(ctx, p, e, m.Target.Host, amounts{}.minus(need(e.Spec)), change)
	} else {
		err = r.set(ctx, e, change)
	}
	if err != nil {
		return err
	}
	detail := map[string]string{"from": src.Host, "by": m.By, "step": string(m.Step), "reason": reason}
	if m.Target != nil {
		detail["to"] = m.Target.Host
	}
	r.fleet(ctx, EventMoveAborted, e.Record, detail)
	return nil
}

// Move records an operator's or automation's move of the caller's Machine. A repeat of the move in
// progress, with the same shape, answers it as it is.
func (s *service) Move(ctx context.Context, by, caller, name string, req orchapi.MoveRequest) (orchapi.Machine, error) {
	for range retries {
		r, rev, err := s.store.Machine(ctx, caller, name)
		if err != nil {
			return orchapi.Machine{}, notFound(err, "Machine", caller+"/"+name)
		}
		if _, err := narrowed(r.Spec.Shape, req.Shape); err != nil {
			return orchapi.Machine{}, err
		}
		switch {
		case r.Status.Phase == machineapi.PhaseDeleting:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s", ErrDeleting, caller, name)
		case r.Move != nil && !r.Move.Cancel && sameShape(r.Move.Shape, req.Shape):
			return operatorView(r), nil
		case r.Move != nil:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s, %s by %s", ErrMoving, caller, name, r.Move.Step, r.Move.By)
		case r.Binding == nil:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s", ErrNotPlaced, caller, name)
		}
		r.Move = &Move{By: by, Requested: s.clock.Now().UTC(), Shape: req.Shape, Step: MoveRequested, Source: *r.Binding}
		if _, err := s.store.Update(ctx, r, rev, nil); err == nil {
			return operatorView(r), nil
		} else if !errors.Is(err, ErrRaced) {
			return orchapi.Machine{}, err
		}
	}
	return orchapi.Machine{}, fmt.Errorf("move %s/%s: %d writes raced", caller, name, retries)
}

func sameShape(a, b *orchapi.MoveShape) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Region == b.Region && maps.Equal(a.Labels, b.Labels)
}

// CancelMove asks for the caller's Machine's move to be abandoned. One past its commit goes forward.
func (s *service) CancelMove(ctx context.Context, caller, name string) (orchapi.Machine, error) {
	for range retries {
		r, rev, err := s.store.Machine(ctx, caller, name)
		if err != nil {
			return orchapi.Machine{}, notFound(err, "Machine", caller+"/"+name)
		}
		switch {
		case r.Move == nil:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s", ErrNoMove, caller, name)
		case r.Move.Step.committed():
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s is past its commit and completes forward", ErrMoving, caller, name)
		case r.Move.Cancel:
			return operatorView(r), nil
		}
		m := *r.Move
		m.Cancel = true
		r.Move = &m
		if _, err := s.store.Update(ctx, r, rev, nil); err == nil {
			return operatorView(r), nil
		} else if !errors.Is(err, ErrRaced) {
			return orchapi.Machine{}, err
		}
	}
	return orchapi.Machine{}, fmt.Errorf("cancel the move of %s/%s: %d writes raced", caller, name, retries)
}

// OperatorMachine is the caller's Machine as the operator API shows it.
func (s *service) OperatorMachine(ctx context.Context, caller, name string) (orchapi.Machine, error) {
	r, _, err := s.store.Machine(ctx, caller, name)
	if err != nil {
		return orchapi.Machine{}, notFound(err, "Machine", caller+"/"+name)
	}
	return operatorView(r), nil
}

// operatorView is the record as the operator API shows it, with no secret in it.
func operatorView(r Record) orchapi.Machine {
	m := orchapi.Machine{Caller: r.Caller, Machine: r.Machine(), Reason: r.OperatorReason, Move: r.Move.view()}
	if r.Binding != nil {
		m.Binding = &orchapi.MachineBinding{Host: r.Binding.Host, Epoch: r.Binding.Epoch}
	}
	return m
}
