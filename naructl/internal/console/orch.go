package console

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Orch is what the console reads of orch and the one thing it writes. *orchapi.Client implements it.
type Orch interface {
	Hosts(ctx context.Context) ([]orchapi.Host, error)
	Pending(ctx context.Context) ([]orchapi.Pending, error)
	// Events lists the log, oldest first.
	Events(ctx context.Context) ([]orchapi.Event, error)
	// Admit posts an operator's admission of a pending host.
	Admit(ctx context.Context, req orchapi.AdmitRequest) (orchapi.Host, error)
}

// Fleet is what the console shows of the microVMs and groups. A Server built without one says so on
// those pages.
type Fleet interface {
	MicroVMs(ctx context.Context, hostID string) ([]MicroVM, error)
	Groups(ctx context.Context) ([]Group, error)
}
