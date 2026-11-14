package mvmd

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func TestFail_MapsMvmdsOutcomes(t *testing.T) {
	g := New("/nonexistent")
	cases := map[codes.Code]error{
		codes.NotFound:           ErrNotFound,
		codes.AlreadyExists:      ErrConflict,
		codes.InvalidArgument:    ErrInvalid,
		codes.FailedPrecondition: ErrPrecondition,
		codes.Unavailable:        ErrUnavailable,
	}
	for code, want := range cases {
		err := g.fail(status.Error(code, "reason"))
		assert.ErrorIs(t, err, want, code.String())
	}
	assert.NoError(t, g.fail(nil))
	other := g.fail(status.Error(codes.Internal, "boom"))
	for _, e := range cases {
		assert.False(t, errors.Is(other, e), "an internal failure is none of the named outcomes")
	}
}

func TestMachineFromProto_CarriesTheFence(t *testing.T) {
	lapsed := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	m := machineFromProto(&v1alpha2.Machine{
		Id: "web", State: v1alpha2.MachineState_MACHINE_STATE_STOPPED, Fenced: true, FenceLapsedAt: timestamppb.New(lapsed),
		LastFailureReason: "the Machine's own fence lapsed and ended its VMM",
	})
	assert.True(t, m.Fenced)
	assert.True(t, m.FenceLapsedAt.Equal(lapsed))
	assert.Contains(t, m.Failure, "own fence lapsed")

	plain := machineFromProto(&v1alpha2.Machine{Id: "web"})
	assert.False(t, plain.Fenced)
	assert.True(t, plain.FenceLapsedAt.IsZero(), "no lapse is the zero time, not the epoch")
}

func TestDeadlineFromProto(t *testing.T) {
	d := deadlineFromProto(&v1alpha2.DeadlineStatus{
		Phase: v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, Length: durationpb.New(time.Minute),
		Remaining: durationpb.New(20 * time.Second), LastRenewalNumber: 7,
	})
	assert.Equal(t, Deadline{Armed: true, Phase: "armed", Length: time.Minute, Remaining: 20 * time.Second, LastNumber: 7}, d)
	assert.Equal(t, "notArmed", deadlineFromProto(&v1alpha2.DeadlineStatus{Phase: v1alpha2.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED}).Phase)
	assert.Equal(t, "expired", deadlineFromProto(&v1alpha2.DeadlineStatus{Phase: v1alpha2.DeadlinePhase_DEADLINE_PHASE_EXPIRED}).Phase)
}
