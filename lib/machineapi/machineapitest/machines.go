package machineapitest

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

// Create checks create's idempotency by name: 201 when it makes the Machine, 200 for the same
// spec with State left out of the comparison, and 409 for another spec, which leaves the Machine as
// it was.
func Create(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	name := unique("create")
	m := create(t, c.Caller, name, c.Spec)
	assert.Equal(t, name, m.Name)
	assert.True(t, machineapi.IsMachineID(m.ID), "the id %q is 16 lowercase hexadecimal digits", m.ID)
	assert.Equal(t, m.ID+"."+c.Zone, m.Status.Address.Name, "the address is the stable name")
	assert.Equal(t, machineapi.StateRunning, m.Spec.State, "an absent state is running")
	assert.Equal(t, machineapi.PhasePending, m.Status.Phase, "a create records the Machine, and placement follows")
	assert.NotZero(t, m.Status.Generation)
	assert.Contains(t, m.Status.Capabilities.Snapshot, machineapi.SnapshotVolumes)
	assert.Contains(t, m.Status.Capabilities.Move, machineapi.MoveCold)

	again, created, err := c.Caller.CreateMachine(ctx, name, c.Spec)
	require.NoError(t, err, "the same spec")
	assert.False(t, created, "the same spec answers 200")
	assert.Equal(t, m.ID, again.ID, "and the same Machine")

	stopped := c.Spec
	stopped.State = machineapi.StateStopped
	again, created, err = c.Caller.CreateMachine(ctx, name, stopped)
	require.NoError(t, err, "the same spec with another state")
	assert.False(t, created, "the state is left out of the comparison")
	assert.Equal(t, machineapi.StateRunning, again.Spec.State, "and create does not change it")

	other := c.Spec
	other.CPU++
	_, _, err = c.Caller.CreateMachine(ctx, name, other)
	refused(t, err, http.StatusConflict, machineapi.CodeConflict, "another spec")
	got, err := c.Caller.Machine(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, c.Spec.CPU, got.Spec.CPU, "the refused spec changed nothing")

	_, _, err = c.Caller.CreateMachine(ctx, "Not_A_Label", c.Spec)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeInvalid, "a name that is not a DNS label")
}

// StartStop checks that start and stop set the desired state, the phase follows it, and a repeat
// changes nothing: neither the generation nor the phase.
func StartStop(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	name := unique("startstop")
	create(t, c.Caller, name, c.Spec)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)

	m, err := c.Caller.StopMachine(ctx, name, machineapi.StopRequest{GraceSeconds: 5})
	require.NoError(t, err)
	assert.Equal(t, machineapi.StateStopped, m.Spec.State)
	m = waitPhase(t, c, c.Caller, name, machineapi.PhaseStopped)
	again, err := c.Caller.StopMachine(ctx, name, machineapi.StopRequest{})
	require.NoError(t, err, "a repeated stop")
	assert.Equal(t, m.Status.Generation, again.Status.Generation, "a repeated stop changes no generation")
	assert.Equal(t, machineapi.PhaseStopped, again.Status.Phase, "nor the phase")

	m, err = c.Caller.StartMachine(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, machineapi.StateRunning, m.Spec.State)
	m = waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	assert.Greater(t, m.Status.Generation, again.Status.Generation, "a start that changes the state is a new generation")
	again, err = c.Caller.StartMachine(ctx, name)
	require.NoError(t, err, "a repeated start")
	assert.Equal(t, m.Status.Generation, again.Status.Generation, "a repeated start changes no generation")
	assert.Equal(t, machineapi.PhaseRunning, again.Status.Phase, "nor the phase")
}

// Delete checks that a delete is accepted, the name is held while the Machine is deleted, the
// Machine's snapshots stay, and the name is free once it is gone.
func Delete(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	name, snap := unique("delete"), unique("delete-snap")
	m := create(t, c.Caller, name, c.Spec)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	_, _, err := c.Caller.PutSnapshot(ctx, snap, machineapi.SnapshotRequest{Machine: name})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Caller.DeleteSnapshot(context.Background(), snap) })
	waitSnapshot(t, c, c.Caller, snap)

	require.NoError(t, c.Caller.DeleteMachine(ctx, name))
	_, _, err = c.Caller.CreateMachine(ctx, name, c.Spec)
	refused(t, err, http.StatusConflict, machineapi.CodeDeleting, "a create while the Machine is deleted")

	waitGone(t, c, c.Caller, name)
	_, err = c.Caller.Snapshot(ctx, snap)
	require.NoError(t, err, "the Machine's snapshot outlives it")
	err = c.Caller.DeleteMachine(ctx, name)
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "a delete of a Machine that is gone")

	again := create(t, c.Caller, name, c.Spec)
	assert.NotEqual(t, m.ID, again.ID, "the name, once free, makes a new Machine")
}

