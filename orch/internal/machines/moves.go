package machines

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"
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
	// TargetAddress is the address the target gave the Machine's microVM there, which the commit
	// makes the Machine's private address; invalid while the target has reported none.
	TargetAddress netip.Addr `json:"targetAddress,omitzero"`
	// Routed is the first revision after the commit the clean up read, and when: no table of it or
	// later routes the Machine to the source, so the source is deleted once every proxy has
	// loaded one (ADR 0112 item 6).
	Routed *Unrouted `json:"routed,omitempty"`
	// Cancel is an operator's cancellation, carried out before the commit.
	Cancel bool `json:"cancel,omitempty"`
	// AbandonVolumes is a move of a lost Machine: nothing is stopped or copied, and the target
	// makes the volumes new, the old ones staying on the abandoned host (ADR 0109 item 6).
	AbandonVolumes bool `json:"abandonVolumes,omitempty"`
	// Abort is the abort decided, with its reason. It is written, guarded on the record as read,
	// before any of the abort's host calls, and from then on the move only ends: no commit follows
	// it, and a new leader finishes it.
	Abort string `json:"abort,omitempty"`
	// TargetSilent is when the reserved target was first seen not answering, zero while it answers;
	// a move whose target stays silent for targetSilence is aborted.
	TargetSilent time.Time `json:"targetSilent,omitzero"`
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
	// EventMoveLost is a move whose target was abandoned after its commit: the Machine is lost
	// there, and the source's stopped copy is left as an older epoch.
	EventMoveLost = "machine.move-lost"
	// EventVolumesAbandoned is a lost Machine moved with new volumes, naming those left behind.
	EventVolumesAbandoned = "machine.volumes-abandoned"
	// EventDrainHeld is a draining host's Machine that fits on no other host: it stays, and the drain
	// of its host holds there. One per change.
	EventDrainHeld = "machine.drain-held"
)

// The limits of a drain (ADR 0110 item 7): a drain starts a move from a host with none in progress,
// and while fewer than drainFleet are in progress in the fleet.
const drainFleet = 2

// targetSilence bounds how long a move waits, before its commit, for a target that does not
// answer: the source is stopped meanwhile, and a drain's move has no operator watching it.
const targetSilence = 10 * time.Minute

// copyTimeout bounds one volume's export and import together. The hosts end a stream that stops
// moving; this is the backstop for one that moves too slowly to finish.
const copyTimeout = 6 * time.Hour

// ErrNotPlaced is a move of a Machine that is bound to no host.
var ErrNotPlaced = errors.New("the Machine is not placed on a host")

// ErrMoving is a move of a Machine with another move in progress, or a cancellation of one past its
// commit.
var ErrMoving = errors.New("the Machine has a move in progress")

// ErrNoMove is a cancellation of a Machine that has no move.
var ErrNoMove = errors.New("the Machine has no move in progress")

