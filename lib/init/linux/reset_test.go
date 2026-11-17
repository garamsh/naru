package linux

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
)

func TestClearDataDisk_RemovesEverythingButTheStoredConfiguration(t *testing.T) {
	mnt := t.TempDir()
	for _, f := range []string{"config/store.yaml", "hostd/host.key", "mvmd/state/a", "lost+found/x", "reset-me"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(mnt, f)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(mnt, f), []byte("x"), 0o600))
	}
	h := &Host{dataMountPoint: mnt}

	require.NoError(t, h.ClearDataDisk())

	assert.FileExists(t, filepath.Join(mnt, "config/store.yaml"))
	assert.FileExists(t, filepath.Join(mnt, "lost+found/x"))
	assert.NoFileExists(t, filepath.Join(mnt, "hostd/host.key"))
	assert.NoDirExists(t, filepath.Join(mnt, "mvmd"))
	assert.NoFileExists(t, filepath.Join(mnt, "reset-me"))
}

func TestClearDataDisk_RefusesWhenNothingIsMounted(t *testing.T) {
	assert.Error(t, (&Host{}).ClearDataDisk())
}

func TestWipeDataDisk_UnmountsAndZeroesTheFirstAndLastMiB(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "disk")
	disk := bytes.Repeat([]byte{0xAB}, 3*blankProbe)
	require.NoError(t, os.WriteFile(dev, disk, 0o600))
	var unmounted []string
	h := &Host{dataMountPoint: "/var/lib/naru", unmount: func(dir string) error { unmounted = append(unmounted, dir); return nil }}

	require.NoError(t, h.WipeDataDisk(config.DataDisk{Device: dev, MountPoint: "/var/lib/naru"}))

	assert.Equal(t, []string{"/var/lib/naru"}, unmounted)
	got, err := os.ReadFile(dev)
	require.NoError(t, err)
	assert.Len(t, got, len(disk))
	assert.Equal(t, make([]byte, blankProbe), got[:blankProbe])
	assert.Equal(t, make([]byte, blankProbe), got[len(got)-blankProbe:])
	assert.Equal(t, disk[blankProbe:2*blankProbe], got[blankProbe:2*blankProbe], "only the two ends are blanked")
	f, err := os.Open(dev)
	require.NoError(t, err)
	defer f.Close()
	ok, err := isBlankDisk(f)
	require.NoError(t, err)
	assert.True(t, ok, "the disk is blank by the rule that formats a blank disk")
	assert.Empty(t, h.dataMountPoint)
}

func TestWipeDataDisk_DoesNotTouchTheDiskWhenItCannotBeUnmounted(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "disk")
	disk := bytes.Repeat([]byte{0xAB}, 2*blankProbe)
	require.NoError(t, os.WriteFile(dev, disk, 0o600))
	h := &Host{dataMountPoint: "/var/lib/naru", unmount: func(string) error { return os.ErrPermission }}

	assert.Error(t, h.WipeDataDisk(config.DataDisk{Device: dev}))

	got, _ := os.ReadFile(dev)
	assert.Equal(t, disk, got)
}
