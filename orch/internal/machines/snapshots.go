package machines

import (
	"context"
	"errors"
	"fmt"

	"github.com/garamsh/naru/lib/machineapi"
)

// PutSnapshot records the snapshot pending; the reconciler takes it on the Machine's host.
func (s *service) PutSnapshot(ctx context.Context, caller, name string, req machineapi.SnapshotRequest) (machineapi.Snapshot, bool, error) {
	if err := machineapi.CheckName(name); err != nil {
		return machineapi.Snapshot{}, false, &machineapi.FieldError{Field: "name", Problem: "is not a DNS label"}
	}
	for range retries {
		m, rev, err := s.store.Machine(ctx, caller, req.Machine)
		if errors.Is(err, ErrNotFound) {
			return machineapi.Snapshot{}, false, fmt.Errorf("%w: %s", ErrMachineNotFound, req.Machine)
		}
		if err != nil {
			return machineapi.Snapshot{}, false, err
		}
		have, _, err := s.store.Snapshot(ctx, caller, name)
		switch {
		case err == nil && have.Snapshot.MachineID != m.ID:
			return machineapi.Snapshot{}, false, fmt.Errorf("%w: the snapshot %s is of another Machine", ErrConflict, name)
		case err == nil:
			return have.Answer(), false, nil
		case !errors.Is(err, ErrNotFound):
			return machineapi.Snapshot{}, false, err
		}
		if m.Status.Phase == machineapi.PhaseDeleting {
			return machineapi.Snapshot{}, false, fmt.Errorf("%w: %s", ErrDeleting, req.Machine)
		}
		snap := Snapshot{Caller: caller, Snapshot: machineapi.Snapshot{
			Name: name, Machine: m.Name, MachineID: m.ID, Spec: m.Spec, Phase: machineapi.SnapshotPending, CreatedAt: s.clock.Now().UTC(),
		}}
		// The write is guarded on the Machine as read, so one marked deleting meanwhile is not snapshot.
		err = s.store.CreateSnapshot(ctx, snap, rev)
		if err == nil {
			return snap.Answer(), true, nil
		}
		if !errors.Is(err, ErrRaced) {
			return machineapi.Snapshot{}, false, err
		}
	}
	return machineapi.Snapshot{}, false, fmt.Errorf("snapshot %s/%s: %d writes raced", caller, name, retries)
}

func (s *service) Snapshot(ctx context.Context, caller, name string) (machineapi.Snapshot, error) {
	snap, _, err := s.store.Snapshot(ctx, caller, name)
	if err != nil {
		return machineapi.Snapshot{}, notFound(err, "snapshot", name)
	}
	return snap.Answer(), nil
}

func (s *service) Snapshots(ctx context.Context, caller string) ([]machineapi.Snapshot, error) {
	snaps, err := s.store.Snapshots(ctx, caller)
	if err != nil {
		return nil, err
	}
	out := make([]machineapi.Snapshot, 0, len(snaps))
	for _, snap := range snaps {
		out = append(out, snap.Answer())
	}
	return out, nil
}

func (s *service) DeleteSnapshot(ctx context.Context, caller, name string) error {
	return notFound(s.store.DeleteSnapshot(ctx, caller, name), "snapshot", name)
}
