package groupsvc

import "time"

// Clock is the service's time.
type Clock interface {
	Now() time.Time
}
