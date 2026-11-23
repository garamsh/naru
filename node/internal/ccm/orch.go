package ccm

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Orch is the part of orch's API the controller calls: the exposure calls of its network and the
// list of that network's members. *orchapi.Client satisfies it.
type Orch interface {
	ApplyExposure(ctx context.Context, id string, spec orchapi.ExposureSpec) (orchapi.Exposure, error)
	Exposure(ctx context.Context, id string) (orchapi.Exposure, error)
	Exposures(ctx context.Context) ([]orchapi.Exposure, error)
	DeleteExposure(ctx context.Context, id string) error
	NetworkMembers(ctx context.Context, name string) ([]orchapi.NetworkMember, error)
}
