// Package machines keeps what the Machine API writes: each caller's Machines and snapshots under
// the caller's name, and each caller's own event log of its Machines' status (ADR 0108 items 4 to
// 6, ADR 0109 item 2). Its reconciler, on the leader, places each Machine on a host by orch's own
// account of capacity and makes the host run it as the record asks (ADR 0109 items 1, 4 and 7, ADR
// 0110).
package machines

import (
	"errors"
	"fmt"
	"time"

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
	// Binding is the host the leader placed the Machine on; nil until it is placed.
	Binding *Binding `json:"binding,omitempty"`
	// OperatorReason is Status.Reason with the fleet's figures, which only the operator API answers.
	OperatorReason string `json:"operatorReason,omitempty"`
	// Restarted is when the reconciler last started the Machine again after it stopped on its own.
	Restarted time.Time `json:"restarted,omitzero"`
}

// Binding is a placement of a Machine: its host, and the epoch that counts its placements.
type Binding struct {
	Host  string `json:"host"`
	Epoch uint64 `json:"epoch"`
}

// MicroVMPrefix begins the name of every microVM the reconciler makes; a microVM without it is
// outside the reconciler's reach (ADR 0109 item 4).
const MicroVMPrefix = "m-"

// MicroVM is the name of the Machine id's microVM on its host under the epoch: m-<id>-<epoch>,
// which names no caller (ADR 0109 item 3).
func (b Binding) MicroVM(id string) string { return fmt.Sprintf("%s%s-%d", MicroVMPrefix, id, b.Epoch) }

// Machine is the record as the API answers it, with no secret in it.
func (r Record) Machine() machineapi.Machine {
	return machineapi.Machine{Name: r.Name, ID: r.ID, Spec: r.Spec.Redact(), Status: r.Status}
}

// Snapshot is a snapshot as orch stores it, under /naru/orch/snapshots/<caller>/<name>, with its
// Machine's spec unredacted. ID names its disk snapshots on Host, one per volume, which the
// reconciler sets before it takes them.
type Snapshot struct {
	Caller   string              `json:"caller"`
	Snapshot machineapi.Snapshot `json:"snapshot"`
	ID       string              `json:"id"`
	Host     string              `json:"host,omitempty"`
}

// DiskSnapshot is the name of the disk snapshot that holds the snapshot's i-th volume on its host:
// s-<id>-<i>, a DNS label whatever the volume's name.
func (s Snapshot) DiskSnapshot(i int) string { return fmt.Sprintf("s-%s-%d", s.ID, i) }

// DiskSnapshots are the names of all the snapshot's disk snapshots, in the order of its volumes.
func (s Snapshot) DiskSnapshots() []string {
	out := make([]string, len(s.Snapshot.Spec.Volumes))
	for i := range out {
		out[i] = s.DiskSnapshot(i)
	}
	return out
}

// Deletion is a deleted snapshot whose disk snapshots the reconciler still deletes on its host,
// under /naru/orch/snapshot-deletions/<id>.
type Deletion struct {
	ID    string   `json:"id"`
	Host  string   `json:"host"`
	Disks []string `json:"disks"`
}

// Answer is the snapshot as the API answers it, with no secret in it.
func (s Snapshot) Answer() machineapi.Snapshot {
	a := s.Snapshot
	a.Spec = a.Spec.Redact()
	return a
}
