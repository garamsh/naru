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

// snapshot takes the snapshot name of the Machine through cl, waits until it is ready, and deletes
// it when the test ends.
func snapshot(t *testing.T, c Config, cl *machineapi.Client, name, machine string) machineapi.Snapshot {
	t.Helper()
	s, created, err := cl.PutSnapshot(t.Context(), name, machineapi.SnapshotRequest{Machine: machine})
	require.NoError(t, err, "snapshot %s", name)
	require.True(t, created, "snapshot %s answered 201", name)
	t.Cleanup(func() { _ = cl.DeleteSnapshot(context.Background(), name) })
	waitSnapshot(t, c, cl, name)
	return s
}

// Snapshots checks a snapshot's idempotency by name: 201 when taken, 200 for the same Machine, 409
// for another, and that a delete removes it.
func Snapshots(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	a, b, name := unique("snaps-a"), unique("snaps-b"), unique("snaps")
	ma := create(t, c.Caller, a, c.Spec)
	create(t, c.Caller, b, c.Spec)
	waitPhase(t, c, c.Caller, a, machineapi.PhaseRunning)
	waitPhase(t, c, c.Caller, b, machineapi.PhaseRunning)

	s := snapshot(t, c, c.Caller, name, a)
	assert.Equal(t, a, s.Machine)
	assert.Equal(t, ma.ID, s.MachineID)
	assert.Equal(t, c.Spec.Volumes, s.Spec.Volumes, "it holds the Machine's volumes")

	_, created, err := c.Caller.PutSnapshot(ctx, name, machineapi.SnapshotRequest{Machine: a})
	require.NoError(t, err, "the same Machine")
	assert.False(t, created, "the same Machine answers 200")
	_, _, err = c.Caller.PutSnapshot(ctx, name, machineapi.SnapshotRequest{Machine: b})
	refused(t, err, http.StatusConflict, machineapi.CodeConflict, "another Machine under the snapshot's name")
	_, _, err = c.Caller.PutSnapshot(ctx, unique("snaps-none"), machineapi.SnapshotRequest{Machine: unique("absent")})
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, "a snapshot of no Machine")

	list, err := c.Caller.Snapshots(ctx)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(list, func(s machineapi.Snapshot) bool { return s.Name == name }), "the list holds it")
	require.NoError(t, c.Caller.DeleteSnapshot(ctx, name))
	_, err = c.Caller.Snapshot(ctx, name)
	refused(t, err, http.StatusNotFound, machineapi.CodeNotFound, "a deleted snapshot")
}

// Restore checks that a create from a snapshot is taken when its volumes match the snapshot's by
// name and size, and refused with 422 otherwise, and that the restored volumes hold what the
// snapshot's Machine wrote to them.
func Restore(t *testing.T, c Config) {
	c = c.check(t)
	ctx := t.Context()
	src, snap := unique("restore-src"), unique("restore")
	create(t, c.Caller, src, c.Spec)
	waitPhase(t, c, c.Caller, src, machineapi.PhaseRunning)
	file := c.Spec.Volumes[0].MountPath + "/restore"
	require.NoError(t, c.Caller.WriteFile(ctx, src, file, []byte("before the snapshot")))
	snapshot(t, c, c.Caller, snap, src)

	from := c.Spec
	from.FromSnapshot = &machineapi.SnapshotSource{Snapshot: snap}
	restored := unique("restored")
	m := create(t, c.Caller, restored, from)
	assert.Equal(t, from.FromSnapshot, m.Spec.FromSnapshot, "the matching volumes are taken")
	waitPhase(t, c, c.Caller, restored, machineapi.PhaseRunning)
	content, err := c.Caller.ReadFile(ctx, restored, file)
	require.NoError(t, err, "the restored volume holds the snapshot's file")
	assert.Equal(t, "before the snapshot", string(content))

	larger := from
	larger.Volumes = append([]machineapi.Volume(nil), from.Volumes...)
	larger.Volumes[0].SizeBytes *= 2
	_, _, err = c.Caller.CreateMachine(ctx, unique("restore-larger"), larger)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, "a volume of another size")

	more := from
	more.Volumes = append(append([]machineapi.Volume(nil), from.Volumes...), machineapi.Volume{Name: "conformance-extra", SizeBytes: from.Volumes[0].SizeBytes, MountPath: "/conformance-extra"})
	_, _, err = c.Caller.CreateMachine(ctx, unique("restore-more"), more)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, "a volume the snapshot does not hold")

	fewer := from
	fewer.Volumes = from.Volumes[1:]
	_, _, err = c.Caller.CreateMachine(ctx, unique("restore-fewer"), fewer)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, "a snapshot's volume the spec leaves out")

	absent := c.Spec
	absent.FromSnapshot = &machineapi.SnapshotSource{Snapshot: unique("absent")}
	_, _, err = c.Caller.CreateMachine(ctx, unique("restore-absent"), absent)
	refused(t, err, http.StatusUnprocessableEntity, machineapi.CodeSnapshotNotFound, "a snapshot the caller has not")
}
