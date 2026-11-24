//go:build qemu_real

package tests_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/hostcli"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

var treeField = regexp.MustCompile(`storagetree (?:service=\S+ )?disks=(\d+) snapshots=(\d+) machines=(\d+)`)

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
	c := start(t, node.ClusterSpec{N: 1, GenNode: true, NodeAPI: true, Host: true, Configure: withStorageDisk})
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

// A host that a release before the links ran left a file in mvmd's snapshots directory on the data
// disk. Init moves it into the storage mount and links the directory, where it used to refuse the
// boot, and nothing an operator could reach cleared the file. The control host is the first boot
// of the same Node, whose snapshots directory is a link; its disk is then changed the way the old
// release left it. Without the move, init refuses and the storage mount never holds the file.
func TestNode_AFileInMvmdsStateSnapshotsDirectoryIsMovedIntoTheStorageMountAndTheDirectoryLinked(t *testing.T) {
	dir := t.TempDir()
	spec := node.ClusterSpec{N: 1, Configure: withStorageDisk}
	spec.Disk, spec.Dir, spec.Inputs, spec.Naructl = filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), dir, inputs, tool
	c, err := node.StartCluster(spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		t.Logf("console of %s:\n%s", c.Nodes[0].Name, c.Nodes[0].Console())
	})
	nd := c.Nodes[0]
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "reflink=ok", "marker=written"))
	require.True(t, nd.Has("linked into storage"), "the control: the first boot linked mvmd's directories")
	nd.Stop()

	data := filepath.Join(dir, nd.Name, "data.img")
	stale := filepath.Join(t.TempDir(), "stale")
	require.NoError(t, os.WriteFile(stale, []byte("left by a crash-looping mvmd"), 0o600))
	script := filepath.Join(t.TempDir(), "debugfs")
	state := strings.TrimPrefix(wire.MvmdStateDir, wire.DataMount+"/")
	require.NoError(t, os.WriteFile(script, []byte("rm "+state+"/snapshots\nmkdir "+state+"/snapshots\nwrite "+stale+" "+state+"/snapshots/stale\n"), 0o600))
	// e2fsck exits 1 when it replayed the journal.
	if out, err := exec.Command("/sbin/e2fsck", "-fy", data).CombinedOutput(); err != nil {
		var ee *exec.ExitError
		require.True(t, errors.As(err, &ee) && ee.ExitCode() <= 1, string(out))
	}
	out, err := exec.Command("/sbin/debugfs", "-w", "-f", script, data).CombinedOutput()
	require.NoError(t, err, string(out))

	require.NoError(t, nd.Reboot())

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "fs=xfs", "marker=kept"))
	assert.True(t, nd.Has("storage entry moved"), "init moved the file")
	assert.True(t, nd.Has("linked into storage"), "and linked the directory")
	assert.False(t, nd.Has("boot refused"), "instead of refusing the boot")
	eventually(t, "the storage mount holds the file", func() bool {
		_, s, _ := onStorage(nd)
		return s >= 1
	})
}
