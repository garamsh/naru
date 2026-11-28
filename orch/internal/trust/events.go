package trust

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Events is where a Watcher appends its events.
type Events interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}
