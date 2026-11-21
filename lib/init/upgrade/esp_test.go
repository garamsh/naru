package upgrade_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/upgrade"
	"github.com/garamsh/naru/lib/wire"
)

// sysfs lays out /sys/class/block as the kernel does: each name is a link into the device tree,
// where a partition lies under its disk.
func sysfs(t *testing.T, disks map[string]map[string]string) string {
	t.Helper()
	root := t.TempDir()
	class := filepath.Join(root, "class/block")
	require.NoError(t, os.MkdirAll(class, 0o755))
	for disk, parts := range disks {
		d := filepath.Join(root, "devices", disk)
		require.NoError(t, os.MkdirAll(d, 0o755))
		require.NoError(t, os.Symlink(d, filepath.Join(class, disk)))
		for part, uuid := range parts {
			p := filepath.Join(d, part)
			require.NoError(t, os.MkdirAll(p, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(p, "uevent"), []byte("DEVTYPE=partition\nPARTUUID="+uuid+"\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(p, "partition"), []byte("1\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(p, "start"), []byte("2048\n"), 0o644))
			require.NoError(t, os.WriteFile(filepath.Join(p, "size"), []byte("1024\n"), 0o644))
			require.NoError(t, os.Symlink(p, filepath.Join(class, part)))
		}
	}
	return class
}

func TestFindESP_FollowsThePartitionGUIDNotTheDiskName(t *testing.T) {
	class := sysfs(t, map[string]map[string]string{
		"sda": {"sda1": "11111111-2222-3333-4444-555555555555"},
		"sdb": {"sdb1": wire.ESPGUID, "sdb2": "66666666-2222-3333-4444-555555555555"},
	})

	part, disk, err := upgrade.FindESP(class, "/dev", wire.ESPGUID)

	require.NoError(t, err)
	assert.Equal(t, "/dev/sdb1", part)
	assert.Equal(t, "/dev/sdb", disk, "the boot disk is the one holding the ESP, not sda")
}

func TestFindESP_FailsWhenNoPartitionHasTheGUID(t *testing.T) {
	class := sysfs(t, map[string]map[string]string{"sda": {"sda1": "11111111-2222-3333-4444-555555555555"}})

	_, _, err := upgrade.FindESP(class, "/dev", wire.ESPGUID)

	assert.ErrorContains(t, err, "no partition has the GUID")
}
