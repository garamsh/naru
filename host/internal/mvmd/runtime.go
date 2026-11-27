// Package mvmd is naru-hostd's view of mvmd: the calls it makes over mvmd's local gRPC socket
// (runtime API v1alpha2), in the types the host API needs, and the errors it maps mvmd's outcomes to.
package mvmd

import (
	"context"
	"errors"
	"io"
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

// The filesystems of a disk, as a disk stream's header names them.
const (
	FilesystemExt4 = "ext4"
	FilesystemNone = "none"
)

// DiskHeader opens a disk's stream: the size of the disk and what it holds, FilesystemExt4 or
// FilesystemNone.
type DiskHeader struct {
	SizeBytes  uint64
	Filesystem string
}

// DiskExtent is a run of a disk's bytes at Offset. A stream's extents ascend and do not overlap,
// and a byte no extent covers is zero.
type DiskExtent struct {
	Offset uint64
	Data   []byte
}

// DiskPart is one message of a disk's stream: a DiskHeader, then DiskExtents, then the DiskDigest
// that ends it.
type DiskPart interface{ diskPart() }

// DiskDigest is the SHA-256 in hex of a disk's whole size, holes counted as zeros.
type DiskDigest string

func (DiskHeader) diskPart() {}
func (DiskExtent) diskPart() {}
func (DiskDigest) diskPart() {}

// Attachment is a disk of a Machine, and where the guest mounts it; empty mounts nothing.
type Attachment struct {
	ID        string
	ReadOnly  bool
	MountPath string
}

// File is a file mvmd places in an image-form Machine before its entrypoint runs, and FileInfo what
// mvmd reports of one: no content.
type (
	File     = hostapi.File
	FileInfo struct {
		Path      string
		Mode      uint32
		SizeBytes uint64
	}
)

// DiskSnapshot is a copy of a disk mvmd keeps. AllocatedBytes is the space its file occupies.
type DiskSnapshot struct {
	ID             string
	DiskID         string
	SizeBytes      uint64
	AllocatedBytes uint64
	CreatedAt      time.Time
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
	// EnvNames, Files and WorkingDir are what mvmd reports of an image-form Machine's environment,
	// files and working directory: the names, and the files without their content.
	EnvNames   []string
	Files      []FileInfo
	WorkingDir string
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
	// RequestedIPv4 is the address the Machine is to have, inside the node's Machine prefix; empty leaves
	// the choice to mvmd, which refuses an address it cannot give (ADR 0069 of mvmd).
	RequestedIPv4 string
	// RestartOnBoot makes mvmd start the Machine again at the first fencing deadline of a new host
	// boot when it ran when the host went down and was not stopped since. It is fixed for the
	// Machine's life.
	RestartOnBoot bool
	// Fenced gives the Machine a fence of its own, not armed until it is renewed; mvmd refuses to
	// start a fenced Machine before that.
	Fenced bool
	// Command, Args, Env, User, WorkingDir and Files are an image-form Machine's, each replacing or
	// adding to the image's as mvmd's CreateMachine describes; empty keeps the image's.
	Command    []string
	Args       []string
	Env        map[string]string
	User       string
	WorkingDir string
	Files      []File
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
	// BootSets are the boot sets the node offers an OS-form Machine: the ones its configuration
	// names and the ones AddBootSet added. They are not the Kernels, which an image-form Machine boots.
	BootSets []BootSet
	// MachinePrefixes are the prefixes mvmd gives the addresses of Machines from.
	MachinePrefixes []string
}

// BootSet is a boot set to add: its name and the SHA-256 digests, in hex, the kernel and the initrd
// must have.
type BootSet struct {
	Name         string
	KernelDigest string
	InitrdDigest string
	// Origin is how the node came to offer the set: "configured" or "added". It is empty in a set to add.
	Origin string
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

// ExecStatus is how far an exec got.
type ExecStatus int

const (
	ExecStarted ExecStatus = iota
	// ExecNotStarted is a command the guest did not start; the record's Reason says why.
	ExecNotStarted
	// ExecCutShort is a command the Machine's stop ended.
	ExecCutShort
	// ExecExited is a command that ended: ExitCode, or TermSignal when a signal ended it.
	ExecExited
)

// ExecRecord is an exec as mvmd records it.
type ExecRecord struct {
	Status     ExecStatus
	Reason     string
	ExitCode   int32
	TermSignal int32
}

// Chunk is a piece of an exec's output: Stderr's or stdout's, at Offset in the whole output.
type Chunk struct {
	Stderr bool
	Offset uint64
	Data   []byte
}

// Runtime is mvmd. Every method returns one of the errors above, wrapped, or mvmd's own.
type Runtime interface {
	Info(ctx context.Context) (Info, error)
	// Pull pulls the image repository@digest, which mvmd keeps by digest.
	Pull(ctx context.Context, repository, digest string) error
	CreateDisk(ctx context.Context, id string, sizeBytes uint64) error
	// CreateDiskFrom creates a disk of sizeBytes, at least the snapshot's, as a copy of the disk
	// snapshot; ErrNotFound when mvmd has no such snapshot.
	CreateDiskFrom(ctx context.Context, id string, sizeBytes uint64, snapshotID string) error
	// SnapshotDisk copies a disk into a disk snapshot under the ID the caller chooses; the same ID
	// again is the original outcome.
	SnapshotDisk(ctx context.Context, snapshotID, diskID string) (DiskSnapshot, error)
	ListDiskSnapshots(ctx context.Context) ([]DiskSnapshot, error)
	// DeleteDiskSnapshot deletes a disk snapshot; an absent one is ErrNotFound.
	DeleteDiskSnapshot(ctx context.Context, id string) error
	// CreateRawDisk creates a disk with no filesystem holding contents (at most 1 MiB), of the
	// contents' length rounded up to 512 when sizeBytes is 0. The same contents under the same ID
	// is the original outcome; other contents are ErrConflict.
	CreateRawDisk(ctx context.Context, id string, sizeBytes uint64, contents []byte) error
	DeleteDisk(ctx context.Context, id string) error
	ListDisks(ctx context.Context) ([]Disk, error)
	// ReadDisk passes the disk's stream to sink, part by part: a disk no Machine holds, or a
	// Created or Stopped Machine's (ErrPrecondition otherwise). sink's error ends the read and is
	// returned as it is.
	ReadDisk(ctx context.Context, id string, sink func(DiskPart) error) error
	// CreateDiskFromStream creates the disk of the header from the extents and then the digest
	// that parts sends, in order. mvmd publishes it only when the bytes hash to the digest; a
	// mismatch is ErrInvalid and leaves no disk. The same ID, header and digest again is success.
	// An error parts returns ends the stream with no disk and is returned as it is.
	CreateDiskFromStream(ctx context.Context, id string, h DiskHeader, parts func(send func(DiskPart) error) error) error
	// CreateMachine creates the Machine under a request ID of its own, so a repeat of the same
	// parameters is mvmd's original outcome and other parameters are ErrConflict.
	CreateMachine(ctx context.Context, c Create) (Machine, error)
	// AttachDisk attaches a disk to a Created or Stopped Machine; it appears at the next boot.
	AttachDisk(ctx context.Context, machineID string, a Attachment) (Machine, error)
	StartMachine(ctx context.Context, id string) (Machine, error)
	// StopMachine stops the Machine, ending it when its guest has not stopped within grace.
	StopMachine(ctx context.Context, id string, grace time.Duration) error
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
	// AddBootSet streams the kernel and then the initrd to mvmd, which refuses a file whose
	// SHA-256 is not the set's (ErrInvalid). The same name and digests is success, and the same
	// name with other digests is ErrConflict.
	AddBootSet(ctx context.Context, set BootSet, kernel, initrd io.Reader) error
	// RemoveBootSet removes a set added by AddBootSet. An absent one is success; a set a Machine
	// names, or one the node's configuration offers, is ErrPrecondition.
	RemoveBootSet(ctx context.Context, name string) error

	// Exec starts command in the Running Machine under execID, once: the same ID again is the same
	// record. A Machine that is not Running is ErrPrecondition.
	Exec(ctx context.Context, machineID, execID string, command []string) (ExecRecord, error)
	// ReadExecOutput passes each chunk of the exec's output after the offset to sink, in order,
	// and returns the record once the command ended. A read the channel to the guest cut is read
	// again from the last chunk passed; sink's error ends the read and is returned as it is.
	ReadExecOutput(ctx context.Context, machineID, execID string, after uint64, sink func(Chunk) error) (ExecRecord, error)
	// SignalExec sends the signal, its number as on Linux, to the exec's command. A command that
	// has ended is ErrPrecondition.
	SignalExec(ctx context.Context, machineID, execID string, signal int32) error
	// ReadFile passes the content of the guest's file to sink, in order; an absent file is
	// ErrNotFound, and sink's error ends the read and is returned as it is.
	ReadFile(ctx context.Context, machineID, path string, sink func([]byte) error) error
	// WriteFile replaces the guest's file with what content reads.
	WriteFile(ctx context.Context, machineID, path string, content io.Reader) error
}