// Events checks that a list's cursor and the events after it miss nothing and repeat nothing: each
// change of a Machine created after the list, its deletion included, is read once.
func Events(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	list, err := c.Caller.Machines(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, list.Cursor, "a list carries a cursor")

	name := unique("events")
	m := create(t, c.Caller, name, c.Spec)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	first := of(t, c.Caller, list.Cursor, m.ID)
	require.NotEmpty(t, first.Events, "the events after the list's cursor hold the new Machine's")
	assert.Equal(t, machineapi.PhasePending, first.Events[0].Status.Phase, "the first of them is the create, which records the Machine pending")
	assert.Equal(t, machineapi.PhaseRunning, last(first).Status.Phase, "the last of them is the phase it reached")
	for _, e := range first.Events {
		assert.Equal(t, name, e.Machine)
		assert.Equal(t, machineapi.EventStatus, e.Type)
		assert.Contains(t, []machineapi.Phase{machineapi.PhasePending, machineapi.PhaseStarting, machineapi.PhaseRunning}, e.Status.Phase, "a change of a create")
	}
	assert.Empty(t, of(t, c.Caller, first.Cursor, m.ID).Events, "nothing changed after the page's cursor")

	_, err = c.Caller.StopMachine(ctx, name, machineapi.StopRequest{})
	require.NoError(t, err)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseStopped)
	second := of(t, c.Caller, first.Cursor, m.ID)
	require.NotEmpty(t, second.Events, "the stop is read from the page's cursor")
	assert.Equal(t, machineapi.PhaseStopped, last(second).Status.Phase)
	for _, e := range second.Events {
		assert.Contains(t, []machineapi.Phase{machineapi.PhaseStopping, machineapi.PhaseStopped}, e.Status.Phase, "a change of the stop: none before the cursor is read again")
	}

	require.NoError(t, c.Caller.DeleteMachine(ctx, name))
	waitGone(t, c, c.Caller, name)
	third := of(t, c.Caller, second.Cursor, m.ID)
	require.NotEmpty(t, third.Events)
	assert.Equal(t, machineapi.EventDeleted, last(third).Type, "the Machine's end is an event")

	_, err = c.Caller.Events(ctx, "not-a-cursor")
	refused(t, err, http.StatusBadRequest, machineapi.CodeInvalid, "a cursor the log never gave")
}

// of returns the events after since of the Machine id, with the page's cursor.
func of(t *testing.T, cl *machineapi.Client, since, id string) machineapi.EventPage {
	t.Helper()
	p, err := cl.Events(t.Context(), since)
	require.NoError(t, err)
	p.Events = slices.DeleteFunc(p.Events, func(e machineapi.Event) bool { return e.ID != id })
	return p
}

func last(p machineapi.EventPage) machineapi.Event { return p.Events[len(p.Events)-1] }

// Secrets checks that a read never answers an env value or a file's content, while create compares
// the values it was given and the guest has the files.
func Secrets(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	spec := c.Spec
	spec.Env = map[string]string{"CONFORMANCE_TOKEN": "s3cret"}
	spec.Files = []machineapi.File{{Path: "/etc/conformance/token", Mode: 0o600, Content: []byte("s3cret")}}
	name := unique("secrets")
	m := create(t, c.Caller, name, spec)
	got, err := c.Caller.Machine(ctx, name)
	require.NoError(t, err)
	for what, s := range map[string]machineapi.Spec{"create": m.Spec, "read": got.Spec} {
		assert.Equal(t, map[string]string{"CONFORMANCE_TOKEN": machineapi.Redacted}, s.Env, "%s answers the env's names only", what)
		assert.Equal(t, []machineapi.File{{Path: "/etc/conformance/token", Mode: 0o600, SizeBytes: 6}}, s.Files, "%s answers a file's size, not its content", what)
	}

	_, created, err := c.Caller.CreateMachine(ctx, name, spec)
	require.NoError(t, err, "the same values")
	assert.False(t, created)
	changed := spec
	changed.Env = map[string]string{"CONFORMANCE_TOKEN": "other"}
	_, _, err = c.Caller.CreateMachine(ctx, name, changed)
	refused(t, err, http.StatusConflict, machineapi.CodeConflict, "another env value under the same name")

	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	content, err := c.Caller.ReadFile(ctx, name, "/etc/conformance/token")
	require.NoError(t, err)
	assert.Equal(t, "s3cret", string(content), "the guest has the file")
}

// RunningOnly checks that exec and the file calls are refused while a Machine does not run, and
// answer once it does.
func RunningOnly(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	spec := c.Spec
	spec.State = machineapi.StateStopped
	name := unique("running")
	create(t, c.Caller, name, spec)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseStopped)
	file := c.Spec.Volumes[0].MountPath + "/conformance"
	exec := machineapi.ExecRequest{Command: []string{"cat"}, Stdin: []byte("ping"), TimeoutSeconds: 30}

	_, err := c.Caller.Exec(ctx, name, exec)
	refused(t, err, http.StatusConflict, machineapi.CodeNotRunning, "an exec in a stopped Machine")
	err = c.Caller.WriteFile(ctx, name, file, []byte("hello"))
	refused(t, err, http.StatusConflict, machineapi.CodeNotRunning, "a file write in a stopped Machine")
	_, err = c.Caller.ReadFile(ctx, name, file)
	refused(t, err, http.StatusConflict, machineapi.CodeNotRunning, "a file read in a stopped Machine")

	_, err = c.Caller.StartMachine(ctx, name)
	require.NoError(t, err)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	out, err := c.Caller.Exec(ctx, name, exec)
	require.NoError(t, err, "an exec in the running Machine")
	assert.Equal(t, int32(0), out.ExitCode)
	assert.Equal(t, "ping", string(out.Stdout))
	require.NoError(t, c.Caller.WriteFile(ctx, name, file, []byte("hello")), "a file write in the running Machine")
	content, err := c.Caller.ReadFile(ctx, name, file)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(content))
}
