package machineapi

import (
	"encoding/json"
	"time"
)

// Version is the version of the Machine API these types and this client speak.
const Version = "machines/v1alpha1"

// PathPrefix is the path every operation of Version is served under.
const PathPrefix = "/" + Version + "/"

// Redacted is what a read answers in place of a value it never returns: an env value of a spec and
// an alias's key.
const Redacted = "<set>"

// State is the desired state of a Machine.
type State string

// The desired states. An empty State in a create is StateRunning.
const (
	StateRunning State = "running"
	StateStopped State = "stopped"
)

// Phase is where a Machine is in its life, as the status reports it.
type Phase string

// The phases. A client reads a phase it does not know as PhaseUnknown.
const (
	PhasePending  Phase = "pending"
	PhaseStarting Phase = "starting"
	PhaseRunning  Phase = "running"
	PhaseStopping Phase = "stopping"
	PhaseStopped  Phase = "stopped"
	PhaseMoving   Phase = "moving"
	PhaseFailed   Phase = "failed"
	PhaseDeleting Phase = "deleting"
	PhaseUnknown  Phase = "unknown"
)

// UnmarshalJSON reads a phase this version does not name as PhaseUnknown.
func (p *Phase) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	switch v := Phase(s); v {
	case PhasePending, PhaseStarting, PhaseRunning, PhaseStopping, PhaseStopped, PhaseMoving, PhaseFailed, PhaseDeleting:
		*p = v
	default:
		*p = PhaseUnknown
	}
	return nil
}

// Machine is a Machine as the API answers it: the caller's name for it, the id orch gave it, which
// never changes, its spec and its status.
type Machine struct {
	Name   string `json:"name"`
	ID     string `json:"id"`
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
}

// Spec is what a caller asks a Machine to be: the body of PUT .../machines/{name}. A read answers
// it with every env value as Redacted and every file without its content.
type Spec struct {
	// Image is repository@sha256:<64 hex digits>.
	Image         string   `json:"image"`
	CPU           uint32   `json:"cpu"`
	MemoryBytes   uint64   `json:"memoryBytes"`
	RootDiskBytes uint64   `json:"rootDiskBytes"`
	Volumes       []Volume `json:"volumes,omitempty"`
	// Command, Args, Env, User, WorkingDir and Files are mvmd's run configuration of Create
	// Machine, with its meaning: Command replaces the image's entrypoint, Args its command.
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	User       string            `json:"user,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Files      []File            `json:"files,omitempty"`
	// Services are the ports a proxy may reach; the first is the default service (ADR 0112).
	Services      []Service     `json:"services,omitempty"`
	NetworkPolicy NetworkPolicy `json:"networkPolicy"`
	Shape         Shape         `json:"shape"`
	// State is left out of create's comparison; start and stop change it.
	State State `json:"state,omitempty"`
	// FromSnapshot makes a restore: the volumes are made from the snapshot, which must hold
	// volumes of the same names and sizes.
	FromSnapshot *SnapshotSource `json:"fromSnapshot,omitempty"`
}

// Volume is a disk that lives as long as its Machine and moves with it, formatted ext4 and
// mounted at MountPath.
type Volume struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes"`
	MountPath string `json:"mountPath"`
}

// File is a file placed in the guest before the entrypoint runs. A read answers it without
// Content and with SizeBytes, which a create does not take.
type File struct {
	Path      string `json:"path"`
	Mode      uint32 `json:"mode"`
	Content   []byte `json:"content,omitempty"`
	SizeBytes uint64 `json:"sizeBytes,omitempty"`
}

// ServiceProtocol is how a proxy carries a service.
type ServiceProtocol string

// The service protocols: http carries HTTP/1.1, HTTP/2 and WebSocket; tcp is TLS from the client
// terminated at the proxy by SNI, and plain TCP to the Machine.
const (
	ServiceHTTP ServiceProtocol = "http"
	ServiceTCP  ServiceProtocol = "tcp"
)

// ProxyProtocolV2 is the one ProxyProtocol a service takes: the proxy writes the client's address
// first.
const ProxyProtocolV2 = "v2"

// Service is a port of the Machine a proxy may reach, under a DNS label without "--".
type Service struct {
	Name          string          `json:"name"`
	Port          uint16          `json:"port"`
	Protocol      ServiceProtocol `json:"protocol"`
	ProxyProtocol string          `json:"proxyProtocol,omitempty"`
}

// NetworkPolicy is the Machine's egress: a packet leaves only if a rule matches. What reaches the
// Machine is its services, from the proxies, and nothing else.
type NetworkPolicy struct {
	Egress []Rule `json:"egress,omitempty"`
}

