// Package mvmd is naru-hostd's view of mvmd: the calls it makes over mvmd's local gRPC socket
// (runtime API v1alpha2), in the types the host API needs, and the errors it maps mvmd's outcomes to.
package mvmd

import (
	"context"
	"errors"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
)

// The outcomes the host API tells apart. Everything else is a failure of mvmd or of the socket.
var (
	// ErrNotFound is a Machine, disk or image mvmd does not have.
	ErrNotFound = errors.New("not found")
	// ErrConflict is an identifier mvmd holds with other parameters.
	ErrConflict = errors.New("conflict")
	// ErrInvalid is a request mvmd rejected as invalid; the error's text is mvmd's reason.
	ErrInvalid = errors.New("invalid")
	// ErrPrecondition is a Machine in a state that does not allow the call.
	ErrPrecondition = errors.New("precondition failed")
	// ErrUnavailable is mvmd not answering: it is down, restarting or not yet listening.
	ErrUnavailable = errors.New("mvmd is not answering")
)

// States of a Machine, as the host API names them.
const (
	StateCreated = "created"
	StateRunning = "running"
	StatePaused  = "paused"
	StateStopped = "stopped"
	StateFaulted = "faulted"
)

// Rule, PortRange and Policy are a Machine's network policy, in the host API's types.
type (
	Rule      = hostapi.Rule
	PortRange = hostapi.PortRange
	Policy    = hostapi.Policy
)

// Disk is a disk mvmd holds. Holder is the Machine it is attached to, or empty.
type Disk struct {
	ID        string
	SizeBytes uint64
	Holder    string
}

// Attachment is a disk of a Machine.
type Attachment struct {
	ID       string
	ReadOnly bool
}

// Machine is what mvmd reports of a Machine.
type Machine struct {
	ID            string
	State         string
	Failure       string
	Addresses     []string
	ImageDigest   string
	CPU           uint32
	MemoryBytes   uint64
	RootDiskBytes uint64
	Disks         []Attachment
	Policy        Policy
	// BootSet, Cmdline and RoutedPrefixes are the OS form's: the boot set the Machine boots, the
	// kernel command line added to it, and the prefixes mvmd routes to the Machine.
	BootSet        string
	Cmdline        string
	RoutedPrefixes []string
	// RestartOnBoot is whether mvmd starts the Machine again after the host reboots, when it ran.
	RestartOnBoot bool
	// Fenced is whether the Machine has a fence of its own, and FenceLapsedAt when it last lapsed,
	// zero until it has. After a lapse Failure begins "the Machine's own fence lapsed".
	Fenced        bool
	FenceLapsedAt time.Time
}

// Create is what CreateMachine takes.
type Create struct {
	ID            string
	ImageDigest   string
	CPU           uint32
	MemoryBytes   uint64
	RootDiskBytes uint64
	Disks         []Attachment
	Policy        Policy
	// BootSet, Cmdline and RoutedPrefixes make an OS-form Machine: it has no image and no root
	// disk, and boots the boot set the node offers.
	BootSet        string
	Cmdline        string
	RoutedPrefixes []string
	// RestartOnBoot makes mvmd start the Machine again at the first fencing deadline of a new host
	// boot when it ran when the host went down and was not stopped since. It is fixed for the
	// Machine's life.
	RestartOnBoot bool
	// Fenced gives the Machine a fence of its own, not armed until it is renewed; mvmd refuses to
	// start a fenced Machine before that.
	Fenced bool
}

// Kernel is a guest kernel the node offers, in the host API's type.
type Kernel = hostapi.Kernel

// Info is what GetRuntimeInfo reports.
type Info struct {
	// APIVersion is the newest runtime API version mvmd serves, and Driver the driver's name and version.
	APIVersion string
	Driver     string
	// CPUs and MemoryBytes are the node's capacity; StateTotalBytes and StateAvailableBytes the state disk's.
	CPUs                                 uint32
	MemoryBytes                          uint64
	StateTotalBytes, StateAvailableBytes uint64
	Kernels                              []Kernel
	// MachinePrefixes are the prefixes mvmd gives the addresses of Machines from.
	MachinePrefixes []string
}

