package mvmd

import (
	"context"
	"errors"
	"strings"
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
	assert.ErrorIs(t, g.fail(status.Error(codes.DeadlineExceeded, "reason")), context.DeadlineExceeded)
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

func TestCreateMachineRequest_CarriesTheImageFormsRunConfiguration(t *testing.T) {
	req := createMachineRequest(Create{
		ID: "web", ImageDigest: "sha256:" + strings.Repeat("a", 64),
		Command: []string{"/bin/app"}, Args: []string{"--port", "8080"}, User: "1000:1000", WorkingDir: "/srv",
		Env:   map[string]string{"TOKEN": "s3cret", "A": "1"},
		Files: []File{{Path: "/etc/app.conf", Mode: 0o600, Content: []byte("x")}},
		Disks: []Attachment{{ID: "web.data", MountPath: "/data"}},
	})
	assert.Equal(t, []string{"/bin/app"}, req.GetCommand())
	assert.Equal(t, []string{"--port", "8080"}, req.GetArgs())
	assert.Equal(t, "1000:1000", req.GetUser())
	assert.Equal(t, "/srv", req.GetWorkingDir())
	var env []string
	for _, e := range req.GetEnv() {
		env = append(env, e.GetName()+"="+e.GetValue())
	}
	assert.Equal(t, []string{"A=1", "TOKEN=s3cret"}, env, "every variable, ordered by name")
	if assert.Len(t, req.GetFiles(), 1) {
		f := req.GetFiles()[0]
		assert.Equal(t, "/etc/app.conf", f.GetPath())
		assert.Equal(t, uint32(0o600), f.GetMode())
		assert.Equal(t, []byte("x"), f.GetContent())
	}
	if assert.Len(t, req.GetDisks(), 1) {
		assert.Equal(t, "/data", req.GetDisks()[0].GetMountPath())
	}
}

func TestMachineFromProto_CarriesWhatMvmdReportsOfTheRunConfiguration(t *testing.T) {
	m := machineFromProto(&v1alpha2.Machine{
		Id: "web", EnvNames: []string{"TOKEN"}, WorkingDir: "/srv",
		Files: []*v1alpha2.FileInfo{{Path: "/etc/app.conf", Mode: 0o600, SizeBytes: 1}},
		Disks: []*v1alpha2.DiskAttachment{{DiskId: "web.data", MountPath: "/data"}},
	})
	assert.Equal(t, []string{"TOKEN"}, m.EnvNames)
	assert.Equal(t, "/srv", m.WorkingDir)
	assert.Equal(t, []FileInfo{{Path: "/etc/app.conf", Mode: 0o600, SizeBytes: 1}}, m.Files)
	assert.Equal(t, []Attachment{{ID: "web.data", MountPath: "/data"}}, m.Disks)
}

func TestDiskSnapshotFromProto(t *testing.T) {
	at := time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	d := diskSnapshotFromProto(&v1alpha2.DiskSnapshot{Id: "before", DiskId: "web.data", SizeBytes: 1 << 30, AllocatedBytes: 4096, CreatedAt: timestamppb.New(at)})
	assert.Equal(t, DiskSnapshot{ID: "before", DiskID: "web.data", SizeBytes: 1 << 30, AllocatedBytes: 4096, CreatedAt: at}, d)
}
