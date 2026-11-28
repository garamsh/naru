package trust

import "time"

// Clock is the Watcher's clock.
type Clock interface {
	Now() time.Time
}
