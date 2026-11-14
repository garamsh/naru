//go:build qemu_real

package tests_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
)

// Criterion "host: snapshot and resume". naructl has no snapshot command, so the test calls the host
// API through lib/hostapi's client over the same client files naructl uses.
func TestNode_HostAPISnapshotsARunningMicroVMAndResumesItIntoANewOneAlsoAfterTheSourceIsDeleted(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	prov, reader := c.Host.Provisioner, c.Host.Reader
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "started", "service=naru-hostd"))
	provisioner, err := hostcli.New(prov)
	require.NoError(t, err)
	readerClient, err := hostcli.New(reader)
	require.NoError(t, err)
	ctx := context.Background()

	made := hctl(prov, []string{"microvm", "create"}, "-f", requestFile(t, "src", false))
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists src Running", func() bool { return strings.Contains(machines(nd), "src=RUNNING") })

	// A reader may list snapshots and may not take one.
	_, err = readerClient.SnapshotMicroVM(ctx, "src", hostapi.SnapshotRequest{Name: "denied"})
	var apiErr *hostapi.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 403, apiErr.Status)

	snap, err := provisioner.SnapshotMicroVM(ctx, "src", hostapi.SnapshotRequest{Name: "snap1"})
	require.NoError(t, err)
	assert.Equal(t, "snap1", snap.Name)
	assert.Equal(t, "src", snap.MicroVM)
	assert.Equal(t, "running", microVMState(t, reader, "src"), "the microVM stays as it was")

	listed, err := readerClient.Snapshots(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, "snap1", listed[0].Name)

	// Resume while the source still exists.
	vm, err := provisioner.ResumeSnapshot(ctx, "snap1", hostapi.ResumeRequest{Name: "copy1"})
	require.NoError(t, err)
	assert.Equal(t, "copy1", vm.Name)
	eventually(t, "mvmd lists copy1 Running", func() bool { return strings.Contains(machines(nd), "copy1=RUNNING") })

	// Resume after the source is deleted: the snapshot outlives it.
	require.NoError(t, provisioner.DeleteMicroVM(ctx, "src"))
	eventually(t, "src is gone", func() bool { return !strings.Contains(machines(nd), "src=") })
	vm, err = provisioner.ResumeSnapshot(ctx, "snap1", hostapi.ResumeRequest{Name: "copy2"})
	require.NoError(t, err)
	assert.Equal(t, "copy2", vm.Name)
	eventually(t, "mvmd lists copy2 Running", func() bool { return strings.Contains(machines(nd), "copy2=RUNNING") })
	assert.Equal(t, "running", microVMState(t, reader, "copy1"))

	require.NoError(t, provisioner.DeleteSnapshot(ctx, "snap1"))
	listed, err = readerClient.Snapshots(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed)
	_, err = provisioner.ResumeSnapshot(ctx, "snap1", hostapi.ResumeRequest{Name: "copy3"})
	require.ErrorAs(t, err, &apiErr, "a deleted snapshot cannot be resumed")
}
