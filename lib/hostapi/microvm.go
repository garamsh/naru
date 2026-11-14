package hostapi

import "time"

// CreateRequest is the body of POST /v1/microvms.
type CreateRequest struct {
	Name string `json:"name"`
	// Image is repository@sha256:<64 hex digits>, which hostd has mvmd pull, or the bare digest of
	// an image mvmd already has.
	Image         string            `json:"image"`
	CPU           uint32            `json:"cpu"`
	Memory        uint64            `json:"memory"`
	RootDiskBytes uint64            `json:"rootDiskBytes"`
	Disks         []DiskSpec        `json:"disks,omitempty"`
	NetworkPolicy Policy            `json:"networkPolicy"`
	Env           map[string]string `json:"env,omitempty"`
	Command       []string          `json:"command,omitempty"`
	// RestartOnBoot makes mvmd start the microVM again after the host reboots, when it ran and was
	// not stopped. Absent, it is true for the OS form and false for the image form.
	RestartOnBoot *bool `json:"restartOnBoot,omitempty"`
	// Fenced gives the microVM a fence of its own. It is created and not started: the caller
	// renews the fence (POST /v1/microvms/{name}/fence), then starts it.
	Fenced bool `json:"fenced,omitempty"`

	// The OS form, in place of Image, RootDiskBytes, Disks, Env and Command: a microVM that boots
	// BootSet, with Cmdline added to the kernel's command line, a data disk of DataDisk bytes, a
	// NoCloud seed whose user-data is Seed, and PodPrefix routed to it.
	BootSet   string `json:"bootSet,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	Seed      []byte `json:"seed,omitempty"`
	DataDisk  uint64 `json:"dataDisk,omitempty"`
	PodPrefix string `json:"podPrefix,omitempty"`
}

// MicroVM is a microVM as the host API shows it.
type MicroVM struct {
	Name string `json:"name"`
	// MachineID is mvmd's identifier of the Machine, which is the name.
	MachineID string `json:"machineId"`
	State     string `json:"state"`
	// Failure is the reason of the last failure mvmd recorded. After a lapse of the microVM's own
	// fence it begins "the Machine's own fence lapsed".
	Failure       string     `json:"failure,omitempty"`
	Addresses     []string   `json:"addresses"`
	ImageDigest   string     `json:"imageDigest"`
	CPU           uint32     `json:"cpu"`
	Memory        uint64     `json:"memory"`
	RootDiskBytes uint64     `json:"rootDiskBytes"`
	Disks         []DiskSpec `json:"disks"`
	NetworkPolicy Policy     `json:"networkPolicy"`
	// BootSet, Cmdline and PodPrefix are an OS-form microVM's.
	BootSet   string `json:"bootSet,omitempty"`
	Cmdline   string `json:"cmdline,omitempty"`
	PodPrefix string `json:"podPrefix,omitempty"`
	// RestartOnBoot is whether mvmd starts the microVM again after the host reboots.
	RestartOnBoot bool `json:"restartOnBoot"`
	// Fenced is whether the microVM has a fence of its own; FenceLapsedAt is when it last lapsed
	// and ended the microVM, and is absent until it has.
	Fenced        bool      `json:"fenced"`
	FenceLapsedAt time.Time `json:"fenceLapsedAt,omitzero"`
}
