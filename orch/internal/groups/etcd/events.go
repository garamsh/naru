package etcd

import (
	"context"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

// EventPrefix starts the type of every event the engine writes to the log.
const EventPrefix = "group."

// holdRepeat is how long a hold that repeats is left out of the log after it was written.
const holdRepeat = 10 * time.Minute

// Appender is the inventory's event log.
type Appender interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}

// Events writes the engine's events to the inventory's log. A hold the engine reports on every
// pass is written once per holdRepeat.
type Events struct {
	log Appender

	mu   sync.Mutex
	held map[holdKey]time.Time
}

type holdKey struct{ group, member, message string }

var _ groups.Events = (*Events)(nil)

// NewEvents returns an adapter that appends to log.
func NewEvents(log Appender) *Events { return &Events{log: log, held: map[holdKey]time.Time{}} }

// Record implements groups.Events.
func (e *Events) Record(ctx context.Context, ev groups.Event) error {
	if ev.Kind == groups.EventHold && e.repeated(ev) {
		return nil
	}
	detail := map[string]string{"message": ev.Message}
	if ev.Group != "" {
		detail["group"] = ev.Group
	}
	if ev.Member != "" {
		detail["member"] = ev.Member
	}
	if ev.Host != "" {
		detail["host"] = ev.Host
	}
	subject := ev.Group
	if ev.Member != "" {
		subject = ev.Member
	}
	return e.log.AppendEvent(ctx, orchapi.Event{Time: ev.Time, Type: EventPrefix + string(ev.Kind), Subject: subject, Detail: detail})
}

func (e *Events) repeated(ev groups.Event) bool {
	k := holdKey{ev.Group, ev.Member, ev.Message}
	e.mu.Lock()
	defer e.mu.Unlock()
	if at, ok := e.held[k]; ok && ev.Time.Sub(at) < holdRepeat {
		return true
	}
	// Holds that stopped repeating are dropped so the map does not outgrow the live holds.
	for old, at := range e.held {
		if ev.Time.Sub(at) >= holdRepeat {
			delete(e.held, old)
		}
	}
	e.held[k] = ev.Time
	return false
}
