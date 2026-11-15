package groupsvc

import (
	"context"

	"github.com/garamsh/naru/orch/internal/groups"
)

// Store holds the groups and members. GetGroup and DeleteGroup return inventory.ErrNotFound for a
// group that is not there.
type Store interface {
	groups.Store
	// PutGroup writes g only while the stored group is still prev (absent when prev is nil), and
	// returns inventory.ErrConflict otherwise.
	PutGroup(ctx context.Context, g groups.GroupSpec, prev *groups.GroupSpec) error
	GetGroup(ctx context.Context, name string) (groups.GroupSpec, error)
	DeleteGroup(ctx context.Context, name string) error
}
