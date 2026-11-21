package linux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
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

func TestHost_PrepareStorageMountsEveryStorageDiskWithProjectQuota(t *testing.T) {
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})
	var data []string
	mount := r.host.mount
	r.host.mount = func(source, target, fstype string, flags uintptr, d string) error {
		data = append(data, d)
		return mount(source, target, fstype, flags, d)
	}

	require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")}))

	assert.Equal(t, []string{"prjquota"}, data, "the volume driver sets a limit per directory, which XFS enforces only on a mount with project quota (ADR 0083)")
}

func TestHost_PrepareStorageFormatsABlankDiskMountsItAndGrowsNothing(t *testing.T) {
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})

	err := r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})

	require.NoError(t, err)
	assert.Equal(t, [][]string{{"/bin/mkfs.xfs", "-m", "reflink=1,crc=1", "-L", "vmdisks", filepath.Join(r.host.devDir, "sdb")}}, r.mkfs)
	assert.Equal(t, []mountCall{{filepath.Join(r.host.devDir, "sdb"), r.mnt, "xfs"}}, *r.mounts)
	assert.Empty(t, r.grows, "a file system made to fit the device needs no growth")
	assert.Equal(t, []string{r.mnt}, r.host.storageMounts, "the mount is kept for the shutdown to undo")
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
		"ext4":                             ext4test.Image(t, "other", "", nil),
		"unknown bytes":                    disk(t, 4<<20, []byte("not a file system")),
		"data at the end of the first MiB": diskWithByteAt(t, blankProbe-1),
		"a backup table in the last MiB":   diskWithByteAt(t, 4<<20-1),
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

// withSerial gives the device of the rig, a /sys/class/block entry, the file attrs name, as
// /sys/block/<dev> has for a virtio-blk disk (serial) or a SCSI disk (device/vpd_pg80, device/wwid).
func withSerial(t *testing.T, r *storageRig, dev string, attrs map[string]string) {
	t.Helper()
	entry := filepath.Join(r.host.sysClassBlock, dev)
	require.NoError(t, os.Remove(entry))
	for name, content := range attrs {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(entry, name)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(entry, name), []byte(content), 0o644))
	}
}

func TestHost_PrepareStorageFindsAnEntryGivenBySerialAndFormatsItWhenBlank(t *testing.T) {
	data := ext4test.Image(t, "naru-data", "", nil)
	r := newStorageRig(t, map[string]string{"vda": data, "vdb": disk(t, 64<<20, nil)})
	withSerial(t, r, "vda", map[string]string{"serial": "DATA-0\n"})
	withSerial(t, r, "vdb", map[string]string{"serial": "S3RIAL-01\n"})

	require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"}))

	require.Len(t, r.mkfs, 1)
	assert.Equal(t, []string{mkfsXFS, "-m", "reflink=1,crc=1", "-L", "vmdisks", filepath.Join(r.host.devDir, "vdb")}, r.mkfs[0])
	assert.Equal(t, filepath.Join(r.host.devDir, "vdb"), (*r.mounts)[0].source)
}

func TestHost_PrepareStorageFindsAnEntryGivenBySerialByItsLabelAtTheNextBootWhateverTheOrder(t *testing.T) {
	r := newStorageRig(t, map[string]string{
		"vda": ext4test.Image(t, "naru-data", "", nil),
		"vdb": disk(t, 64<<20, xfsSuperblock("vmdisks", 64<<20/4096)),
	})
	// The serial is on the disk that is now vdb as before, but the entry is found by its label.
	withSerial(t, r, "vdb", map[string]string{"serial": "S3RIAL-01"})

	require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"}))

	assert.Empty(t, r.mkfs)
	assert.Equal(t, filepath.Join(r.host.devDir, "vdb"), (*r.mounts)[0].source)
}

func TestHost_PrepareStorageReadsASCSIDisksSerialFromItsVPDPageOrWWID(t *testing.T) {
	for name, attrs := range map[string]map[string]string{
		"vpd page": {"device/vpd_pg80": "\x00\x80\x00\x09S3RIAL-01"},
		"wwid":     {"device/wwid": "S3RIAL-01\n"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newStorageRig(t, map[string]string{"sdb": disk(t, 64<<20, nil)})
			withSerial(t, r, "sdb", attrs)

			require.NoError(t, r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"}))

			assert.Len(t, r.mkfs, 1)
		})
	}
}

