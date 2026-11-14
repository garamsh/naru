// Package hostapi holds the request and response types of the host API that naru-hostd serves
// (docs/architecture/host.md), and a client for it. The server and its callers share these types,
// so a field one side adds is one the other reads.
package hostapi

import "time"

// Rule is one rule of a microVM's network policy.
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

// Policy is a microVM's network policy.
type Policy struct {
	Egress  []Rule `json:"egress,omitempty"`
	Ingress []Rule `json:"ingress,omitempty"`
}

// Kernel is a guest kernel the host offers.
type Kernel struct {
	Name    string `json:"name"`
	Digest  string `json:"digest"`
	Default bool   `json:"default"`
}

// ErrorBody is the body of every refusal and failure.
type ErrorBody struct {
	Error  string   `json:"error"`
	Fields []string `json:"fields,omitempty"`
}

// Resources are the amounts GET /v1/host reports as capacity and as free. Memory and Disk are bytes.
type Resources struct {
	CPU    uint32 `json:"cpu"`
	Memory uint64 `json:"memory"`
	Disk   uint64 `json:"disk"`
}

// MvmdInfo is what GET /v1/host says of mvmd: the runtime API version it serves, its driver, and
// the guest kernels a microVM can boot.
type MvmdInfo struct {
	Version string   `json:"version"`
	Driver  string   `json:"driver"`
	Kernels []Kernel `json:"kernels"`
}

// Host is the body of GET /v1/host.
type Host struct {
	Region   string            `json:"region"`
	Labels   map[string]string `json:"labels"`
	Capacity Resources         `json:"capacity"`
	Free     Resources         `json:"free"`
	Mvmd     MvmdInfo          `json:"mvmd"`
}

// DiskSpec is a disk of a microVM, created with it and deleted with it.
type DiskSpec struct {
	Name      string `json:"name"`
	SizeBytes uint64 `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// FenceRequest is the body of POST /v1/microvms/{name}/fence: the fence's deadline becomes
// LengthSeconds from the moment mvmd receives it, unless Renewal is not above the last one mvmd
// applied, which changes nothing.
type FenceRequest struct {
	LengthSeconds uint64 `json:"lengthSeconds"`
	Renewal       uint64 `json:"renewal"`
}

// The phases of a fence.
const (
	FenceNotArmed = "notArmed"
	FenceArmed    = "armed"
	FenceExpired  = "expired"
)

// Fence is a microVM's fence as mvmd reports it.
type Fence struct {
	Phase            string `json:"phase"`
	LengthSeconds    uint64 `json:"lengthSeconds"`
	RemainingSeconds uint64 `json:"remainingSeconds"`
	// Renewal is the last renewal number mvmd applied.
	Renewal uint64 `json:"renewal"`
}

// SnapshotRequest is the body of POST /v1/microvms/{name}/snapshot: the name the snapshot takes.
type SnapshotRequest struct {
	Name string `json:"name"`
}

// SnapshotDisk is a disk a snapshot's microVM had attached.
type SnapshotDisk struct {
	ID        string `json:"id"`
	SizeBytes uint64 `json:"sizeBytes"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// Snapshot is a snapshot of a microVM as the host holds it.
type Snapshot struct {
	Name string `json:"name"`
	// MicroVM is the microVM it was taken of, which may no longer exist.
	MicroVM     string `json:"microvm"`
	ImageDigest string `json:"imageDigest"`
	// Driver and CPUModel are what a host needs to match to resume it.
	Driver        string         `json:"driver"`
	CPUModel      string         `json:"cpuModel"`
	Kernel        string         `json:"kernel,omitempty"`
	RootDiskBytes uint64         `json:"rootDiskBytes"`
	Disks         []SnapshotDisk `json:"disks"`
	SizeBytes     uint64         `json:"sizeBytes"`
	CreatedAt     time.Time      `json:"createdAt,omitzero"`
}

// ResumeRequest is the body of POST /v1/snapshots/{name}/resume: the new microVM's name, its
// network policy, its routed prefix and the network it is attached to, which the snapshot does
// not carry. As in a create, the pod prefix must be inside the network's prefix.
type ResumeRequest struct {
	Name          string `json:"name"`
	NetworkPolicy Policy `json:"networkPolicy"`
	Network       string `json:"network,omitempty"`
	PodPrefix     string `json:"podPrefix,omitempty"`
}
