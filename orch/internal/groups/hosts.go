package groups

import (
	"context"
	"time"
)

// MachineSpec is what Hosts.Create makes: the template, named by the member's machine ID.
type MachineSpec struct {
	ID       string
	Template Template
	// BootSet is the boot set an OS-form template boots on the host that creates it, as
	// ResolveBootSet names it; empty for the image form.
	BootSet string
	// Address is the IPv4 address the Machine is to have, or empty.
	Address string
	// Fenced makes the Machine start only while its fence deadline is armed.
	Fenced bool
}

// Hosts is the host API as the engine needs it. Delete, Stop and Start of a Machine already
// deleted, stopped or running answer nil.
type Hosts interface {
	List(ctx context.Context) ([]Host, error)
	Create(ctx context.Context, hostID string, spec MachineSpec) error
	Delete(ctx context.Context, hostID, machineID string) error
	Stop(ctx context.Context, hostID, machineID string) error
	Start(ctx context.Context, hostID, machineID string) error
	RenewFence(ctx context.Context, hostID, machineID string, deadline time.Time) error
	InspectFence(ctx context.Context, hostID, machineID string) (deadline, lapsedAt time.Time, err error)
}
