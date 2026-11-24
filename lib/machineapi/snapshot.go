package machineapi

import (
	"encoding/json"
	"time"
)

// SnapshotRequest is the body of PUT .../snapshots/{snapshot}: the Machine whose volumes it takes.
type SnapshotRequest struct {
	Machine string `json:"machine"`
}

// SnapshotPhase is where a snapshot is in being taken.
type SnapshotPhase string

// The snapshot phases. A client reads a phase it does not know as SnapshotUnknown.
const (
	SnapshotPending SnapshotPhase = "pending"
	SnapshotReady   SnapshotPhase = "ready"
	SnapshotFailed  SnapshotPhase = "failed"
	SnapshotUnknown SnapshotPhase = "unknown"
)

// UnmarshalJSON reads a phase this version does not name as SnapshotUnknown.
func (p *SnapshotPhase) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch v := SnapshotPhase(s); v {
	case SnapshotPending, SnapshotReady, SnapshotFailed:
		*p = v
	default:
		*p = SnapshotUnknown
	}
	return nil
}

// Snapshot is a snapshot of one Machine's volumes, with its spec as a read answers it. It holds no
// memory: it is consistent when taken stopped and crash-consistent when taken running. It
// outlives its Machine until the caller deletes it.
type Snapshot struct {
	Name    string `json:"name"`
	Machine string `json:"machine"`
	// MachineID is the id of the Machine it was taken of, which a later Machine of the same name
	// does not have.
	MachineID string        `json:"machineId"`
	Spec      Spec          `json:"spec"`
	Phase     SnapshotPhase `json:"phase"`
	Reason    string        `json:"reason,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

// SnapshotList is the body of GET .../snapshots, ordered by name.
type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}
