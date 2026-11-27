// Package machines keeps what the Machine API writes: each caller's Machines and snapshots under
// the caller's name, and each caller's own event log of its Machines' status (ADR 0108 items 4 to
// 6, ADR 0109 item 2). Its reconciler, on the leader, places each Machine on a host by orch's own
// account of capacity and makes the host run it as the record asks (ADR 0109 items 1, 4 and 7, ADR
// 0110).
package machines

import (
	"errors"
	"fmt"
	"slices"
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
	// ErrNotRunning is an exec or a file call to a Machine that does not run.
	ErrNotRunning = errors.New("the Machine is not running")
	// ErrFileNotFound is a file call to a running Machine whose guest has no file at the path.
	ErrFileNotFound = errors.New("the guest has no such file")
	// ErrTooLarge is an exec's output or a guest file above machineapi.MaxGuestBytes.
	ErrTooLarge = errors.New("larger than the most a guest call carries")
	// ErrHostUnavailable is a guest call the Machine's host did not answer.
	ErrHostUnavailable = errors.New("the Machine's host did not answer")
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
	// Refused is the generation whose create the host refused; the reconciler makes no create while
	// it is the Machine's generation.
	Refused uint64 `json:"refused,omitempty"`
	// Move is the move in progress, nil when there is none (ADR 0109 item 5).
	Move *Move `json:"move,omitempty"`
	// Epoch is the highest epoch a placement or a move of the Machine took, so the next is above it
	// although the host that holds it does not answer.
	Epoch uint64 `json:"epoch,omitempty"`
	// Abandoned are the epochs of the Machine whose volumes are left on an abandoned host, kept until
	// an operator deletes them (ADR 0109 item 7).
	Abandoned []AbandonedEpoch `json:"abandoned,omitempty"`
}

// AbandonedEpoch is a copy of the Machine's volumes left behind, at an epoch on a host: the
// placement on a host that was abandoned (RoleBound), or a move's source copy, left stopped when
// the move's target was abandoned after the commit (RoleSource).
type AbandonedEpoch struct {
	Host    string    `json:"host"`
	Epoch   uint64    `json:"epoch"`
	Volumes []string  `json:"volumes"`
	At      time.Time `json:"at"`
	Role    string    `json:"role,omitempty"`
	// Deleting is an operator's delete of a source copy, which the reconciler carries out on its
	// host before it forgets the entry and frees the disk it holds.
	Deleting bool `json:"deleting,omitempty"`
}

// The roles of an AbandonedEpoch. A RoleSource copy is on a host that answers: its microVM is
// counted through the disk its host's allocation keeps for it.
const (
	RoleBound  = "bound"
	RoleSource = "source"
)

// role is the entry's role, RoleBound for an entry recorded without one.
func (a AbandonedEpoch) role() string {
	if a.Role == "" {
		return RoleBound
	}
	return a.Role
}

// lost reports whether the Machine is lost: its phase is, or its current binding is a placement
// listed as abandoned. It is read from the record alone, so it holds after the host's abandonment
// mark is gone, as when the host is removed and admitted again under its ID (ADR 0109 item 6).
func (r Record) lost() bool {
	if r.Status.Phase == machineapi.PhaseLost {
		return true
	}
	return r.Binding != nil && slices.ContainsFunc(r.Abandoned, func(a AbandonedEpoch) bool {
		return a.role() == RoleBound && a.Host == r.Binding.Host && a.Epoch == r.Binding.Epoch
	})
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
	// Refusals counts the host's refusals of its disk snapshots; at MaxSnapshotRefusals it fails.
	Refusals int `json:"refusals,omitempty"`
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

// Deletion is a deleted or failed snapshot's disk snapshots on its host, under
// /naru/orch/snapshot-deletions/<id>. Until Done the reconciler deletes them; after, the record is
// the tombstone that lets it delete one a host still holds, until it is TombstoneAge old.
type Deletion struct {
	ID    string    `json:"id"`
	Host  string    `json:"host"`
	Disks []string  `json:"disks"`
	Done  time.Time `json:"done,omitzero"`
}

// Tombstone is a removed Machine, under /naru/orch/machine-tombstones/<id>: the reconciler deletes a
// microVM of an id it names, and of no other id without a record (ADR 0109 item 7), until it is
// TombstoneAge old.
type Tombstone struct {
	ID      string    `json:"id"`
	Removed time.Time `json:"removed"`
}

// TombstoneAge is how long a tombstone is kept: a microVM a late create left behind is found on the
// first pass its host answers, and one whose host stays away longer is left, as unknown.
const TombstoneAge = 7 * 24 * time.Hour

// Answer is the snapshot as the API answers it, with no secret in it.
func (s Snapshot) Answer() machineapi.Snapshot {
	a := s.Snapshot
	a.Spec = a.Spec.Redact()
	return a
}