// Rule is one egress rule: a destination prefix, a protocol (tcp, udp, icmp, icmpv6 or any) and,
// for tcp and udp, the destination ports, every port when empty.
type Rule struct {
	Prefix   string      `json:"prefix"`
	Protocol string      `json:"protocol"`
	Ports    []PortRange `json:"ports,omitempty"`
}

// PortRange is First to Last, inclusive.
type PortRange struct {
	First uint16 `json:"first"`
	Last  uint16 `json:"last"`
}

// Shape is what a host must offer to run the Machine. A caller names a shape, never a host.
type Shape struct {
	Region string `json:"region,omitempty"`
	// Labels are values a host must carry.
	Labels map[string]string `json:"labels,omitempty"`
	GPU    *GPU              `json:"gpu,omitempty"`
}

// GPU is a count of whole devices of one model (ADR 0111).
type GPU struct {
	Model string `json:"model"`
	Count uint32 `json:"count"`
}

// SnapshotSource names the snapshot a restore makes its volumes from.
type SnapshotSource struct {
	Snapshot string `json:"snapshot"`
}

// Status is what orch last learned of the Machine.
type Status struct {
	Phase Phase `json:"phase"`
	// Reason says why a pending, failed or unknown Machine is so, naming what of the spec or shape
	// cannot be met and no figure of the fleet's capacity.
	Reason             string       `json:"reason,omitempty"`
	Address            Address      `json:"address"`
	Generation         uint64       `json:"generation"`
	ObservedGeneration uint64       `json:"observedGeneration"`
	Capabilities       Capabilities `json:"capabilities"`
}

// Address is how the Machine is reached: Name is its stable name, <id>.<zone>, which no move
// changes. Its other services are <service>--<id>.<zone>.
type Address struct {
	Name string `json:"name"`
}

// StopRequest is the body of POST .../machines/{name}/stop: how long the guest has to stop before
// it is ended, at most MaxGraceSeconds.
type StopRequest struct {
	GraceSeconds uint32 `json:"graceSeconds,omitempty"`
}

// MaxGraceSeconds is the most a stop's grace may be: the host ends a stop within its own call.
const MaxGraceSeconds = 240

// MachineList is the body of GET .../machines: the caller's Machines, ordered by name, and the
// cursor of the event log the list is current at, from which GET .../events misses nothing.
type MachineList struct {
	Machines []Machine `json:"machines"`
	Cursor   string    `json:"cursor"`
}

// EventType says what an Event reports.
type EventType string

// The event types. A client skips a type it does not know.
const (
	// EventStatus is a change of the Machine's status, which the event carries.
	EventStatus EventType = "status"
	// EventDeleted is the Machine gone, its volumes with it, and its name free.
	EventDeleted EventType = "deleted"
)

// Event is one change of one of the caller's Machines.
type Event struct {
	Type    EventType `json:"type"`
	Machine string    `json:"machine"`
	ID      string    `json:"id"`
	Status  Status    `json:"status"`
	Time    time.Time `json:"time"`
}

// EventPage is the body of GET .../events: the events after the cursor asked for, oldest first,
// and the cursor to ask from next.
type EventPage struct {
	Events []Event `json:"events"`
	Cursor string  `json:"cursor"`
}

// ExecRequest is the body of POST .../machines/{name}/exec: the command, run without input (a
// caller with input writes it as a guest file first), and the seconds it may run, at most
// MaxExecSeconds; 0 is MaxExecSeconds.
type ExecRequest struct {
	Command        []string `json:"command"`
	TimeoutSeconds uint32   `json:"timeoutSeconds,omitempty"`
}

// MaxExecSeconds is the most an exec runs before it is ended with SIGKILL.
const MaxExecSeconds = 600

// MaxGuestBytes is the most an exec's output, stdout and stderr together, or a guest file holds:
// above it the call is 413.
const MaxGuestBytes = 16 << 20

// ExecResult is what an exec returns, whole. A command a signal ended has Signal, its number as on
// Linux, and ExitCode 128 plus it, as a shell gives; TimedOut is a command ended with SIGKILL at its
// timeout. A command that did not start has ExitCode 127 and the reason in Stderr.
type ExecResult struct {
	ExitCode int32  `json:"exitCode"`
	Signal   int32  `json:"signal,omitempty"`
	TimedOut bool   `json:"timedOut,omitempty"`
	Stdout   []byte `json:"stdout"`
	Stderr   []byte `json:"stderr"`
}
