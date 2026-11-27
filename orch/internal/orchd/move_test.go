package orchd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/machines"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// orchStatus is the status of the operator API's refusal.
func orchStatus(t *testing.T, err error) int {
	t.Helper()
	var e *orchapi.Error
	require.ErrorAs(t, err, &e)
	return e.Status
}

func TestOrch_MovesAMachineAnOperatorAsksToAnotherHost(t *testing.T) {
	hosts := orchtest.NewFakeMachineHosts(
		machines.Host{ID: "h-a", Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 64, Memory: 256 << 30, Disk: 4 << 40}},
		machines.Host{ID: "h-b", Region: "kr-seoul", Capacity: hostapi.Resources{CPU: 64, Memory: 128 << 30, Disk: 4 << 40}},
	)
	o := startMachineOrchWith(t, orchtest.Options{NewMachineHosts: func(inventory.Service) machines.Hosts { return hosts }})
	ctx := t.Context()
	m, _, err := o.garam.CreateMachine(ctx, "web", machineSpec())
	require.NoError(t, err)
	operator := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	reader := o.c.CA.Issue(t, "rita", identity.RoleReader)
	op, read := o.c.Client(t, 0, &operator), o.c.Client(t, 0, &reader)
	require.Eventually(t, func() bool {
		got, err := read.Machine(ctx, "garam", "web")
		return err == nil && got.Status.Phase == machineapi.PhaseRunning
	}, 10*time.Second, 50*time.Millisecond)
	got, err := read.Machine(ctx, "garam", "web")
	require.NoError(t, err)
	require.Equal(t, "h-a", got.Binding.Host)
	assert.Equal(t, machineapi.Redacted, got.Spec.Env["TOKEN"], "with no secret shown")

	_, err = read.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{})
	assert.Equal(t, http.StatusForbidden, orchStatus(t, err), "a reader moves nothing")
	_, err = op.CancelMove(ctx, "garam", "web")
	assert.Equal(t, http.StatusNotFound, orchStatus(t, err), "no move to cancel")
	_, err = op.MoveMachine(ctx, "garam", "nope", orchapi.MoveRequest{})
	assert.Equal(t, http.StatusNotFound, orchStatus(t, err))
	_, err = read.Machine(ctx, "functions", "web")
	assert.Equal(t, http.StatusNotFound, orchStatus(t, err), "the name is the caller's")

	asked, err := op.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{})
	require.NoError(t, err)
	require.NotNil(t, asked.Move)
	assert.Equal(t, "alice", asked.Move.By, "by the operator who asked")
	assert.Equal(t, "h-a", asked.Move.Source)
	require.Eventually(t, func() bool {
		got, err := read.Machine(ctx, "garam", "web")
		return err == nil && got.Move == nil && got.Binding.Host == "h-b" && got.Status.Phase == machineapi.PhaseRunning
	}, 10*time.Second, 50*time.Millisecond, "the Machine runs on h-b")
	assert.Equal(t, "running", hosts.MicroVMs("h-b")["m-"+m.ID+"-2"].State)
	assert.Empty(t, hosts.MicroVMs("h-a"))
	caller, err := o.garam.Machine(ctx, "web")
	require.NoError(t, err)
	assert.Equal(t, m.Status.Address, caller.Status.Address, "the caller's Machine keeps its stable name")
}

func TestOrch_RefusesAMoveOfAMachineNotPlacedAndANarrowingItsShapeContradicts(t *testing.T) {
	o := startMachineOrch(t)
	ctx := t.Context()
	s := machineSpec()
	s.Shape.Region = "kr-seoul"
	_, _, err := o.garam.CreateMachine(ctx, "web", s)
	require.NoError(t, err)
	operator := o.c.CA.Issue(t, "alice", identity.RoleOperator)
	op := o.c.Client(t, 0, &operator)

	_, err = op.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{Shape: &orchapi.MoveShape{Region: "us-east"}})
	assert.Equal(t, http.StatusUnprocessableEntity, orchStatus(t, err))
	_, err = op.MoveMachine(ctx, "garam", "web", orchapi.MoveRequest{})
	assert.Equal(t, http.StatusConflict, orchStatus(t, err), "a Machine on no host has nothing to move")
}
