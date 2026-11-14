package groupsvc

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Log is the inventory's event log.
type Log interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}
