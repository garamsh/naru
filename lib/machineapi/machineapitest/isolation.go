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

// Isolation checks that another caller's Machine, snapshot and alias are not found (404, not 403),
// appear in no list or event of the caller's, and that a name is the caller's own: the other caller
// creating the same name makes a Machine of its own.
func Isolation(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	name, snap, host := unique("isolation"), unique("isolation-snap"), unique("isolation")+"."+c.Zone
	since, err := c.Other.Machines(ctx)
	require.NoError(t, err)
	m := create(t, c.Caller, name, c.Spec)
	waitPhase(t, c, c.Caller, name, machineapi.PhaseRunning)
	snapshot(t, c, c.Caller, snap, name)
	_, _, err = c.Caller.PutAlias(ctx, host, machineapi.AliasRequest{Machine: name})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Caller.DeleteAlias(context.Background(), host) })
	file := c.Spec.Volumes[0].MountPath + "/isolation"
	require.NoError(t, c.Caller.WriteFile(ctx, name, file, []byte("mine")))

	for what, call := range map[string]func(cl *machineapi.Client) error{
		"read": func(cl *machineapi.Client) error { _, err := cl.Machine(ctx, name); return err },
		"exec": func(cl *machineapi.Client) error {
			_, err := cl.Exec(ctx, name, machineapi.ExecRequest{Command: []string{"cat"}})
			return err
		},
		"file read":     func(cl *machineapi.Client) error { _, err := cl.ReadFile(ctx, name, file); return err },
		"snapshot read": func(cl *machineapi.Client) error { _, err := cl.Snapshot(ctx, snap); return err },
		"alias read":    func(cl *machineapi.Client) error { _, err := cl.Alias(ctx, host); return err },
	} {
		require.NoError(t, call(c.Caller), "the caller's own %s", what)
		refused(t, call(c.Other), http.StatusNotFound, machineapi.CodeNotFound, "another caller's "+what)
	}
	_, err = c.Other.StartMachine(ctx, name)
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "another caller's start")
	_, err = c.Other.StopMachine(ctx, name, machineapi.StopRequest{})
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "another caller's stop")
	refused(t, c.Other.WriteFile(ctx, name, file, []byte("theirs")), http.StatusNotFound, machineapi.CodeNotFound, "another caller's file write")
	refused(t, c.Other.DeleteMachine(ctx, name), http.StatusNotFound, machineapi.CodeNotFound, "another caller's delete")
	refused(t, c.Other.DeleteSnapshot(ctx, snap), http.StatusNotFound, machineapi.CodeNotFound, "another caller's snapshot delete")
	refused(t, c.Other.DeleteAlias(ctx, host), http.StatusNotFound, machineapi.CodeNotFound, "another caller's alias delete")
	_, _, err = c.Other.PutSnapshot(ctx, unique("isolation-theirs"), machineapi.SnapshotRequest{Machine: name})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "a snapshot of another caller's Machine")

	list, err := c.Other.Machines(ctx)
	require.NoError(t, err)
	assert.False(t, slices.ContainsFunc(list.Machines, func(x machineapi.Machine) bool { return x.ID == m.ID }), "another caller's list holds no Machine of the caller's")
	snaps, err := c.Other.Snapshots(ctx)
	require.NoError(t, err)
	assert.False(t, slices.ContainsFunc(snaps, func(x machineapi.Snapshot) bool { return x.MachineID == m.ID }), "nor a snapshot")
	assert.Empty(t, of(t, c.Other, since.Cursor, m.ID).Events, "nor an event")

	content, err := c.Caller.ReadFile(ctx, name, file)
	require.NoError(t, err)
	assert.Equal(t, "mine", string(content), "no refused call changed the caller's Machine")
	after, err := c.Caller.Machine(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, machineapi.StateRunning, after.Spec.State, "nor its desired state")

	theirs := create(t, c.Other, name, c.Spec)
	assert.NotEqual(t, m.ID, theirs.ID, "the same name under another caller is another Machine")
	mine, err := c.Caller.Machine(ctx, name)
	require.NoError(t, err)
	assert.Equal(t, m.ID, mine.ID, "and leaves the caller's as it was")
}
