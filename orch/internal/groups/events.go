package groups

import (
	"context"
	"time"
)

// EventKind names what an event records.
type EventKind string

const (
	EventCreate      EventKind = "create"
	EventDelete      EventKind = "delete"
	EventStop        EventKind = "stop"
	EventStart       EventKind = "start"
	EventRenew       EventKind = "renew-fence"
	EventForget      EventKind = "forget"
	EventHold        EventKind = "hold"
	EventFailed      EventKind = "failed"
	EventObserveFail EventKind = "observe-failed"
	EventPassFailed  EventKind = "pass-failed"
)

// Event is one action taken or one decision made.
type Event struct {
	Time    time.Time
	Kind    EventKind
	Group   string
	Member  string
	Host    string
	Message string
}

// Events records what the engine did and decided.
type Events interface {
	Record(ctx context.Context, e Event) error
}