// ErrLost is a move of a lost Machine that does not abandon its volumes, and ErrNotLost one that
// abandons the volumes of a Machine that is not lost.
var (
	ErrLost    = errors.New("the Machine's host was abandoned")
	ErrNotLost = errors.New("only a lost Machine's volumes are abandoned")
)

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
	v := &orchapi.MachineMove{By: m.By, Requested: m.Requested, Shape: m.Shape, Step: string(m.Step), Source: m.Source.Host, Cancelled: m.Cancel || m.Abort != "", AbandonVolumes: m.AbandonVolumes, Reason: m.Reason}
	if m.Abort != "" {
		v.Reason = "aborting: " + m.Abort
	}
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
		if !p.hosts[id].Draining {
			// A host drained again later holds, and says so, afresh.
			r.held(id, "")
			continue
		}
		if from[id] || moving >= drainFleet {
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
		if m == nil || m.Step != MoveRequested || m.Cancel || e.Status.Phase == machineapi.PhaseDeleting || p.moving[e.ID] {
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
	alloc.Takes = delta.cpu > 0 || delta.memory > 0 || delta.disk > 0
	rev, err := r.cfg.Store.Allocate(ctx, next, e.Rev, host, alloc, ev)
	if err != nil {
		return fmt.Errorf("write %s/%s with %s's allocation: %w", e.Caller, e.Name, host, err)
	}
	e.Record, e.Rev = next, rev
	alloc.Rev, alloc.Takes = rev, false
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
		case m.Abort != "":
			return r.endAbort(ctx, p, e)
		case !m.Step.committed() && (m.Cancel || e.Status.Phase == machineapi.PhaseDeleting):
			return r.abort(ctx, p, e, abortReason(m, e))
		case !m.Step.committed() && !m.AbandonVolumes && p.abandoned(m.Source.Host):
			return r.abort(ctx, p, e, "the source "+m.Source.Host+" was abandoned")
		case !m.Step.committed() && m.Target != nil && p.abandoned(m.Target.Host):
			return r.abort(ctx, p, e, "the target "+m.Target.Host+" was abandoned")
		case m.Step.committed() && p.abandoned(m.Target.Host):
			return r.lostAtTarget(ctx, p, e)
		case ranAgain && !m.Step.committed():
			return r.abort(ctx, p, e, ReasonSourceRanAgain)
		case !m.Step.committed() && m.Target != nil && !answers(p, m.Target.Host):
			return r.targetSilent(ctx, p, e)
		case !m.Step.committed() && !m.TargetSilent.IsZero():
			err = r.setMove(ctx, e, func(_ *Record, m *Move) { m.TargetSilent = time.Time{} })
			if err != nil {
				return err
			}
			continue
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
				// The route changes in the write that rebinds the Machine (ADR 0112 item 6).
				rec.Move, rec.Binding, rec.PrivateAddress = &mv, mv.Target, mv.TargetAddress
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

// answers reports whether the host answered this pass.
func answers(p *pass, host string) bool {
	_, ok := p.answering(host)
	return ok
}

// targetSilent records when the target was first seen silent, and aborts the move once it has
// been silent for targetSilence.
func (r *Reconciler) targetSilent(ctx context.Context, p *pass, e *Entry) error {
	now := r.cfg.Clock.Now().UTC()
	m := e.Move
	if m.TargetSilent.IsZero() {
		return r.setMove(ctx, e, func(_ *Record, m *Move) {
			m.TargetSilent, m.Reason = now, "the target "+m.Target.Host+" does not answer; the move waits for it at most "+targetSilence.String()
		})
	}
	if now.Sub(m.TargetSilent) < targetSilence {
		return nil
	}
	return r.abort(ctx, p, e, "the target "+m.Target.Host+" did not answer for "+targetSilence.String())
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
	if e.Move.AbandonVolumes {
		// The source is abandoned: there is nothing to stop, and nothing is copied.
		return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step, m.Reason = MoveCopied, "" })
	}
	h, ok := p.answering(src.Host)
	if !ok {
		return r.waits(ctx, e, "the source "+src.Host+" does not answer; the move waits for its stop")
	}
	if _, made := h.microVM(src.MicroVM(e.ID)); !made && !h.Stale && !p.busy[src.Host] {
		// The source never made the Machine, as after a refused create: nothing is on its volumes,
		// so none is copied and the target makes them new. A fresh answer from a host with no work
		// of an earlier pass still running is the only one that shows it.
		return r.setMove(ctx, e, func(_ *Record, m *Move) { m.Step, m.Reason = MoveCopied, "" })
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
		d := &req.Disks[i]
		d.FromSnapshot, d.Imported = "", slices.ContainsFunc(e.Move.Volumes, func(v MovedVolume) bool { return v.Name == d.Name })
	}
	vm, err := r.cfg.Hosts.Create(ctx, t.Host, req)
	if err != nil {
		if refused(err) {
			he, _ := hostError(err)
			return r.abort(ctx, p, e, "the target refused the Machine: "+he.Message)
		}
		return fmt.Errorf("create %s on %s for its move: %w", req.Name, t.Host, err)
	}
	return r.setMove(ctx, e, func(_ *Record, m *Move) {
		m.Step, m.Reason, m.TargetAddress = MoveCreated, "", privateAddress(vm)
	})
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
	var vm hostapi.MicroVM
	if e.Spec.State != machineapi.StateStopped {
		var err error
		vm, err = r.cfg.Hosts.Start(ctx, t.Host, t.MicroVM(e.ID))
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
		if a := privateAddress(vm); a.IsValid() {
			rec.PrivateAddress = a
		}
		rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = phase, "", ""
		rec.Status.ObservedGeneration = rec.Status.Generation
	})
}

