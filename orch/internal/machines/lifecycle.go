package machines

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

// newID is a Machine's id: 16 random lowercase hexadecimal digits.
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// capabilities are what v1alpha1's implementation offers before the Machine is placed: volume
// snapshots and cold moves always, and the GPUs the shape asks for. Resize comes from the host's
// Driver once the Machine is bound (ADR 0108 item 8).
func capabilities(s machineapi.Spec) machineapi.Capabilities {
	c := machineapi.Capabilities{Snapshot: []machineapi.SnapshotKind{machineapi.SnapshotVolumes}, Move: []machineapi.MoveKind{machineapi.MoveCold}}
	if g := s.Shape.GPU; g != nil {
		gpu := *g
		c.GPU = &gpu
	}
	return c
}

func (s *service) Create(ctx context.Context, caller, name string, spec machineapi.Spec) (machineapi.Machine, bool, error) {
	if err := machineapi.CheckName(name); err != nil {
		return machineapi.Machine{}, false, &machineapi.FieldError{Field: "name", Problem: "is not a DNS label"}
	}
	if err := spec.Check(); err != nil {
		return machineapi.Machine{}, false, err
	}
	if spec.State == "" {
		spec.State = machineapi.StateRunning
	}
	for range retries {
		have, _, err := s.store.Machine(ctx, caller, name)
		switch {
		case err == nil:
			return same(have, spec)
		case !errors.Is(err, ErrNotFound):
			return machineapi.Machine{}, false, err
		}
		from, err := s.checkRestore(ctx, caller, spec)
		if err != nil {
			return machineapi.Machine{}, false, err
		}
		id := newID()
		r := Record{Caller: caller, Name: name, ID: id, Spec: spec, Status: machineapi.Status{
			Phase: machineapi.PhasePending, Address: machineapi.Address{Name: machineapi.StableName(id, "", s.zone)}, Generation: 1, Capabilities: capabilities(spec),
		}}
		err = s.store.Create(ctx, r, from, s.event(machineapi.EventStatus, r))
		if err == nil {
			return r.Machine(), true, nil
		}
		if !errors.Is(err, ErrRaced) {
			return machineapi.Machine{}, false, err
		}
		// The name was created meanwhile, the id is another Machine's, or the snapshot changed: read
		// again.
	}
	return machineapi.Machine{}, false, fmt.Errorf("create %s/%s: %d writes raced", caller, name, retries)
}

// same answers a create of a name the caller holds: the Machine when it has the spec, State aside.
func same(have Record, spec machineapi.Spec) (machineapi.Machine, bool, error) {
	switch {
	case have.Status.Phase == machineapi.PhaseDeleting:
		return machineapi.Machine{}, false, fmt.Errorf("%w: the Machine %s is being deleted, and its name is held until its volumes are gone", ErrDeleting, have.Name)
	case !machineapi.SameSpec(have.Spec, spec):
		return machineapi.Machine{}, false, fmt.Errorf("%w: the Machine %s exists with another spec", ErrConflict, have.Name)
	}
	return have.Machine(), false, nil
}

// checkRestore refuses a restore from a snapshot the caller has not, or whose volumes are not the
// spec's by name and size. It returns the revision of the snapshot it checked, which the create
// guards on, 0 when the spec restores nothing.
func (s *service) checkRestore(ctx context.Context, caller string, spec machineapi.Spec) (int64, error) {
	src := spec.FromSnapshot
	if src == nil {
		return 0, nil
	}
	snap, rev, err := s.store.Snapshot(ctx, caller, src.Snapshot)
	if errors.Is(err, ErrNotFound) {
		return 0, fmt.Errorf("%w: %s", ErrSnapshotNotFound, src.Snapshot)
	}
	if err != nil {
		return 0, err
	}
	if err := machineapi.MatchVolumes(snap.Snapshot.Spec.Volumes, spec.Volumes); err != nil {
		return 0, fmt.Errorf("%w: %v", ErrSnapshotMismatch, err)
	}
	return rev, nil
}

func (s *service) Machine(ctx context.Context, caller, name string) (machineapi.Machine, error) {
	r, _, err := s.store.Machine(ctx, caller, name)
	if err != nil {
		return machineapi.Machine{}, notFound(err, "Machine", name)
	}
	return r.Machine(), nil
}

