package machines

import (
	"context"
	"errors"

	"github.com/garamsh/naru/lib/machineapi"
)

// ErrRaced is a write's refusal when a record it depends on is taken or changed since it was read.
// The writer reads again.
var ErrRaced = errors.New("a record the write depends on changed since it was read")

// Store keeps the records and each caller's event log. A revision is etcd's: an event's cursor is
// the revision that committed it, so a cursor only grows, whichever member wrote it.
type Store interface {
	// Machine returns the caller's Machine and the revision of its last write; ErrNotFound when
	// there is none.
	Machine(ctx context.Context, caller, name string) (Record, int64, error)
	// Machines returns the caller's Machines, ordered by name, and the revision the read is current
	// at, which is the cursor of the caller's log the list is current at.
	Machines(ctx context.Context, caller string) ([]Record, int64, error)
	// All returns every caller's Machines, ordered by caller and name.
	All(ctx context.Context) ([]Record, error)
	// Create writes r and appends ev to its caller's log in one transaction, while no Machine of the
	// caller's has the name, none of any caller's has the id, and, when r restores from a snapshot,
	// that snapshot is still the one written at from; ErrRaced otherwise.
	Create(ctx context.Context, r Record, from int64, ev machineapi.Event) error
	// Update writes r and appends ev in one transaction, while the record is still the one written
	// at rev; ErrRaced otherwise.
	Update(ctx context.Context, r Record, rev int64, ev machineapi.Event) error
	// Snapshot returns the caller's snapshot and the revision of its write; ErrNotFound when there
	// is none.
	Snapshot(ctx context.Context, caller, name string) (Snapshot, int64, error)
	// Snapshots returns the caller's snapshots, ordered by name.
	Snapshots(ctx context.Context, caller string) ([]Snapshot, error)
	// CreateSnapshot writes s while the caller has no snapshot of its name and the snapshot's Machine
	// is still the one written at machine; ErrRaced otherwise.
	CreateSnapshot(ctx context.Context, s Snapshot, machine int64) error
	// DeleteSnapshot removes the caller's snapshot; ErrNotFound when there is none.
	DeleteSnapshot(ctx context.Context, caller, name string) error
	// Events returns the caller's events committed after the revision after, oldest first, and the
	// revision the read is current at. A nil after is the whole log. A revision above the current one
	// is ErrBadCursor; one older than the log keeps is ErrCursorExpired.
	Events(ctx context.Context, caller string, after *int64) ([]machineapi.Event, int64, error)
	// ClaimZone records zone as the zone of the cluster's stable names when none is recorded, and
	// returns the recorded one. An empty zone claims nothing and returns what is recorded, if any.
	ClaimZone(ctx context.Context, zone string) (string, error)
}