func TestHost_PrepareStorageFormatsNothingWhenNoDiskOrTwoDisksHaveTheSerial(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"vdb": disk(t, 64<<20, nil)})
		withSerial(t, r, "vdb", map[string]string{"serial": "OTHER"})
		r.host.diskWait = 0

		err := r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "S3RIAL-01")
		assert.Empty(t, r.mkfs)
		assert.Empty(t, *r.mounts)
	})
	t.Run("two", func(t *testing.T) {
		r := newStorageRig(t, map[string]string{"vdb": disk(t, 64<<20, nil), "vdc": disk(t, 64<<20, nil)})
		withSerial(t, r, "vdb", map[string]string{"serial": "S3RIAL-01"})
		withSerial(t, r, "vdc", map[string]string{"serial": "S3RIAL-01"})

		err := r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "2 disks")
		assert.Empty(t, r.mkfs)
		assert.Empty(t, *r.mounts)
	})
}

func TestHost_PrepareStorageLeavesAnXFSLabelledWithAnotherNameAloneAtTheSerial(t *testing.T) {
	r := newStorageRig(t, map[string]string{"vdb": disk(t, 64<<20, xfsSuperblock("elsewhere", 64<<20/4096))})
	withSerial(t, r, "vdb", map[string]string{"serial": "S3RIAL-01"})

	err := r.prepare(config.Storage{Name: "vmdisks", Serial: "S3RIAL-01"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `labelled "elsewhere"`)
	assert.Empty(t, r.mkfs)
	assert.Empty(t, *r.mounts)
}

// diskWithByteAt returns a blank 4 MiB disk with one non-zero byte at off.
func diskWithByteAt(t *testing.T, off int64) string {
	t.Helper()
	p := disk(t, 4<<20, nil)
	f, err := os.OpenFile(p, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte{1}, off)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return p
}

// mvmdRig is a storageRig whose mount point is the one mvmd's disks, snapshots and disk snapshots are linked into,
// with a state directory of its own.
func mvmdRig(t *testing.T) (*storageRig, string) {
	t.Helper()
	dev := disk(t, 64<<20, nil)
	r := newStorageRig(t, map[string]string{"sdb": dev})
	state := filepath.Join(t.TempDir(), "mvmd")
	r.host.mvmdStorage, r.host.mvmdState = r.mnt, state
	return r, state
}

// seedStorage makes fn run once the storage disk is mounted, as the mount hides what its mount
// point held before.
func seedStorage(t *testing.T, r *storageRig, fn func()) {
	t.Helper()
	mount := r.host.mount
	r.host.mount = func(source, target, fstype string, flags uintptr, data string) error {
		if err := mount(source, target, fstype, flags, data); err != nil {
			return err
		}
		fn()
		return nil
	}
}

func prepareMvmd(r *storageRig) error {
	return r.prepare(config.Storage{Name: "vmdisks", Device: filepath.Join(r.host.devDir, "sdb")})
}

func TestHost_PrepareStorageLinksMvmdsDisksSnapshotsAndDiskSnapshotsIntoTheMountOfTheStorageDisk(t *testing.T) {
	r, state := mvmdRig(t)

	require.NoError(t, prepareMvmd(r))

	for _, name := range []string{"disks", "snapshots", "disk-snapshots"} {
		got, err := os.Readlink(filepath.Join(state, name))
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(r.mnt, name), got)
		info, err := os.Stat(filepath.Join(state, name))
		require.NoError(t, err, "the link resolves, as MkdirAll of mvmd needs")
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "only the daemon lists it")
	}
	assert.Contains(t, r.logs.String(), "linked")
}

func TestHost_PrepareStorageLinksNothingForADiskMountedElsewhere(t *testing.T) {
	r, state := mvmdRig(t)
	r.host.mvmdStorage = filepath.Join(t.TempDir(), "other")

	require.NoError(t, prepareMvmd(r))

	assert.NoDirExists(t, state)
}

