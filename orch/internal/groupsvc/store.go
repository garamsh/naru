package groupsvc

import (
	"context"

	"github.com/garamsh/naru/orch/internal/groups"
)

// Store holds the groups and members. GetGroup and DeleteGroup return inventory.ErrNotFound for a
// group that is not there.
type Store interface {
	groups.Store
	PutGroup(ctx context.Context, g groups.GroupSpec) error
	GetGroup(ctx context.Context, name string) (groups.GroupSpec, error)
	DeleteGroup(ctx context.Context, name string) error
}
