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

	// Reserve takes a part of space for hostID, chosen by pick from the parts of space already
	// claimed, and holds it until the given time. Reservations that ran out before now do not claim a
	// part and are dropped. A reservation hostID still holds in space is returned again, extended. The
	// choice and the write are one step that holds only while no claim of space changed in between,
	// and, in SpaceMesh, while the profile is the one stored before pick ran.
	Reserve(ctx context.Context, space Space, hostID string, pick func(claimed []netip.Prefix) (netip.Prefix, error), until, now time.Time) (Allocation, error)
	// ClaimPrefix records a as confirmed in SpaceMachine: the Machine prefix the host a.HostID reports
	// holding. It is
	// the same record Reserve and Admit make for a joined host, so one set of records tells the pool
	// what every host holds. It does nothing when the host already holds the prefix, takes over its own
	// reservation or one that ran out, and is ErrAllocationTaken when another host holds it and
	// ErrNotFound when the host is not admitted.
	ClaimPrefix(ctx context.Context, a Allocation) error
	// Release drops the allocation a from a.Space, when it is still the one stored.
	Release(ctx context.Context, a Allocation) error
	// Admit replaces the pending record p by the host h and appends ev, in one step that fails
	// with ErrNotFound when p is no longer pending. It also confirms each of allocs, the reservations
	// Reserve returned, in that step, which fails with ErrAllocationTaken when one is no longer the
	// stored one.
	Admit(ctx context.Context, p Pending, h Host, ev orchapi.Event, allocs ...Allocation) error
	// GiveMeshAddress gives the admitted host hostID a mesh address chosen by pick from the
	// addresses already claimed, writing the confirmed allocation and the host's MeshAddress in one
	// step that holds only while neither the claims, the host nor the profile changed since before
	// pick ran. A host that already holds one is returned as it is. It is ErrNotFound when the host is
	// not admitted.
	GiveMeshAddress(ctx context.Context, hostID string, pick func(claimed []netip.Prefix) (netip.Prefix, error)) (Host, error)
	// DropStoredConfigs rewrites every host record that carries a stored config without it and returns
	// how many it rewrote.
	DropStoredConfigs(ctx context.Context) (int, error)
	// RemoveHost deletes the host, frees its allocations in every space and appends ev in one step,
	// and returns the host as it was.
	RemoveHost(ctx context.Context, id string, ev orchapi.Event) (Host, error)
	// ListAllocations returns the allocations of space, each with its Space set.
	ListAllocations(ctx context.Context, space Space) ([]Allocation, error)
	// GetProfile returns the host profile's YAML, ErrNotFound when none is set. PutProfile replaces
	// it and appends ev in one step. check is given the profile it replaces (nil for none) after the
	// store read it with the address-space guard the networks and the address pools share and the
	// mesh addresses, and the profile is written only while none of the three changed since; a write
	// that raced one is ErrConflict.
	GetProfile(ctx context.Context) ([]byte, error)
	PutProfile(ctx context.Context, yaml []byte, ev orchapi.Event, check func(current []byte) error) error
	GetHost(ctx context.Context, id string) (Host, error)
	ListHosts(ctx context.Context) ([]Host, error)
	// UpdateHost applies change to the stored host as one step and appends ev when it is not nil.
	UpdateHost(ctx context.Context, id string, change func(*Host), ev *orchapi.Event) (Host, error)

	AppendEvent(ctx context.Context, ev orchapi.Event) error
	// ListEvents lists the retained events after the cursor after, oldest first; an empty after lists
	// them all. Each carries its Cursor.
	ListEvents(ctx context.Context, after string) ([]orchapi.Event, error)
}