func TestHost_PrepareStorageReplacesAnEmptyDirectoryOfMvmdsStateWithTheLink(t *testing.T) {
	r, state := mvmdRig(t)
	require.NoError(t, os.MkdirAll(filepath.Join(state, "disks"), 0o700))

	require.NoError(t, prepareMvmd(r))

	got, err := os.Readlink(filepath.Join(state, "disks"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(r.mnt, "disks"), got)
}

func TestHost_PrepareStorageMovesTheEntriesOfMvmdsStateDirectoryIntoStorageBeforeLinkingIt(t *testing.T) {
	r, state := mvmdRig(t)
	require.NoError(t, os.MkdirAll(filepath.Join(state, "snapshots", "s1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(state, "snapshots", "s1", "data"), []byte("snap"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(state, "snapshots", "s2"), []byte("two"), 0o600))

	require.NoError(t, prepareMvmd(r))

	got, err := os.ReadFile(filepath.Join(r.mnt, "snapshots", "s1", "data"))
	require.NoError(t, err)
	assert.Equal(t, "snap", string(got))
	assert.FileExists(t, filepath.Join(r.mnt, "snapshots", "s2"))
	link, err := os.Readlink(filepath.Join(state, "snapshots"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(r.mnt, "snapshots"), link)
	assert.FileExists(t, filepath.Join(state, "snapshots", "s2"), "mvmd sees the entry through the link")
	assert.Contains(t, r.logs.String(), "moved")
}

func TestHost_PrepareStorageFinishesAMoveThatWasInterrupted(t *testing.T) {
	r, state := mvmdRig(t)
	snaps, target := filepath.Join(state, "snapshots"), filepath.Join(r.mnt, "snapshots")
	require.NoError(t, os.MkdirAll(snaps, 0o700))
	for _, name := range []string{"half", "done"} {
		require.NoError(t, os.WriteFile(filepath.Join(snaps, name), []byte("whole "+name), 0o600))
	}
	seedStorage(t, r, func() {
		require.NoError(t, os.MkdirAll(target, 0o700))
		// Interrupted while copying: a partial copy under the temporary name.
		require.NoError(t, os.WriteFile(filepath.Join(target, movingPrefix+"half"), []byte("whol"), 0o600))
		// Interrupted after the rename, before the source was removed: both places hold the entry.
		require.NoError(t, os.WriteFile(filepath.Join(target, "done"), []byte("whole done"), 0o600))
	})

	require.NoError(t, prepareMvmd(r))

	for _, name := range []string{"half", "done"} {
		got, err := os.ReadFile(filepath.Join(target, name))
		require.NoError(t, err)
		assert.Equal(t, "whole "+name, string(got))
	}
	assert.NoFileExists(t, filepath.Join(target, movingPrefix+"half"))
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	assert.Len(t, entries, 2)
}

func TestHost_PrepareStorageRefusesAnEntryWhoseNameIsTakenInStorageAndNamesBothPaths(t *testing.T) {
	r, state := mvmdRig(t)
	src, dst := filepath.Join(state, "disks", "d1"), filepath.Join(r.mnt, "disks", "d1")
	require.NoError(t, os.MkdirAll(filepath.Join(state, "disks"), 0o700))
	require.NoError(t, os.WriteFile(src, []byte("old"), 0o600))
	seedStorage(t, r, func() {
		require.NoError(t, os.MkdirAll(filepath.Join(r.mnt, "disks"), 0o700))
		require.NoError(t, os.WriteFile(dst, []byte("other"), 0o600))
	})

	err := prepareMvmd(r)

	require.Error(t, err)
	assert.ErrorContains(t, err, src)
	assert.ErrorContains(t, err, dst)
	assert.ErrorContains(t, err, "mvmd will refuse to start")
	got, _ := os.ReadFile(src)
	assert.Equal(t, "old", string(got), "neither side is overwritten")
	got, _ = os.ReadFile(dst)
	assert.Equal(t, "other", string(got))
}

func TestHost_PrepareStorageLeavesALinkThatIsRightAndRefusesOneThatIsNot(t *testing.T) {
	r, state := mvmdRig(t)
	require.NoError(t, prepareMvmd(r))
	require.NoError(t, os.WriteFile(filepath.Join(r.mnt, "snapshots", "s1"), []byte("x"), 0o600))

	require.NoError(t, linkTo(filepath.Join(state, "disks"), filepath.Join(r.mnt, "disks"), slog.Default()), "the same link again")
	assert.FileExists(t, filepath.Join(state, "snapshots", "s1"), "what is behind the link is untouched")

	err := linkTo(filepath.Join(state, "disks"), filepath.Join(r.mnt, "elsewhere"), slog.Default())
	require.Error(t, err)
	assert.ErrorContains(t, err, "mvmd will refuse to start")
}

func TestHost_PrepareStorageDropsWhatAnInterruptedRemovalLeftAndDoesNotTakeItForAConflict(t *testing.T) {
	r, state := mvmdRig(t)
	snaps, target := filepath.Join(state, "snapshots"), filepath.Join(r.mnt, "snapshots")
	require.NoError(t, os.MkdirAll(filepath.Join(snaps, removedPrefix+"s1"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(snaps, removedPrefix+"s1", "part"), []byte("half"), 0o600))
	seedStorage(t, r, func() {
		require.NoError(t, os.MkdirAll(target, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(target, "s1"), []byte("whole"), 0o600))
	})

	require.NoError(t, prepareMvmd(r))

	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the tombstone was neither moved nor kept")
	assert.Equal(t, "s1", entries[0].Name())
}

func TestCopyData_KeepsAHoleOfASparseFileUnallocated(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	f, err := os.Create(src)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("tail"), 64<<20)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(65<<20))
	require.NoError(t, f.Close())

	require.NoError(t, copyFileMode(src, filepath.Join(dir, "dst"), 0o600))

	want, err := os.ReadFile(src)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "dst"))
	require.NoError(t, err)
	assert.Equal(t, want, got, "same bytes and size")
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(dir, "dst"), &st))
	assert.Less(t, st.Blocks*512, int64(8<<20), "the 64 MiB hole takes no blocks")
}
