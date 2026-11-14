package linux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/ext4/ext4test"
)

// xfsSuperblock is the first sector of an XFS file system of blocks 4 KiB blocks.
func xfsSuperblock(label string, blocks uint64) []byte {
	b := make([]byte, 512)
	copy(b, "XFSB")
	binary.BigEndian.PutUint32(b[4:], 4096)
	binary.BigEndian.PutUint64(b[8:], blocks)
	copy(b[108:120], label)
	b[127] = 25
	return b
}

// disk returns a file of size bytes that begins with head.
func disk(t *testing.T, size int64, head []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk")
	f, err := os.Create(p)
	require.NoError(t, err)
	_, err = f.WriteAt(head, 0)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
	return p
}

type growCall struct {
	dir     string
	blocks  uint64
	imaxPct uint8
}

// storageRig is a Host whose mkfs.xfs and grow ioctl are recorded. A mkfs run writes the
// superblock of a file system of the whole device, as the real one does.
type storageRig struct {
	host   *Host
	mounts *[]mountCall
	mkfs   [][]string
	grows  []growCall
	logs   *bytes.Buffer
	log    *slog.Logger
	mnt    string
	failFS error
}

func newStorageRig(t *testing.T, devices map[string]string) *storageRig {
	t.Helper()
	h, mounts := testHost(t, devices)
	r := &storageRig{host: h, mounts: mounts, logs: &bytes.Buffer{}, mnt: filepath.Join(t.TempDir(), "mnt")}
	r.log = slog.New(slog.NewTextHandler(r.logs, nil))
	h.run = func(name string, args ...string) ([]byte, error) {
		r.mkfs = append(r.mkfs, append([]string{name}, args...))
		if r.failFS != nil {
			return []byte("mkfs.xfs: boom"), r.failFS
		}
		dev := args[len(args)-1]
		st, err := os.Stat(dev)
		require.NoError(t, err)
		f, err := os.OpenFile(dev, os.O_WRONLY, 0)
		require.NoError(t, err)
		_, err = f.WriteAt(xfsSuperblock(args[3], uint64(st.Size())/4096), 0)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return nil, nil
	}
	h.grow = func(dir string, blocks uint64, imaxPct uint8) error {
		r.grows = append(r.grows, growCall{dir, blocks, imaxPct})
		return nil
	}
	return r
}

func (r *storageRig) prepare(s config.Storage) error {
	s.MountPoint = r.mnt
	return r.host.PrepareStorage(s, r.log)
}

func TestHost_PrepareStorageFormatsABlankDiskMountsItAndGrowsNothing(t *testing.T) {
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"/bin/mkfs.xfs", "-m", "reflink=1,crc=1", "-L", "vmdisks", filepath.Join(r.host.devDir, "sdb")}}, r.mkfs)
	assert.Equal(t, []mountCall{{filepath.Join(r.host.devDir, "sdb"), r.mnt, "xfs"}}, *r.mounts)
	assert.Empty(t, r.grows, "a file system made to fit the device needs no growth")
	assert.Contains(t, r.logs.String(), "formatted")
	assert.Contains(t, r.logs.String(), "mounted")
}

func TestHost_PrepareStorageNeverReformatsAnXFSDisk(t *testing.T) {
	dev := disk(t, 64<<20, xfsSuperblock("vmdisks", 64<<20/4096))
	r := newStorageRig(t, map[string]string{"sdb": dev})

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.NoError(t, err)
	assert.Empty(t, r.mkfs)
	assert.Len(t, *r.mounts, 1)
	assert.NotContains(t, r.logs.String(), "formatted")
}

func TestHost_PrepareStorageGrowsTheFileSystemToALargerDevice(t *testing.T) {
	dev := disk(t, 128<<20, xfsSuperblock("vmdisks", 64<<20/4096))
	r := newStorageRig(t, map[string]string{"sdb": dev})

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.NoError(t, err)
	assert.Equal(t, []growCall{{r.mnt, 128 << 20 / 4096, 25}}, r.grows)
	assert.Contains(t, r.logs.String(), "grown")
}

func TestHost_PrepareStorageLeavesADiskThatIsNotXFSAlone(t *testing.T) {
	tests := map[string]string{
		"ext4":          ext4test.Image(t, "other", "", nil),
		"unknown bytes": disk(t, 4<<20, []byte("not a file system")),
		"data after 1 MiB": func() string {
			p := disk(t, 4<<20, nil)
			f, err := os.OpenFile(p, os.O_WRONLY, 0)
			require.NoError(t, err)
			_, err = f.WriteAt([]byte{1}, blankProbe-1)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			return p
		}(),
	}
	for name, dev := range tests {
		t.Run(name, func(t *testing.T) {
			before, err := os.ReadFile(dev)
			require.NoError(t, err)
			r := newStorageRig(t, map[string]string{"sdb": dev})

			err = r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

			require.Error(t, err)
			assert.Empty(t, r.mkfs)
			assert.Empty(t, *r.mounts)
			after, rerr := os.ReadFile(dev)
			require.NoError(t, rerr)
			assert.True(t, bytes.Equal(before, after), "the disk is untouched")
		})
	}
}

func TestHost_PrepareStorageRefusesAMissingDisk(t *testing.T) {
	r := newStorageRig(t, nil)

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.ErrorIs(t, err, errNoDisk)
	assert.Empty(t, r.mkfs)
	assert.Empty(t, *r.mounts)
}

