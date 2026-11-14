package groups

import (
	"context"
	"errors"
)

// ErrConflict is returned by PutMember when the record's Revision is not the stored one.
var ErrConflict = errors.New("revision conflict")

// Store holds the records the engine reads and writes. Groups are written by the API layer,
// so the engine only lists them.
type Store interface {
	ListGroups(ctx context.Context) ([]GroupSpec, error)
	ListMembers(ctx context.Context) ([]Member, error)
	// ListNetworks returns the networks the groups may join.
	ListNetworks(ctx context.Context) ([]Network, error)
	// PutMember writes m if m.Revision is the stored revision (0 creates) and returns the
	// record with its new revision; otherwise it returns ErrConflict. A member created with a
	// PodPrefix claims it for its network, so ErrConflict also means another member holds it;
	// the claim ends when a member of phase gone is written.
	PutMember(ctx context.Context, m Member) (Member, error)
}
