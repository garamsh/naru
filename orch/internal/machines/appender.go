package machines

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Appender is the fleet's event log, where the reconciler records a restart and an older epoch it
// stopped.
type Appender interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}