func (s *service) Machines(ctx context.Context, caller string) (machineapi.MachineList, error) {
	rs, rev, err := s.store.Machines(ctx, caller)
	if err != nil {
		return machineapi.MachineList{}, err
	}
	l := machineapi.MachineList{Machines: make([]machineapi.Machine, 0, len(rs)), Cursor: strconv.FormatInt(rev, 10)}
	for _, r := range rs {
		l.Machines = append(l.Machines, r.Machine())
	}
	return l, nil
}

func (s *service) Start(ctx context.Context, caller, name string) (machineapi.Machine, error) {
	return s.change(ctx, caller, name, func(r *Record) bool {
		if r.Spec.State == machineapi.StateRunning {
			return false
		}
		r.Spec.State = machineapi.StateRunning
		return true
	})
}

func (s *service) Stop(ctx context.Context, caller, name string, req machineapi.StopRequest) (machineapi.Machine, error) {
	if err := req.Check(); err != nil {
		return machineapi.Machine{}, err
	}
	return s.change(ctx, caller, name, func(r *Record) bool {
		if r.Spec.State == machineapi.StateStopped {
			return false
		}
		r.Spec.State, r.StopGraceSeconds = machineapi.StateStopped, req.GraceSeconds
		return true
	})
}

// change applies a change of the desired state to the caller's Machine, as a new generation, unless
// change reports there is none.
func (s *service) change(ctx context.Context, caller, name string, change func(r *Record) bool) (machineapi.Machine, error) {
	for range retries {
		r, rev, err := s.store.Machine(ctx, caller, name)
		if err != nil {
			return machineapi.Machine{}, notFound(err, "Machine", name)
		}
		if r.Status.Phase == machineapi.PhaseDeleting {
			return machineapi.Machine{}, fmt.Errorf("%w: %s", ErrDeleting, name)
		}
		if !change(&r) {
			return r.Machine(), nil
		}
		r.Status.Generation++
		// A placed Machine shows the state it heads for at once, in the event this change appends;
		// one that waits for a host stays pending, and one whose host does not answer stays unknown.
		if r.Binding != nil && r.Status.Phase != machineapi.PhaseUnknown && r.Status.Phase != machineapi.PhaseLost {
			r.Status.Phase, r.Status.Reason = machineapi.PhaseStarting, ""
			if r.Spec.State == machineapi.StateStopped {
				r.Status.Phase = machineapi.PhaseStopping
			}
		}
		ev := s.event(machineapi.EventStatus, r)
		_, err = s.store.Update(ctx, r, rev, &ev)
		if err == nil {
			return r.Machine(), nil
		}
		if !errors.Is(err, ErrRaced) {
			return machineapi.Machine{}, err
		}
	}
	return machineapi.Machine{}, fmt.Errorf("change %s/%s: %d writes raced", caller, name, retries)
}

// Delete marks the Machine deleting and removes its aliases in the same write, so no table derived
// after it routes to the Machine (ADR 0112 item 3).
func (s *service) Delete(ctx context.Context, caller, name string) (machineapi.Machine, error) {
	for range retries {
		r, rev, err := s.store.Machine(ctx, caller, name)
		if err != nil {
			return machineapi.Machine{}, notFound(err, "Machine", name)
		}
		if r.Status.Phase == machineapi.PhaseDeleting {
			return r.Machine(), nil
		}
		all, err := s.store.Aliases(ctx)
		if err != nil {
			return machineapi.Machine{}, err
		}
		aliases := slices.DeleteFunc(all, func(a AliasEntry) bool { return a.MachineID != r.ID })
		r.Status.Phase = machineapi.PhaseDeleting
		ev := s.event(machineapi.EventStatus, r)
		err = s.store.MarkDeleting(ctx, r, rev, aliases, ev)
		if err == nil {
			return r.Machine(), nil
		}
		if !errors.Is(err, ErrRaced) {
			return machineapi.Machine{}, err
		}
	}
	return machineapi.Machine{}, fmt.Errorf("delete %s/%s: %d writes raced", caller, name, retries)
}

func (s *service) All(ctx context.Context) ([]orchapi.Machine, error) {
	rs, err := s.store.All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Machine, 0, len(rs))
	for _, r := range rs {
		out = append(out, operatorView(r.Record))
	}
	return out, nil
}

// notFound names what was not found, and passes any other failure on.
func notFound(err error, what, name string) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: no %s %s", ErrNotFound, what, name)
	}
	return err
}
