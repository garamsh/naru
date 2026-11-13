//go:build qemu_real

package tests_test

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The tests of the storage disk (ADR 0027, ADR 0032) give one Node a blank sparse disk besides its
// boot and data disks, the data and extra disks both virtio-blk so that the kernel names them
// /dev/vda and /dev/vdb in the order attached, and spec.storage naming the extra one. The disk is
// mounted where mvmd keeps its disks, and naru-storageprobe reports on it from the Node.
const (
	storageDir  = "/var/lib/naru/mvmd/disks"
	extraSize   = 512 << 20
	grownSize   = 1 << 30
	formatted   = "storage disk formatted as XFS"
	storageMark = "storage disk mounted"
)

func withStorageDisk(c *node.Config) {
	c.ExtraDisk = extraSize
	c.VirtioDisks = true
	c.Storage = []node.Storage{{Name: "vmdisks", Device: "/dev/vdb", MountPoint: storageDir}}
	c.StorageProbe = storageDir
}

var sizeField = regexp.MustCompile(`storageprobe .*size=(\d+)`)

// probedSize is the size, in bytes, the probe last reported on the Node's console.
func probedSize(t *testing.T, nd *node.Node) uint64 {
	t.Helper()
	all := sizeField.FindAllStringSubmatch(nd.Console(), -1)
	require.NotEmpty(t, all, "the probe reported a size")
	n, err := strconv.ParseUint(all[len(all)-1][1], 10, 64)
	require.NoError(t, err)
	return n
}

// restart ends the Node's QEMU, as a power cut does, and starts it
// again on the same disks.
func restart(t *testing.T, nd *node.Node) {
	t.Helper()
	nd.Stop()
	require.NoError(t, nd.Reboot())
}

func TestNode_ABlankExtraDiskIsFormattedAsXFSMountedAndReflinksAFile(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Configure: withStorageDisk})
	nd := c.Nodes[0]

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "fs=xfs", "reflink=ok", "marker=written"))

	assert.True(t, nd.Has(formatted, "device=/dev/vdb"), "init formatted the blank disk")
	assert.True(t, nd.Has(storageMark, "mount="+storageDir), "init mounted it where mvmd keeps its disks")
	assert.False(t, nd.Has("storage file system grown"), "a file system made to fit the disk needs no growth")
	assert.Greater(t, probedSize(t, nd), uint64(extraSize*8/10), "the file system fills the disk, less its log")
}

func TestNode_AnXFSDiskKeepsItsContentAcrossARebootAndIsNotFormattedAgain(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Configure: withStorageDisk})
	nd := c.Nodes[0]
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "reflink=ok", "marker=written"))
	require.True(t, nd.Has(formatted), "the control: the first boot formatted the disk")

	restart(t, nd)

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "fs=xfs", "marker=kept"))
	assert.True(t, nd.Has(storageMark), "the second boot mounted the disk")
	assert.False(t, nd.Has(formatted), "and did not format it")
}

func TestNode_ADiskGrownBetweenBootsGrowsItsFileSystemAtTheNextBoot(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Configure: withStorageDisk})
	nd := c.Nodes[0]
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "marker=written"))
	before := probedSize(t, nd)
	nd.Stop()
	require.NoError(t, os.Truncate(nd.ExtraDisk(), grownSize))

	require.NoError(t, nd.Reboot())

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "marker=kept"))
	assert.True(t, nd.Has("storage file system grown"), "init grew the file system")
	assert.False(t, nd.Has(formatted), "without formatting it")
	after := probedSize(t, nd)
	assert.Greater(t, after, before, "the file system is larger")
	assert.Greater(t, after, uint64(grownSize*9/10), "and fills the larger disk")
}

func TestNode_AStorageDiskIsFoundByItsLabelWhenTheDisksSwapNamesBetweenBoots(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Configure: withStorageDisk})
	nd := c.Nodes[0]
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "fs=xfs", "marker=written"))
	require.True(t, nd.Has(formatted, "device=/dev/vdb"), "the control: the first boot formatted the extra disk at the name the entry gives")
	require.True(t, nd.Has(storageMark, "device=/dev/vdb"))
	nd.Stop()
	nd.SwapDisks()

	require.NoError(t, nd.Reboot())

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "storageprobe", "fs=xfs", "marker=kept"))
	assert.True(t, nd.Has(storageMark, "device=/dev/vda"), "init mounted the extra disk where the kernel now names it")
	assert.False(t, nd.Has(formatted), "and formatted nothing, the data disk now at /dev/vdb included")
	assert.False(t, nd.Has("storage disk not prepared"), "and did not refuse the entry")
}
