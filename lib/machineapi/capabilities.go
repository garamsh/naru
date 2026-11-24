package machineapi

import (
	"encoding/json"
	"slices"
)

// SnapshotKind is what a snapshot of the Machine can hold.
type SnapshotKind string

// The snapshot kinds. v1alpha1's implementation offers SnapshotVolumes only.
const (
	SnapshotVolumes SnapshotKind = "volumes"
	SnapshotMemory  SnapshotKind = "memory"
)

// MoveKind is a kind of move the Machine takes.
type MoveKind string

// The move kinds. v1alpha1's implementation offers MoveCold only: stop, place and start.
const (
	MoveCold     MoveKind = "cold"
	MoveSnapshot MoveKind = "snapshot"
	MoveLive     MoveKind = "live"
)

// ResizeKind is a figure of the Machine its host's Driver can change while it runs.
type ResizeKind string

// The resize kinds.
const (
	ResizeCPU    ResizeKind = "cpu"
	ResizeMemory ResizeKind = "memory"
)

// Capabilities are what the Machine's host and form allow, which orch computes. A call they
// exclude is 409 naming the capability (ADR 0108 item 8).
type Capabilities struct {
	Snapshot []SnapshotKind `json:"snapshot"`
	Move     []MoveKind     `json:"move"`
	Resize   []ResizeKind   `json:"resize,omitempty"`
	// GPU is the model and count attached, absent without one.
	GPU *GPU `json:"gpu,omitempty"`
}

// UnmarshalJSON reads a capability value this version does not name as absent.
func (c *Capabilities) UnmarshalJSON(b []byte) error {
	type plain Capabilities
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	p.Snapshot = known(p.Snapshot, SnapshotVolumes, SnapshotMemory)
	p.Move = known(p.Move, MoveCold, MoveSnapshot, MoveLive)
	p.Resize = known(p.Resize, ResizeCPU, ResizeMemory)
	*c = Capabilities(p)
	return nil
}

// known keeps the values of got that are among names.
func known[T comparable](got []T, names ...T) []T {
	return slices.DeleteFunc(got, func(v T) bool { return !slices.Contains(names, v) })
}
