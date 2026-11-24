package machines

import (
	"context"
	"fmt"
	"strconv"

	"github.com/garamsh/naru/lib/machineapi"
)

func (s *service) Events(ctx context.Context, caller, since string) (machineapi.EventPage, error) {
	var after *int64
	if since != "" {
		n, err := strconv.ParseInt(since, 10, 64)
		if err != nil || n < 0 {
			return machineapi.EventPage{}, fmt.Errorf("%w: %q", ErrBadCursor, since)
		}
		after = &n
	}
	evs, rev, err := s.store.Events(ctx, caller, after)
	if err != nil {
		return machineapi.EventPage{}, err
	}
	return machineapi.EventPage{Events: evs, Cursor: strconv.FormatInt(rev, 10)}, nil
}
