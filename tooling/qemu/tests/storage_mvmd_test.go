//go:build qemu_real

package tests_test

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

var treeField = regexp.MustCompile(`storagetree disks=(\d+) snapshots=(\d+) machines=(\d+)`)

// onStorage is what naru-storageprobe last counted on the storage disk: disks, snapshots and
// run directories.
func onStorage(nd *node.Node) (disks, snapshots, machines int) {
	all := treeField.FindAllStringSubmatch(nd.Console(), -1)
	if len(all) == 0 {
		return 0, 0, 0
	}
	last := all[len(all)-1]
	disks, _ = strconv.Atoi(last[1])
	snapshots, _ = strconv.Atoi(last[2])
	machines, _ = strconv.Atoi(last[3])
	return disks, snapshots, machines
}

// A Node with a storage disk runs mvmd: state_dir stays on the data disk, and the disks,
// the snapshots and the run directories are on the XFS (ADR 0073). Before that layout mvmd
// refused to start, so no host API call that needs it was answered.
func TestNode_AStorageDiskHoldsMvmdsDisksSnapshotsAndRunDirectoriesWhileAMicroVMIsCreatedSnapshotAndResumed(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Gaon: true, NodeAPI: true, Host: true, Configure: withStorageDisk})
	require.NotNil(t, c.Host)
	nd := c.Nodes[0]
	prov := c.Host.Provisioner
	eventually(t, "the host API answers a reader", func() bool { return hctl(c.Host.Reader, []string{"host"}).Err == nil })
	provisioner, err := hostcli.New(prov)
	require.NoError(t, err)
	require.True(t, nd.Has(storageMark, "mount="+storageDir), "the control: init mounted the storage disk")

	made := hctl(prov, []string{"microvm", "create"}, "-f", requestFile(t, "src", false))
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists src Running", func() bool { return strings.Contains(machines(nd), "src=RUNNING") })
	eventually(t, "the storage disk holds src's disk and run directory", func() bool {
		d, _, m := onStorage(nd)
		return d >= 1 && m >= 1
	})

	_, err = provisioner.SnapshotMicroVM(t.Context(), "src", hostapi.SnapshotRequest{Name: "snap1"})
	require.NoError(t, err)
	eventually(t, "the storage disk holds the snapshot", func() bool {
		_, s, _ := onStorage(nd)
		return s >= 1
	})

	_, err = provisioner.ResumeSnapshot(t.Context(), "snap1", hostapi.ResumeRequest{Name: "copy1"})
	require.NoError(t, err)
	eventually(t, "mvmd lists copy1 Running", func() bool { return strings.Contains(machines(nd), "copy1=RUNNING") })

	assert.Equal(t, "running", microVMState(t, c.Host.Reader, "src"))
	assert.False(t, nd.Has("storage disk not prepared"), "init prepared the disk and its links")
	assert.False(t, nd.Has("different file systems"), "mvmd found its disks and snapshots on one file system")
}
