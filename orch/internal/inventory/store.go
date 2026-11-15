package inventory

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Store keeps the inventory. Every method returns ErrNotFound for a record that is not there.
type Store interface {
	PutKey(ctx context.Context, k Key) error
	GetKey(ctx context.Context, id string) (Key, error)
	// RevokeKey marks the key revoked and appends ev in the same step.
	RevokeKey(ctx context.Context, id string, ev orchapi.Event) error

	PutPending(ctx context.Context, p Pending, ev *orchapi.Event) error
	GetPending(ctx context.Context, hostID string) (Pending, error)
	ListPending(ctx context.Context) ([]Pending, error)

	// Admit replaces the pending record p by the host h and appends ev, in one step that fails
	// with ErrNotFound when p is no longer pending. With alloc it also records the allocation in that
	// step, which fails with ErrAllocationTaken when the prefix is already allocated.
	Admit(ctx context.Context, p Pending, h Host, ev orchapi.Event, alloc *Allocation) error
	// RemoveHost deletes the host, frees its allocation and appends ev in one step, and returns the
	// host as it was.
	RemoveHost(ctx context.Context, id string, ev orchapi.Event) (Host, error)
	ListAllocations(ctx context.Context) ([]Allocation, error)
	// GetProfile returns the host profile's YAML, ErrNotFound when none is set. PutProfile replaces
	// it and appends ev in one step.
	GetProfile(ctx context.Context) ([]byte, error)
	PutProfile(ctx context.Context, yaml []byte, ev orchapi.Event) error
	GetHost(ctx context.Context, id string) (Host, error)
	ListHosts(ctx context.Context) ([]Host, error)
	// UpdateHost applies change to the stored host as one step and appends ev when it is not nil.
	UpdateHost(ctx context.Context, id string, change func(*Host), ev *orchapi.Event) (Host, error)

	AppendEvent(ctx context.Context, ev orchapi.Event) error
	ListEvents(ctx context.Context) ([]orchapi.Event, error)
}
