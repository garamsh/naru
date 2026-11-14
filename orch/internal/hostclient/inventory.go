package hostclient

import (
	"context"

	"github.com/garamsh/naru/lib/orchapi"
)

// Inventory lists the admitted hosts.
type Inventory interface {
	Hosts(ctx context.Context) ([]orchapi.Host, error)
}
