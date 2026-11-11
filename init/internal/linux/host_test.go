package linux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/ext4/ext4test"
)

type mountCall struct{ source, target, fstype string }

// testHost is a Host over a temporary /sys/class/block, /dev and /proc/sys,
// whose mount records its calls. devices maps a device name to the image or
// file that stands for it.
func testHost(t *testing.T, devices map[string]string) (*Host, *[]mountCall) {
	t.Helper()
	root := t.TempDir()
	h := &Host{
		sysClassBlock: filepath.Join(root, "sys/class/block"),
		devDir:        filepath.Join(root, "dev"),
		procSys:       filepath.Join(root, "proc/sys"),
	}
	for _, d := range []string{h.sysClassBlock, h.devDir, filepath.Join(h.procSys, "net/ipv4"), filepath.Join(h.procSys, "net/ipv6/conf/all")} {
		require.NoError(t, os.MkdirAll(d, 0o755))
	}
	for name, file := range devices {
		require.NoError(t, os.WriteFile(filepath.Join(h.sysClassBlock, name), nil, 0o644))
		require.NoError(t, os.Symlink(file, filepath.Join(h.devDir, name)))
	}
	var calls []mountCall
	h.mount = func(source, target, fstype string, _ uintptr, _ string) error {
		calls = append(calls, mountCall{source, target, fstype})
		return nil
	}
	return h, &calls
}

func notExt4(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "zero")
	require.NoError(t, os.WriteFile(p, make([]byte, 8192), 0o644))
	return p
}

func TestHost_FindsADiskByItsExt4Label(t *testing.T) {
	h, _ := testHost(t, map[string]string{
		"vda": ext4test.Image(t, "OTHER", "", nil),
		"vdb": notExt4(t),
		"vdc": ext4test.Image(t, "NARUCFG", "", map[string]string{"naru.yaml": "x"}),
	})

	dev, err := h.findByLabel("NARUCFG")

	require.NoError(t, err)
	assert.Equal(t, filepath.Join(h.devDir, "vdc"), dev)

	_, err = h.findByLabel("ABSENT")
	assert.ErrorIs(t, err, errNoDisk)
}

func TestHost_MountDataDisk(t *testing.T) {
	verity := ext4test.Image(t, "NARUDATA", "verity", nil)
	plain := ext4test.Image(t, "NARUDATA", "", nil)
	mp := filepath.Join(t.TempDir(), "var/lib/mvmd")

	t.Run("mounts an ext4 disk with verity, found by label", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": verity})

		err := h.MountDataDisk(config.DataDisk{Label: "NARUDATA", MountPoint: mp})

		require.NoError(t, err)
		assert.Equal(t, []mountCall{{filepath.Join(h.devDir, "vda"), mp, "ext4"}}, *calls)
	})

	t.Run("mounts an ext4 disk with verity, named by device", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": verity})
		dev := filepath.Join(h.devDir, "vda")

		require.NoError(t, h.MountDataDisk(config.DataDisk{Device: dev, MountPoint: mp}))

		assert.Equal(t, []mountCall{{dev, mp, "ext4"}}, *calls)
	})

	t.Run("refuses an ext4 disk without verity", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": plain})

		err := h.MountDataDisk(config.DataDisk{Label: "NARUDATA", MountPoint: mp})

		assert.ErrorContains(t, err, "verity")
		assert.Empty(t, *calls)
	})

	t.Run("refuses a disk that is not ext4", func(t *testing.T) {
		h, calls := testHost(t, map[string]string{"vda": notExt4(t)})

		err := h.MountDataDisk(config.DataDisk{Device: filepath.Join(h.devDir, "vda"), MountPoint: mp})

		assert.ErrorContains(t, err, "not an ext4")
		assert.Empty(t, *calls)
	})
}

func TestHost_EnableForwardingWritesOnlyTheNamedFamilies(t *testing.T) {
	h, _ := testHost(t, nil)
	v4 := filepath.Join(h.procSys, "net/ipv4/ip_forward")
	v6 := filepath.Join(h.procSys, "net/ipv6/conf/all/forwarding")
	for _, f := range []string{v4, v6} {
		require.NoError(t, os.WriteFile(f, []byte("0"), 0o644))
	}

	require.NoError(t, h.EnableForwarding([]config.Family{config.IPv6}))

	assert.Equal(t, "0", read(t, v4), "a family not named stays as it was")
	assert.Equal(t, "1", read(t, v6))

	require.NoError(t, h.EnableForwarding([]config.Family{config.IPv4}))
	assert.Equal(t, "1", read(t, v4))
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}