// cleanUp deletes the source's microVM and its volumes, whose verified copies the target holds,
// once no proxy forwards on a table that routes the Machine to the source, and then ends the move
// with the source's capacity freed. A Machine being deleted waits too: a table from before its
// delete still names the source's address.
func (r *Reconciler) cleanUp(ctx context.Context, p *pass, e *Entry) error {
	src := e.Move.Source
	if r.cfg.Routes != nil {
		if e.Move.Routed == nil {
			// e was read after the commit, so no table of its revision or later names the source.
			at := Unrouted{Rev: e.Rev, At: r.cfg.Clock.Now()}
			if err := r.setMove(ctx, e, func(_ *Record, m *Move) { m.Routed = &at }); err != nil {
				return err
			}
		}
		ok, err := r.cfg.Routes.Delivered(ctx, e.Move.Routed.Rev, e.Move.Routed.At)
		if err != nil {
			return fmt.Errorf("ask whether the proxies stopped routing %s/%s to %s: %w", e.Caller, e.Name, src.Host, err)
		}
		if !ok {
			return r.waits(ctx, e, "a proxy may still route the Machine to "+src.Host)
		}
	}
	// An abandoned source is reached no more, and a lost Machine's is not deleted even on a host
	// that answers again: what it holds stays there, listed as abandoned until an operator deletes
	// the entry.
	if !p.abandoned(src.Host) && !e.Move.AbandonVolumes {
		if _, ok := p.answering(src.Host); !ok {
			return r.waits(ctx, e, "the source "+src.Host+" does not answer; the move waits to delete the Machine's copy there")
		}
		if err := r.cfg.Hosts.Delete(ctx, src.Host, src.MicroVM(e.ID)); err != nil {
			return fmt.Errorf("delete %s on %s after its move: %w", src.MicroVM(e.ID), src.Host, err)
		}
	}
	m := *e.Move
	if err := r.allocate(ctx, p, e, src.Host, amounts{}.minus(need(e.Spec)), func(rec *Record) { rec.Move = nil }); err != nil {
		return err
	}
	r.fleet(ctx, EventMoved, e.Record, map[string]string{"from": src.Host, "to": e.Binding.Host, "by": m.By})
	if m.AbandonVolumes {
		volumes := make([]string, 0, len(e.Spec.Volumes))
		for _, v := range e.Spec.Volumes {
			volumes = append(volumes, v.Name)
		}
		r.fleet(ctx, EventVolumesAbandoned, e.Record, map[string]string{"host": src.Host, "epoch": strconv.FormatUint(src.Epoch, 10), "volumes": strings.Join(volumes, ",")})
	}
	return nil
}

// lostAtTarget ends a move whose target was abandoned after the commit: the Machine is lost there
// with the volumes it was copied to. The source's stopped copy holds the Machine's volumes as of
// its stop, so it is listed as a source copy and keeps its disk allocated until an operator deletes
// it; its CPUs and memory are freed.
func (r *Reconciler) lostAtTarget(ctx context.Context, p *pass, e *Entry) error {
	m := *e.Move
	t, src := *m.Target, m.Source
	// The move is past its commit as read, so no table of e's revision or later routes the Machine
	// to the source; the clean up's, when it recorded one, is earlier.
	unrouted := cmp.Or(m.Routed, &Unrouted{Rev: e.Rev, At: r.cfg.Clock.Now()})
	if err := r.allocate(ctx, p, e, src.Host, amounts{}.minus(need(e.Spec)).plus(diskOf(e.Spec)), func(rec *Record) {
		rec.Move = nil
		r.lose(rec, t)
		if !slices.ContainsFunc(rec.Abandoned, func(a AbandonedEpoch) bool {
			return a.role() == RoleSource && a.Host == src.Host && a.Epoch == src.Epoch
		}) {
			rec.Abandoned = append(slices.Clone(rec.Abandoned), AbandonedEpoch{Host: src.Host, Epoch: src.Epoch, Volumes: volumeNames(rec.Spec), At: r.cfg.Clock.Now().UTC(), Role: RoleSource, Unrouted: unrouted})
		}
	}); err != nil {
		return err
	}
	r.fleet(ctx, EventMoveLost, e.Record, map[string]string{
		"from": src.Host, "to": t.Host, "by": m.By, "step": string(m.Step), "sourceEpoch": strconv.FormatUint(src.Epoch, 10), "volumes": strings.Join(volumeNames(e.Spec), ","),
	})
	return nil
}

