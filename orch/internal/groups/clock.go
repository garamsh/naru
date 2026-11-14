package groups

import "time"

// Clock is the engine's time.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}