func TestHost_PrepareStorageMountsNothingOverAFilledDirectoryAndFormatsNothing(t *testing.T) {
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})
	require.NoError(t, os.MkdirAll(r.mnt, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(r.mnt, "a.img"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(r.mnt, "b.img"), []byte("x"), 0o644))

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 entries")
	assert.Empty(t, r.mkfs)
	assert.Empty(t, *r.mounts)
}

func TestHost_PrepareStorageStopsWhenMkfsFails(t *testing.T) {
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})
	r.failFS = errors.New("exit status 1")

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "boom")
	assert.Empty(t, *r.mounts)
}

func TestHost_PrepareStorageFindsADiskByXFSLabelOrByPartitionName(t *testing.T) {
	byFS := disk(t, 64<<20, xfsSuperblock("vmdisks", 64<<20/4096))
	other := disk(t, 64<<20, xfsSuperblock("elsewhere", 64<<20/4096))
	part := disk(t, 64<<20, nil)

	t.Run("an XFS label", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"sdb": other, "sdc": byFS})

		require.NoError(t, r.prepare(config.Storage{Name: "x", Label: "vmdisks"}))

		assert.Equal(t, filepath.Join(r.host.devDir, "sdc"), (*r.mounts)[0].source)
	})
	t.Run("a partition name", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"sdb": other})
		require.NoError(t, os.Remove(filepath.Join(r.host.sysClassBlock, "sdb")))
		require.NoError(t, os.MkdirAll(filepath.Join(r.host.sysClassBlock, "sdb"), 0o755))
		require.NoError(t, os.Symlink(part, filepath.Join(r.host.devDir, "sdb1")))
		require.NoError(t, os.MkdirAll(filepath.Join(r.host.sysClassBlock, "sdb1"), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(r.host.sysClassBlock, "sdb1", "uevent"), []byte("DEVTYPE=partition\nPARTNAME=vmdisks\n"), 0o644))

		require.NoError(t, r.prepare(config.Storage{Name: "x", Label: "vmdisks"}))

		assert.Equal(t, filepath.Join(r.host.devDir, "sdb1"), (*r.mounts)[0].source)
		assert.Len(t, r.mkfs, 1, "the blank partition is formatted")
	})
	t.Run("no disk with the label", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"sdb": other})

		err := r.prepare(config.Storage{Name: "x", Label: "vmdisks"})

		require.ErrorIs(t, err, errNoDisk)
	})
}

func TestHost_PrepareStorageFindsAnEntryGivenByDeviceByItsLabelWhereverTheKernelPutIt(t *testing.T) {
	xfsDisk := func() string { return disk(t, 64<<20, xfsSuperblock("vmdisks", 64<<20/4096)) }

	t.Run("the disk moved to another name and its old name is the data disk", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"sdb": ext4test.Image(t, "naru-data", "", nil), "sdc": xfsDisk()})

		require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")}))

		assert.Equal(t, []mountCall{{filepath.Join(r.host.devDir, "sdc"), r.mnt, "xfs"}}, *r.mounts)
		assert.Empty(t, r.mkfs)
	})
	t.Run("the disk moved to a name before the one given", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"sdb": xfsDisk(), "sdc": disk(t, 64<<20, nil)})

		require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdc")}))

		assert.Equal(t, filepath.Join(r.host.devDir, "sdb"), (*r.mounts)[0].source)
		assert.Empty(t, r.mkfs, "the blank disk at the given name is not formatted while an XFS carries the name")
	})
	t.Run("a swapped name that points at the data disk is not used", func(t *testing.T) {
		data := ext4test.Image(t, "naru-data", "", nil)
		before, err := os.ReadFile(data)
		require.NoError(t, err)
		r := newStorageRig(t, map[string]string{"sdb": data})

		err = r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

		require.Error(t, err)
		assert.Empty(t, r.mkfs)
		assert.Empty(t, *r.mounts)
		after, rerr := os.ReadFile(data)
		require.NoError(t, rerr)
		assert.True(t, bytes.Equal(before, after), "the data disk is untouched")
	})
	t.Run("a blank disk at the given name is formatted when no XFS carries the name", func(t *testing.T) {
		other := disk(t, 64<<20, xfsSuperblock("elsewhere", 64<<20/4096))
		r := newStorageRig(t, map[string]string{"sdb": disk(t, 64<<20, nil), "sdc": other})

		require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")}))

		assert.Len(t, r.mkfs, 1)
		assert.Equal(t, filepath.Join(r.host.devDir, "sdb"), (*r.mounts)[0].source)
	})
}

func TestHost_PrepareStorageLeavesAnXFSLabelledWithAnotherNameAloneAtTheGivenDevice(t *testing.T) {
	dev := disk(t, 64<<20, xfsSuperblock("elsewhere", 64<<20/4096))
	before, err := os.ReadFile(dev)
	require.NoError(t, err)
	r := newStorageRig(t, map[string]string{"sdb": dev})

	err = r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `labelled "elsewhere"`)
	assert.Empty(t, r.mkfs)
	assert.Empty(t, *r.mounts)
	after, rerr := os.ReadFile(dev)
	require.NoError(t, rerr)
	assert.True(t, bytes.Equal(before, after), "the disk is untouched")
}
