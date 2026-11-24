// Package machines keeps what the Machine API writes: each caller's Machines and snapshots under
// the caller's name, and each caller's own event log of its Machines' status (ADR 0108 items 4 to
// 6, ADR 0109 item 2). Placing, starting and stopping a Machine on a host is the reconciler's.
package machines

import (
	"errors"

	"github.com/garamsh/naru/lib/machineapi"
)

// The failures a caller branches on, which the Machine API's transport translates to its codes.
var (
	// ErrNotFound is nothing of the caller's at the name: another caller's is answered so.
	ErrNotFound = errors.New("not found")
	// ErrConflict is a name that holds another spec, or a snapshot of another Machine.
	ErrConflict = errors.New("the name holds something else")
	// ErrDeleting is a name held by a Machine being deleted.
	ErrDeleting = errors.New("the Machine is being deleted")
	// ErrMachineNotFound is a body that names a Machine the caller has not.
	ErrMachineNotFound = errors.New("no such Machine of the caller's")
	// ErrSnapshotNotFound is a restore that names a snapshot the caller has not.
	ErrSnapshotNotFound = errors.New("no such snapshot of the caller's")
	// ErrSnapshotMismatch is a restore whose volumes are not the snapshot's by name and size.
	ErrSnapshotMismatch = errors.New("the volumes are not the snapshot's")
	// ErrCursorExpired is a cursor older than the caller's log keeps.
	ErrCursorExpired = errors.New("the cursor is older than the log keeps")
	// ErrBadCursor is a cursor the log never gave.
	ErrBadCursor = errors.New("not a cursor of the log")
)

// Record is a Machine as orch stores it, under /naru/orch/machines/<caller>/<name>: the spec as
// the caller gave it, env values and file contents included, and the status the API answers.
type Record struct {
	Caller string          `json:"caller"`
	Name   string          `json:"name"`
	ID     string          `json:"id"`
	Spec   machineapi.Spec `json:"spec"`
	// StopGraceSeconds is the grace the last stop asked for.
	StopGraceSeconds uint32            `json:"stopGraceSeconds,omitempty"`
	Status           machineapi.Status `json:"status"`
}

// Machine is the record as the API answers it, with no secret in it.
func (r Record) Machine() machineapi.Machine {
	return machineapi.Machine{Name: r.Name, ID: r.ID, Spec: r.Spec.Redact(), Status: r.Status}
}

// Snapshot is a snapshot as orch stores it, under /naru/orch/snapshots/<caller>/<name>, with its
// Machine's spec unredacted.
type Snapshot struct {
	Caller   string              `json:"caller"`
	Snapshot machineapi.Snapshot `json:"snapshot"`
}

// Answer is the snapshot as the API answers it, with no secret in it.
func (s Snapshot) Answer() machineapi.Snapshot {
	a := s.Snapshot
	a.Spec = a.Spec.Redact()
	return a
}
