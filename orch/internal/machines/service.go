package machines

import (
	"context"
	"time"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
)

// Clock is the time events are dated by.
type Clock interface {
	Now() time.Time
}

// Service is what the Machine API does for a caller, and what the operator API reads of every
// caller's Machines. Every call names the caller, so it reaches only the caller's records.
type Service interface {
	// Create records the Machine name, or answers it as it is when it has the same spec, State aside.
	// created is true when this call recorded it.
	Create(ctx context.Context, caller, name string, spec machineapi.Spec) (m machineapi.Machine, created bool, err error)
	Machine(ctx context.Context, caller, name string) (machineapi.Machine, error)
	Machines(ctx context.Context, caller string) (machineapi.MachineList, error)
	// Start and Stop set the desired state; a repeat changes nothing.
	Start(ctx context.Context, caller, name string) (machineapi.Machine, error)
	Stop(ctx context.Context, caller, name string, r machineapi.StopRequest) (machineapi.Machine, error)
	// Delete marks the Machine deleting. The reconciler removes it once its volumes are gone.
	Delete(ctx context.Context, caller, name string) (machineapi.Machine, error)
	// PutSnapshot records the snapshot name of r.Machine, or answers it as it is when it is of the
	// same Machine. created is true when this call recorded it.
	PutSnapshot(ctx context.Context, caller, name string, r machineapi.SnapshotRequest) (s machineapi.Snapshot, created bool, err error)
	Snapshot(ctx context.Context, caller, name string) (machineapi.Snapshot, error)
	Snapshots(ctx context.Context, caller string) ([]machineapi.Snapshot, error)
	DeleteSnapshot(ctx context.Context, caller, name string) error
	// Exec runs a command in the running Machine's guest, and ReadFile and WriteFile read and
	// replace one of its files, whole, through the Machine's host.
	Exec(ctx context.Context, caller, name string, r machineapi.ExecRequest) (machineapi.ExecResult, error)
	ReadFile(ctx context.Context, caller, name, path string) ([]byte, error)
	WriteFile(ctx context.Context, caller, name, path string, content []byte) error
	// Events returns the caller's events after the cursor since, the whole log when since is empty.
	Events(ctx context.Context, caller, since string) (machineapi.EventPage, error)
	// All is every caller's Machines, and OperatorMachine one, for the operator API.
	All(ctx context.Context) ([]orchapi.Machine, error)
	OperatorMachine(ctx context.Context, caller, name string) (orchapi.Machine, error)
	// Move records a move of the caller's Machine, by the operator's subject or ByDrain, which the
	// reconciler carries out; a repeat of the move in progress answers it. CancelMove abandons the
	// move before its commit.
	Move(ctx context.Context, by, caller, name string, req orchapi.MoveRequest) (orchapi.Machine, error)
	CancelMove(ctx context.Context, caller, name string) (orchapi.Machine, error)
}

// retries bounds how often a write reads again after another write got in first.
const retries = 8

type service struct {
	store Store
	clock Clock
	// zone is the zone of the stable names, such as "m.garam.sh".
	zone string
	// guests reaches the Machines' guests; nil on a member that reaches no host.
	guests Guests
}

// NewService returns the Service over store, giving each Machine its stable name under zone and
// reaching its guest through guests, which may be nil on a member that reaches no host.
func NewService(store Store, clock Clock, zone string, guests Guests) Service {
	return &service{store: store, clock: clock, zone: zone, guests: guests}
}

// event is the record's status as an event of its caller's log.
func (s *service) event(typ machineapi.EventType, r Record) machineapi.Event {
	return machineapi.Event{Type: typ, Machine: r.Name, ID: r.ID, Status: r.Status, Time: s.clock.Now().UTC()}
}
