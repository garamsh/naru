// Package mvmd is naru-hostd's view of mvmd: the calls it makes over mvmd's local gRPC socket
// (runtime API v1alpha2), in the types the host API needs, and the errors it maps mvmd's outcomes to.
package mvmd

import (
	"context"
	"errors"
	"time"
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

// Rule is one rule of a Machine's network policy.
type Rule struct {
	Prefix   string      `json:"prefix"`
	Protocol string      `json:"protocol"`
	Ports    []PortRange `json:"ports,omitempty"`
}

// PortRange is first to last, inclusive.
type PortRange struct {
	First uint32 `json:"first"`
	Last  uint32 `json:"last"`
}

// Policy is a Machine's network policy.
type Policy struct {
	Egress  []Rule `json:"egress,omitempty"`
	Ingress []Rule `json:"ingress,omitempty"`
}

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
}

// Kernel is a guest kernel the node offers.
type Kernel struct {
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Default bool   `json:"default"`
}

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
}
