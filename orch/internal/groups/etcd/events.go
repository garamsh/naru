package etcd

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
)

// EventPrefix starts the type of every event the engine writes to the log.
const EventPrefix = "group."

// bootSetPrefix starts the kind of the boot set reconciler's events, which are written under their
// own type (bootset.added) and not under EventPrefix.
const bootSetPrefix = "bootset."

// HoldRepeat is how long a hold, or a failure to add a boot set, that repeats is left out of the log
// after it was written. A hold the log shows older than twice this has ended.
const HoldRepeat = 10 * time.Minute

// Appender is the inventory's event log.
type Appender interface {
	AppendEvent(ctx context.Context, ev orchapi.Event) error
}

// Events writes the engine's events to the inventory's log. A hold the engine reports on every
// pass, and a boot set that fails again in the same class on the same host, is written once per
// HoldRepeat.
type Events struct {
	log Appender

	mu   sync.Mutex
	held map[holdKey]time.Time
}

type holdKey struct {
	kind                       groups.EventKind
	group, member, host, class string
}

var _ groups.Events = (*Events)(nil)

// NewEvents returns an adapter that appends to log.
func NewEvents(log Appender) *Events { return &Events{log: log, held: map[holdKey]time.Time{}} }

// Record implements groups.Events.
func (e *Events) Record(ctx context.Context, ev groups.Event) error {
	if (ev.Kind == groups.EventHold || ev.Kind == groups.EventBootSetFailed) && e.repeated(ev) {
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
	subject, typ := ev.Group, EventPrefix+string(ev.Kind)
	if ev.Member != "" {
		subject = ev.Member
	}
	if strings.HasPrefix(string(ev.Kind), bootSetPrefix) {
		subject, typ = ev.Host, string(ev.Kind)
	}
	return e.log.AppendEvent(ctx, orchapi.Event{Time: ev.Time, Type: typ, Subject: subject, Detail: detail})
}

func (e *Events) repeated(ev groups.Event) bool {
	what := ev.Message
	if ev.Class != "" {
		what = ev.Class
	}
	k := holdKey{ev.Kind, ev.Group, ev.Member, ev.Host, what}
	e.mu.Lock()
	defer e.mu.Unlock()
	if at, ok := e.held[k]; ok && ev.Time.Sub(at) < HoldRepeat {
		return true
	}
	// Holds that stopped repeating are dropped so the map does not outgrow the live holds.
	for old, at := range e.held {
		if ev.Time.Sub(at) >= HoldRepeat {
			delete(e.held, old)
		}
	}
	e.held[k] = ev.Time
	return false
}
