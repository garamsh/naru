package machines

import (
	"context"
	"errors"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
)

// Entry is a stored Machine and the revision of its last write.
type Entry struct {
	Record
	Rev int64
}

// SnapshotEntry is a stored snapshot and the revision of its last write.
type SnapshotEntry struct {
	Snapshot
	Rev int64
}

// Allocation is what orch has given away on one host, under /naru/orch/host-allocations/<host>: the
// sum of the resources of the Machines bound there, volumes included (ADR 0110 item 2), and the
// revision it was read at, 0 when the host has none.
type Allocation struct {
	hostapi.Resources
	Rev int64
}

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
	All(ctx context.Context) ([]Entry, error)
	// Create writes r and appends ev to its caller's log in one transaction, while no Machine of the
	// caller's has the name, none of any caller's has the id, and, when r restores from a snapshot,
	// that snapshot is still the one written at from; ErrRaced otherwise.
	Create(ctx context.Context, r Record, from int64, ev machineapi.Event) error
	// Update writes r, and appends ev when it is not nil, in one transaction, while the record is
	// still the one written at rev; ErrRaced otherwise. It returns the revision it wrote at.
	Update(ctx context.Context, r Record, rev int64, ev *machineapi.Event) (int64, error)
	// Snapshot returns the caller's snapshot and the revision of its write; ErrNotFound when there
	// is none.
	Snapshot(ctx context.Context, caller, name string) (Snapshot, int64, error)
	// AllSnapshots returns every caller's snapshots.
	AllSnapshots(ctx context.Context) ([]SnapshotEntry, error)
	// UpdateSnapshot writes s, and del when it is not nil, while s is still the one written at rev,
	// and returns the revision it wrote at; ErrRaced otherwise.
	UpdateSnapshot(ctx context.Context, s Snapshot, rev int64, del *Deletion) (int64, error)
	// Snapshots returns the caller's snapshots, ordered by name.
	Snapshots(ctx context.Context, caller string) ([]Snapshot, error)
	// CreateSnapshot writes s while the caller has no snapshot of its name and the snapshot's Machine
	// is still the one written at machine; ErrRaced otherwise.
	CreateSnapshot(ctx context.Context, s Snapshot, machine int64) error
	// DeleteSnapshot removes s, while it is still the one written at rev, and records its Deletion
	// when it names a host; ErrRaced otherwise.
	DeleteSnapshot(ctx context.Context, s Snapshot, rev int64) error
	// Deletions returns every snapshot deletion, done or not.
	Deletions(ctx context.Context) ([]Deletion, error)
	// PutDeletion writes d, as done once its disk snapshots are gone.
	PutDeletion(ctx context.Context, d Deletion) error
	// DropDeletion removes a deletion, once it is done and TombstoneAge old.
	DropDeletion(ctx context.Context, id string) error
	// Tombstones returns every removed Machine's tombstone by id.
	Tombstones(ctx context.Context) (map[string]Tombstone, error)
	// DropTombstone removes a tombstone once it is TombstoneAge old.
	DropTombstone(ctx context.Context, id string) error
	// Allocations returns each host's allocation that has one.
	Allocations(ctx context.Context) (map[string]Allocation, error)
	// Bind writes r, which names its binding, with its host's allocation of a new sum, while r is
	// still the one written at rev and the allocation the one read; ErrRaced otherwise. So two
	// placements never both take the last of a host.
	// It returns the revision it wrote at, the record's and the allocation's.
	Bind(ctx context.Context, r Record, rev int64, alloc Allocation) (int64, error)
	// Remove deletes r and frees its id and name, appending ev, writes its tombstone, dated by ev,
	// and writes its host's allocation's new sum when alloc is not nil, while r and the allocation
	// are still the ones read; ErrRaced otherwise.
	// It returns the revision it wrote at, the allocation's.
	Remove(ctx context.Context, r Record, rev int64, alloc *Allocation, ev machineapi.Event) (int64, error)
	// Events returns the caller's events committed after the revision after, oldest first, and the
	// revision the read is current at. A nil after is the whole log. A revision above the current one
	// is ErrBadCursor; one older than the log keeps is ErrCursorExpired.
	Events(ctx context.Context, caller string, after *int64) ([]machineapi.Event, int64, error)
	// ClaimZone records zone as the zone of the cluster's stable names when none is recorded, and
	// returns the recorded one. An empty zone claims nothing and returns what is recorded, if any.
	ClaimZone(ctx context.Context, zone string) (string, error)
}
