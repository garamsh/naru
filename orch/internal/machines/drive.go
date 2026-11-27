package machines

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
)

// The states the host API reports of a microVM.
const (
	vmCreated = "created"
	vmRunning = "running"
	vmPaused  = "paused"
	vmFaulted = "faulted"
)

// drive brings a Machine bound to an answering host to its record there.
func (r *Reconciler) drive(ctx context.Context, p *pass, e *Entry) error {
	if e.Status.Phase == machineapi.PhaseDeleting {
		return r.finish(ctx, p, e)
	}
	h := p.hosts[e.Binding.Host]
	vm, ok := h.microVM(e.Binding.MicroVM(e.ID))
	switch {
	case ok:
		return r.converge(ctx, e, vm)
	case e.Refused == e.Status.Generation:
		// The host refused this generation's create, and refuses it again: the next generation is
		// created.
		return nil
	case h.Stale:
		// A microVM made since the host last answered is missing from this answer.
		return nil
	}
	return r.create(ctx, p, e)
}

// finish removes a deleted Machine: its microVM and disks first, when it was placed, then its
// record, freeing its name and its host's allocation, with its deleted event and tombstone.
func (r *Reconciler) finish(ctx context.Context, p *pass, e *Entry) error {
	// A lost Machine's host is not called, whatever it answers: what it holds there is the
	// abandoned volumes', which its tombstone has a host that answers delete.
	if e.Binding != nil && !p.abandoned(e.Binding.Host) && !e.lost() {
		name := e.Binding.MicroVM(e.ID)
		if err := r.cfg.Hosts.Delete(ctx, e.Binding.Host, name); err != nil {
			return fmt.Errorf("delete %s on %s: %w", name, e.Binding.Host, err)
		}
	}
	// A source copy's disk is freed with the record that kept it; the tombstone has its host delete
	// the copy.
	for _, a := range slices.Clone(e.Abandoned) {
		if a.role() == RoleSource {
			if err := r.forgetCopy(ctx, p, e, a); err != nil {
				return err
			}
		}
	}
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

// create makes the Machine's microVM on its host: started when it is desired running, and left
// created, never booted, when it is desired stopped.
func (r *Reconciler) create(ctx context.Context, p *pass, e *Entry) error {
	b := *e.Binding
	req, err := r.request(p, e)
	if err != nil {
		return r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseFailed, err.Error(), err.Error()
		})
	}
	req.NoStart = e.Spec.State == machineapi.StateStopped
	if !req.NoStart {
		if err := r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseStarting, "", ""
		}); err != nil {
			return err
		}
	}
	vm, err := r.cfg.Hosts.Create(ctx, b.Host, req)
	if refused(err) {
		he, _ := hostError(err)
		reason := "the host refused the Machine: " + he.Message
		return r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseFailed, reason, reason
			rec.Status.ObservedGeneration, rec.Refused = rec.Status.Generation, rec.Status.Generation
		})
	}
	if err != nil {
		return fmt.Errorf("create %s on %s: %w", req.Name, b.Host, err)
	}
	return r.converge(ctx, e, vm)
}

// refused is a host's answer that the same request will meet again: a 4xx other than a conflict,
// a missing name, a rate limit, or a refusal of orch's own credential, which a later call may pass.
func refused(err error) bool {
	he, ok := hostError(err)
	if !ok || he.Status < 400 || he.Status >= 500 {
		return false
	}
	switch he.Status {
	case http.StatusConflict, http.StatusNotFound, http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden:
		return false
	}
	return true
}

// converge starts or stops the microVM to the desired state, and records the phase it reached.
// A desired state the reconciler has not yet acted on is acted on at once; a Machine that stopped
// on its own after that is started again at most once per RestartEvery.
func (r *Reconciler) converge(ctx context.Context, e *Entry, vm hostapi.MicroVM) error {
	b := *e.Binding
	name := b.MicroVM(e.ID)
	running := vm.State == vmRunning || vm.State == vmPaused
	// restart is a start of a Machine that ran under this generation and stopped on its own.
	restart := e.Status.ObservedGeneration == e.Status.Generation && vm.State != vmCreated
	var err error
	switch {
	case e.Spec.State == machineapi.StateStopped && running:
		if err := r.set(ctx, e, func(rec *Record) {
			rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = machineapi.PhaseStopping, "", ""
			rec.Status.ObservedGeneration = rec.Status.Generation
		}); err != nil {
			return err
		}
		// A grace stored before the Machine API bounded it is the most a host's stop takes.
		vm, err = r.cfg.Hosts.Stop(ctx, b.Host, name, min(e.StopGraceSeconds, hostapi.MaxGraceSeconds))
	case e.Spec.State != machineapi.StateStopped && !running:
		now := r.cfg.Clock.Now()
		if restart && now.Sub(e.Restarted) < RestartEvery {
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
			if restart {
				rec.Restarted = now
			}
			rec.Status.ObservedGeneration = rec.Status.Generation
		}); err != nil {
			return err
		}
		if restart {
			r.fleet(ctx, EventRestart, e.Record, map[string]string{"host": b.Host, "microvm": name, "state": vm.State, "failure": vm.Failure})
		}
		vm, err = r.cfg.Hosts.Start(ctx, b.Host, name)
	}
	if err != nil {
		return fmt.Errorf("%s on %s: %w", name, b.Host, err)
	}
	phase := phaseOf(vm)
	if vm.State == vmCreated && e.Spec.State == machineapi.StateStopped {
		// Made and never started, as a Machine desired stopped is.
		phase = machineapi.PhaseStopped
	}
	return r.set(ctx, e, func(rec *Record) {
		rec.Status.Phase, rec.Status.Reason, rec.OperatorReason = phase, "", ""
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
