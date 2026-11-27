package machines_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// served is spec with one HTTP service, which an alias names.
func served() machineapi.Spec {
	s := spec()
	s.Services = []machineapi.Service{{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP}}
	return s
}

func TestPutAlias_RefusesAMachineWithNoServiceOrBeingDeleted(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.create("bare", spec())
	_, _, err := r.svc.PutAlias(ctx, "garam", "bare.m.example.test", machineapi.AliasRequest{Machine: "bare"})
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "a Machine with no service has nothing to route to")

	r.create("web", served())
	_, created, err := r.svc.PutAlias(ctx, "garam", "web.m.example.test", machineapi.AliasRequest{Machine: "web"})
	require.NoError(t, err, "control: a Machine with a service")
	assert.True(t, created)

	r.create("gone", served())
	_, err = r.svc.Delete(ctx, "garam", "gone")
	require.NoError(t, err)
	m, err := r.svc.Machine(ctx, "garam", "gone")
	require.NoError(t, err)
	require.Equal(t, machineapi.PhaseDeleting, m.Status.Phase, "the setup: the Machine is still being deleted")
	_, _, err = r.svc.PutAlias(ctx, "garam", "gone.m.example.test", machineapi.AliasRequest{Machine: "gone"})
	assert.ErrorIs(t, err, machines.ErrMachineNotFound, "an alias of a Machine being deleted would outlive it")
}

func TestPutAlias_RefusesAnAliasPastTheMachinesBound(t *testing.T) {
	r := newRig(t)
	ctx := t.Context()
	r.create("web", served())
	for i := range machines.MaxAliasesPerMachine {
		_, _, err := r.svc.PutAlias(ctx, "garam", fmt.Sprintf("a%d.m.example.test", i), machineapi.AliasRequest{Machine: "web"})
		require.NoError(t, err, "alias %d, within the bound", i)
	}
	_, _, err := r.svc.PutAlias(ctx, "garam", "a0.m.example.test", machineapi.AliasRequest{Machine: "web"})
	require.NoError(t, err, "control: one the Machine holds, asked again, is not one more")
	_, _, err = r.svc.PutAlias(ctx, "garam", "over.m.example.test", machineapi.AliasRequest{Machine: "web"})
	assert.ErrorIs(t, err, machineapi.ErrInvalid, "one past the bound its delete removes in one transaction")
}