// volumeNames are the names of the spec's volumes, in its order.
func volumeNames(s machineapi.Spec) []string {
	out := make([]string, 0, len(s.Volumes))
	for _, v := range s.Volumes {
		out = append(out, v.Name)
	}
	return out
}

// abort ends a move before its commit: it deletes what the target holds of it, the copied volumes
// with the microVM, frees the target's capacity and starts the source again when the Machine is
// desired running. A target that does not answer keeps its never-started copy, which an older epoch
// leaves for the Machine's deletion (ADR 0109 item 7).
func (r *Reconciler) abort(ctx context.Context, p *pass, e *Entry, reason string) error {
	if e.Move.Step.committed() {
		return fmt.Errorf("the move of %s/%s is past its commit and is not aborted", e.Caller, e.Name)
	}
	// The decision is written first, guarded on the record as read: a pass that read it before an
	// earlier pass's write moved it on makes no host call.
	if err := r.setMove(ctx, e, func(_ *Record, m *Move) { m.Abort = reason }); err != nil {
		return err
	}
	return r.endAbort(ctx, p, e)
}

// endAbort carries out the abort the record holds.
func (r *Reconciler) endAbort(ctx context.Context, p *pass, e *Entry) error {
	m := *e.Move
	reason := m.Abort
	if t := m.Target; t != nil {
		if _, ok := p.answering(t.Host); ok {
			if err := r.cfg.Hosts.Delete(ctx, t.Host, t.MicroVM(e.ID)); err != nil {
				return fmt.Errorf("delete %s on %s for its move's abort: %w", t.MicroVM(e.ID), t.Host, err)
			}
		}
	}
	src := m.Source
	// A move of a lost Machine, by --abandon-volumes, leaves its source lost: it is not started
	// again, whatever its host answers.
	srcLost := p.abandoned(src.Host) || m.AbandonVolumes
	phase, why := e.Status.Phase, e.Status.Reason
	_, srcAnswers := p.answering(src.Host)
	srcAnswers = srcAnswers && !srcLost
	if !srcAnswers {
		// Nothing is inferred of a source that does not answer.
		phase, why = machineapi.PhaseUnknown, ReasonNotAnswering
	}
	if srcAnswers && e.Status.Phase != machineapi.PhaseDeleting && m.Step != MoveRequested {
		vm := hostapi.MicroVM{State: "stopped"}
		if e.Spec.State != machineapi.StateStopped {
			var err error
			if vm, err = r.cfg.Hosts.Start(ctx, src.Host, src.MicroVM(e.ID)); err != nil {
				return fmt.Errorf("start %s on %s for its move's abort: %w", src.MicroVM(e.ID), src.Host, err)
			}
		}
		phase, why = phaseOf(vm), ""
		if vm.State == vmCreated && e.Spec.State == machineapi.StateStopped {
			phase = machineapi.PhaseStopped
		}
	}
	change := func(rec *Record) {
		rec.Move = nil
		switch {
		case srcLost && rec.Status.Phase != machineapi.PhaseDeleting:
			r.lose(rec, src)
		case rec.Status.Phase == machineapi.PhaseMoving:
			rec.Status.Phase, rec.Status.Reason = phase, why
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
		lost := r.Status.Phase == machineapi.PhaseLost
		switch {
		case r.Status.Phase == machineapi.PhaseDeleting:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s", ErrDeleting, caller, name)
		case r.Move != nil && !r.Move.Cancel && r.Move.Abort == "" && sameShape(r.Move.Shape, req.Shape) && r.Move.AbandonVolumes == req.AbandonVolumes:
			return operatorView(r), nil
		case r.Move != nil:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s, %s by %s", ErrMoving, caller, name, r.Move.Step, r.Move.By)
		case r.Binding == nil:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s", ErrNotPlaced, caller, name)
		case lost && !req.AbandonVolumes:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s is lost with its host %s; move it with --abandon-volumes, which gives it new, empty volumes", ErrLost, caller, name, r.Binding.Host)
		case !lost && req.AbandonVolumes:
			return orchapi.Machine{}, fmt.Errorf("%w: %s/%s is not lost; its volumes are moved with it", ErrNotLost, caller, name)
		}
		r.Move = &Move{By: by, Requested: s.clock.Now().UTC(), Shape: req.Shape, Step: MoveRequested, Source: *r.Binding, AbandonVolumes: req.AbandonVolumes}
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
		case r.Move.Cancel || r.Move.Abort != "":
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