// Deadline is mvmd's fencing deadline.
type Deadline struct {
	Armed      bool
	LastNumber uint64
	Remaining  time.Duration
	// Phase is "notArmed", "armed", "expired" or "stoppedEverything", and Length the length of the
	// last renewal.
	Phase  string
	Length time.Duration
}

// Snapshot is a snapshot mvmd holds.
type Snapshot struct {
	ID          string
	MachineID   string
	ImageDigest string
	Driver      string
	CPUModel    string
	Kernel      string
	RootDisk    uint64
	Disks       []SnapshotDisk
	SizeBytes   uint64
	CreatedAt   time.Time
}

// SnapshotDisk is a disk a snapshot's Machine had attached.
type SnapshotDisk struct {
	ID        string
	ReadOnly  bool
	SizeBytes uint64
}

// Restore is what RestoreMachine takes: the new Machine's ID, the snapshot, and the policy and
// routed prefixes a snapshot does not carry.
type Restore struct {
	ID             string
	SnapshotID     string
	Policy         Policy
	RoutedPrefixes []string
}

// Runtime is mvmd. Every method returns one of the errors above, wrapped, or mvmd's own.
type Runtime interface {
	Info(ctx context.Context) (Info, error)
	// Pull pulls the image repository@digest, which mvmd keeps by digest.
	Pull(ctx context.Context, repository, digest string) error
	CreateDisk(ctx context.Context, id string, sizeBytes uint64) error
	// CreateRawDisk creates a disk with no filesystem holding contents (at most 1 MiB), of the
	// contents' length rounded up to 512 when sizeBytes is 0. The same contents under the same ID
	// is the original outcome; other contents are ErrConflict.
	CreateRawDisk(ctx context.Context, id string, sizeBytes uint64, contents []byte) error
	DeleteDisk(ctx context.Context, id string) error
	ListDisks(ctx context.Context) ([]Disk, error)
	// CreateMachine creates the Machine under a request ID of its own, so a repeat of the same
	// parameters is mvmd's original outcome and other parameters are ErrConflict.
	CreateMachine(ctx context.Context, c Create) (Machine, error)
	// AttachDisk attaches a disk to a Created or Stopped Machine; it appears at the next boot.
	AttachDisk(ctx context.Context, machineID string, a Attachment) (Machine, error)
	StartMachine(ctx context.Context, id string) (Machine, error)
	StopMachine(ctx context.Context, id string) error
	// DeleteMachine deletes a Created, Stopped or Faulted Machine. An absent one is success.
	DeleteMachine(ctx context.Context, id string) error
	InspectMachine(ctx context.Context, id string) (Machine, error)
	ListMachines(ctx context.Context) ([]Machine, error)
	// Renew sets the fencing deadline to length from now, under the renewal number.
	Renew(ctx context.Context, length time.Duration, number uint64) error
	InspectDeadline(ctx context.Context) (Deadline, error)
	// RenewMachine and InspectMachineDeadline are the same for one fenced Machine's own fence.
	// ErrNotFound is an unknown Machine and ErrPrecondition one that is not fenced.
	RenewMachine(ctx context.Context, id string, length time.Duration, number uint64) (Deadline, error)
	InspectMachineDeadline(ctx context.Context, id string) (Deadline, error)
	// SnapshotMachine takes a snapshot of a Running or Paused Machine under the ID the caller
	// chooses; the same ID again is the original outcome.
	SnapshotMachine(ctx context.Context, snapshotID, machineID string) error
	ListSnapshots(ctx context.Context) ([]Snapshot, error)
	// DeleteSnapshot deletes a snapshot; an absent one is success and one in use is ErrPrecondition.
	DeleteSnapshot(ctx context.Context, id string) error
	// RestoreMachine creates a new Running Machine from a snapshot.
	RestoreMachine(ctx context.Context, r Restore) (Machine, error)
}
