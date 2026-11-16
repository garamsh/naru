package inventory

import (
	"context"
	"net/netip"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
)

// Store keeps the inventory. Every method returns ErrNotFound for a record that is not there.
type Store interface {
	PutKey(ctx context.Context, k Key) error
	GetKey(ctx context.Context, id string) (Key, error)
	// RevokeKey marks the key revoked and appends ev in the same step.
	RevokeKey(ctx context.Context, id string, ev orchapi.Event) error

	// PutPending writes the pending record p and appends ev, in one step that holds only while the
	// host is not admitted (ErrAlreadyAdmitted otherwise) and, with create, while p is not pending, or
	// without it while p is (ErrConflict otherwise).
	PutPending(ctx context.Context, p Pending, ev *orchapi.Event, create bool) error
	GetPending(ctx context.Context, hostID string) (Pending, error)
	ListPending(ctx context.Context) ([]Pending, error)

	// Reserve takes a Machine prefix for hostID, chosen by pick from the prefixes already claimed,
	// and holds it until the given time. Reservations that ran out before now do not claim a prefix
	// and are dropped. A reservation hostID still holds is returned again, extended. The choice and
	// the write are one step that holds only while no claim changed in between.
	Reserve(ctx context.Context, hostID string, pick func(claimed []netip.Prefix) (netip.Prefix, error), until, now time.Time) (Allocation, error)
	// Release drops the reservation a, when it is still the one stored.
	Release(ctx context.Context, a Allocation) error
	// Admit replaces the pending record p by the host h and appends ev, in one step that fails
	// with ErrNotFound when p is no longer pending. With alloc, the reservation Reserve returned, it
	// also confirms the allocation in that step, which fails with ErrAllocationTaken when the
	// reservation is no longer the stored one.
	Admit(ctx context.Context, p Pending, h Host, ev orchapi.Event, alloc *Allocation) error
	// DropStoredConfigs rewrites every host record that carries a stored config without it and returns
	// how many it rewrote.
	DropStoredConfigs(ctx context.Context) (int, error)
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
	// ListEvents lists the retained events after the cursor after, oldest first; an empty after lists
	// them all. Each carries its Cursor.
	ListEvents(ctx context.Context, after string) ([]orchapi.Event, error)
}
