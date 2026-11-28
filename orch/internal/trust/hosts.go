package trust

import (
	"context"
	"time"
)

// Host is when an admitted host's pool stops trusting an authority, and which, as the host reports
// it.
type Host struct {
	ID        string
	ExpiresAt time.Time
	Authority string
}

// Hosts is what a Watcher reads of the admitted hosts: each that reports when its pool ends.
type Hosts interface {
	Trust(ctx context.Context) ([]Host, error)
}
