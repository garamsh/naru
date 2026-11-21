package volumes

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sysDisk makes a disk vdb with a partition vdb1 in a copy of /sys/class/block.
func sysDisk(t *testing.T, mountinfo string) (SysDisks, Disk) {
	t.Helper()
	root := t.TempDir()
	sys := filepath.Join(root, "sys")
	require.NoError(t, os.MkdirAll(filepath.Join(sys, "vdb", "holders"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(sys, "vdb", "vdb1", "holders"), 0o755))
	mi := filepath.Join(root, "mountinfo")
	require.NoError(t, os.WriteFile(mi, []byte(mountinfo), 0o600))
	return SysDisks{Sys: sys, MountInfo: mi}, Disk{Path: "/dev/vdb"}
}

const baseMounts = "22 1 8:1 / / rw - ext4 /dev/vda1 rw\n"

func TestSysDisksInUse_AMountedPartitionOrAHolderMakesADiskBusyAndAFreeOneIsNot(t *testing.T) {
	s, disk := sysDisk(t, baseMounts)
	assert.NoError(t, s.InUse(disk), "the control: nothing uses vdb")

	s, disk = sysDisk(t, baseMounts+"40 22 8:17 / /var/lib/naru rw - xfs /dev/vdb1 rw\n")
	assert.ErrorContains(t, s.InUse(disk), "/var/lib/naru", "a mounted partition")

	s, disk = sysDisk(t, baseMounts+"41 22 8:16 / /mnt rw - xfs /dev/vdb rw\n")
	assert.ErrorContains(t, s.InUse(disk), "/mnt", "the mounted disk itself")

	s, disk = sysDisk(t, baseMounts)
	require.NoError(t, os.WriteFile(filepath.Join(s.Sys, "vdb", "vdb1", "holders", "dm-0"), nil, 0o600))
	assert.ErrorContains(t, s.InUse(disk), "dm-0", "a device mapped over a partition")

	s, disk = sysDisk(t, baseMounts)
	require.NoError(t, os.WriteFile(filepath.Join(s.Sys, "vdb", "holders", "md0"), nil, 0o600))
	assert.ErrorContains(t, s.InUse(disk), "md0", "a device mapped over the disk")
}
