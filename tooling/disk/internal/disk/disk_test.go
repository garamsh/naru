package disk_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/disk/internal/disk"
	"github.com/garamsh/naru/tooling/disk/internal/fat"
)

const (
	diskGUID = "3F6A1B52-0C9E-4D7A-9B21-6E5D8A4C7F10"
	espGUID  = "A4D2E8C1-5B37-4F90-8E16-2C7B9D0A3E45"
	mib      = 1 << 20
)

// scratch is a temporary directory that is also removed shortly before the test binary's own deadline.
// A test that runs out of time ends in a panic that runs no cleanup, and what a test builds here is
// a disk of 1 GiB or more, which a timeout would otherwise leave behind.
func scratch(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if deadline, ok := t.Deadline(); ok {
		removal := time.AfterFunc(time.Until(deadline)-5*time.Second, func() { _ = os.RemoveAll(dir) })
		t.Cleanup(func() { removal.Stop() })
	}
	return dir
}

func spec(t *testing.T) disk.Spec {
	t.Helper()
	return specIn(t, scratch(t))
}

func specIn(t *testing.T, dir string) disk.Spec {
	t.Helper()
	kernel := filepath.Join(dir, "bzImage")
	require.NoError(t, os.WriteFile(kernel, bytes.Repeat([]byte("kernel"), 300000), 0o644))
	return disk.Spec{
		Kernel: kernel, Out: filepath.Join(dir, "naru.img"),
		Stamp: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), DiskGUID: diskGUID, ESPGUID: espGUID,
	}
}

// The disk of the default spec is built once for the tests that only read it. sgdisk ends with sync(2),
// which waits for every dirty page of the machine, so on a machine others write to each Build costs
// minutes, and a test per disk is what took the package past go test's timeout. Tests that change
// the spec, or that need a second disk to compare, build their own.
var shared struct {
	once sync.Once
	dir  string
	spec disk.Spec
	err  error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if shared.dir != "" {
		_ = os.RemoveAll(shared.dir)
	}
	os.Exit(code)
}

func builtDefault(t *testing.T) disk.Spec {
	t.Helper()
	shared.once.Do(func() {
		shared.dir, shared.err = os.MkdirTemp("", "naru-disk-test-")
		if shared.err != nil {
			return
		}
		shared.spec = specIn(t, shared.dir)
		shared.err = disk.Build(shared.spec)
	})
	require.NoError(t, shared.err)
	return shared.spec
}

func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(b)
}

// esp copies the partition out of the disk the way the firmware finds it: from the fixed start
// sector to the MiB the backup GPT keeps.
func esp(t *testing.T, s disk.Spec) string {
	t.Helper()
	f, err := os.Open(s.Out)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	require.NoError(t, err)
	start := int64(disk.StartSector) * 512
	out := filepath.Join(scratch(t), "esp.img")
	dst, err := os.Create(out)
	require.NoError(t, err)
	// The partition is 1 GiB and nearly all zeros: a copy that writes the zeros costs the test
	// a GiB of disk, which under load took it past the timeout and left a GiB behind when killed.
	length := info.Size() - start - mib
	require.NoError(t, dst.Truncate(length))
	section := io.NewSectionReader(f, start, length)
	chunk := make([]byte, 64<<10)
	for at := int64(0); at < length; at += int64(len(chunk)) {
		n, err := io.ReadFull(section, chunk)
		require.NoError(t, err)
		if len(bytes.TrimLeft(chunk[:n], "\x00")) > 0 {
			_, err = dst.WriteAt(chunk[:n], at)
			require.NoError(t, err)
		}
	}
	require.NoError(t, dst.Close())
	return out
}

func TestBuild_WritesAGPTDiskWithOneEFISystemPartition(t *testing.T) {
	s := builtDefault(t)

	out, err := exec.Command("/sbin/sgdisk", "--print", "--info=1", s.Out).CombinedOutput()
	require.NoError(t, err, string(out))
	text := string(out)
	assert.Contains(t, text, "Disk identifier (GUID): "+diskGUID)
	assert.Contains(t, text, "Partition GUID code: C12A7328-F81F-11D2-BA4B-00A0C93EC93B (EFI system partition)")
	assert.Contains(t, text, "Partition unique GUID: "+espGUID)
	assert.Contains(t, text, "Main partition table begins at sector 2", "sgdisk read a GPT")
	assert.Len(t, regexp.MustCompile(`(?m)^\s+\d+\s+\d+\s+\d+\s`).FindAllString(text, -1), 1, "the table holds one partition")
	assert.Regexp(t, `First sector: 2048 `, text, "the device path of a slot is computed from this start sector")
}

func TestBuild_TwoBuildsInTwoDirectoriesGiveTheSameDisk(t *testing.T) {
	a, b := builtDefault(t), spec(t)

	require.NoError(t, disk.Build(b))

	assert.Equal(t, digest(t, a.Out), digest(t, b.Out))
}

func TestBuild_ADifferentGUIDGivesADifferentDisk(t *testing.T) {
	a, b := builtDefault(t), spec(t)
	b.DiskGUID = "3F6A1B52-0C9E-4D7A-9B21-6E5D8A4C7F11"

	require.NoError(t, disk.Build(b))

	assert.NotEqual(t, digest(t, a.Out), digest(t, b.Out))
}

func TestBuild_FailsWhenTheKernelIsMissing(t *testing.T) {
	s := spec(t)
	s.Kernel = filepath.Join(t.TempDir(), "absent")

	assert.Error(t, disk.Build(s))
}

func TestBuild_TheESPHoldsTheDefaultSlotAndNoTrialSlot(t *testing.T) {
	s := builtDefault(t)
	v, err := fat.Open(esp(t, s), s.Stamp)
	require.NoError(t, err)
	defer func() { _ = v.Close() }()

	_, err = v.ReadFile(disk.Loader)
	assert.NoError(t, err, "the default slot is there")
	_, err = v.ReadFile(disk.Trial)
	assert.ErrorContains(t, err, "not found", "the trial slot is written by an upgrade, not by the disk")
}

func TestESPBytes_IsOneGiBUntilTwoSlotsOfTwiceTheKernelNeedMore(t *testing.T) {
	tests := []struct {
		name   string
		kernel int64
		want   int64
	}{
		{"a kernel of 211 MB, the size of v0.1.0-dev.8's", 211239936, 1024 * mib},
		{"a kernel whose four copies and slack come to exactly 1 GiB", 254 * mib, 1024 * mib},
		{"a kernel whose four copies and slack pass 1 GiB", 300 * mib, 1208 * mib},
		{"a kernel that is not a whole number of MiB rounds the partition up", 300*mib + 1, 1209 * mib},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, disk.ESPBytes(tc.kernel))
		})
	}
}

func TestBuild_TheESPHasRoomForATrialKernelTwiceTheSizeOfTheBuiltOne(t *testing.T) {
	// The ESP is 1 GiB here, whatever the kernel's size, so the room tested is the headroom and
	// not the minimum of the old layout, which held one more kernel of the same size.
	const kernelBytes = 33 * mib
	s := spec(t)
	require.NoError(t, os.WriteFile(s.Kernel, bytes.Repeat([]byte("k"), kernelBytes), 0o644))
	require.NoError(t, disk.Build(s))
	out, err := exec.Command("/sbin/sgdisk", "--info=1", s.Out).CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "Partition size: 2097152 sectors", "the partition is 1 GiB")
	v, err := fat.Open(esp(t, s), s.Stamp)
	require.NoError(t, err)

	err = v.Add(disk.Trial, bytes.NewReader(bytes.Repeat([]byte("t"), 2*kernelBytes)), 2*kernelBytes)

	assert.NoError(t, err)
	assert.NoError(t, v.Close())
}

// extension writes an extension build's <name>.sqfs and <name>.json of size bytes in dir.
func extension(t *testing.T, dir, name, version string, size int) disk.Extension {
	t.Helper()
	image := bytes.Repeat([]byte("squash"), size/6)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".sqfs"), image, 0o644))
	sum := sha256.Sum256(image)
	desc := filepath.Join(dir, name+".json")
	require.NoError(t, os.WriteFile(desc, []byte(`{"name":"`+name+`","version":"`+version+`","sha256":"`+hex.EncodeToString(sum[:])+`","services":[]}`), 0o644))
	return disk.Extension{Description: desc}
}

func TestExtensionRoom_CountsTheImagesTheirAllocationTablesAndAMiBRoundedUp(t *testing.T) {
	assert.Zero(t, disk.ExtensionRoom(0), "a disk with no extension is the size it was")
	// tooling/qemu's TestExtensionRoom_... asserts the same figure for the same size.
	assert.EqualValues(t, 103*mib, disk.ExtensionRoom(100*mib))
}

func TestBuild_CarriesTheExtensionsOnTheESPBesideTheKernelAndGrowsTheESPByTheirRoom(t *testing.T) {
	dir := scratch(t)
	s := specIn(t, dir)
	s.Extensions = []disk.Extension{extension(t, dir, "naru-volumes", "v9", 3*mib)}
	plain := builtDefault(t)
	plainInfo, err := os.Stat(plain.Out)
	require.NoError(t, err)

	require.NoError(t, disk.Build(s))

	info, err := os.Stat(s.Out)
	require.NoError(t, err)
	assert.EqualValues(t, disk.ExtensionRoom(3*mib/6*6), info.Size()-plainInfo.Size(), "only the extension's room is added: the slots keep theirs")
	v, err := fat.Open(esp(t, s), s.Stamp)
	require.NoError(t, err)
	defer func() { _ = v.Close() }()
	raw, err := v.ReadFile(disk.ExtensionIndex)
	require.NoError(t, err)
	var idx struct {
		Extensions []struct {
			Name, Version, SHA256, File string
			Bytes                       int64
		}
	}
	require.NoError(t, json.Unmarshal(raw, &idx))
	require.Len(t, idx.Extensions, 1)
	want, err := os.ReadFile(filepath.Join(dir, "naru-volumes.sqfs"))
	require.NoError(t, err)
	got, err := v.ReadFile(disk.ExtensionDir + "/" + idx.Extensions[0].File)
	require.NoError(t, err)
	assert.Equal(t, want, got, "the image on the ESP is the file that went in")
	assert.Equal(t, "naru-volumes", idx.Extensions[0].Name)
	assert.Equal(t, "v9", idx.Extensions[0].Version)
	assert.EqualValues(t, len(want), idx.Extensions[0].Bytes)
}

func TestBuild_RefusesAnExtensionWhoseImageIsNotTheDigestItsDescriptionNames(t *testing.T) {
	dir := scratch(t)
	s := specIn(t, dir)
	e := extension(t, dir, "naru-volumes", "v9", mib)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "naru-volumes.sqfs"), []byte("changed"), 0o644))
	s.Extensions = []disk.Extension{e}

	err := disk.Build(s)

	require.ErrorContains(t, err, "has the SHA-256")
	_, statErr := os.Stat(s.Out)
	assert.True(t, os.IsNotExist(statErr), "no disk is written")
}

func TestSpecLayout_NamesTheRoomTheExtensionsTakeAsTheReleaseReadsIt(t *testing.T) {
	dir := scratch(t)
	s := specIn(t, dir)
	s.Extensions = []disk.Extension{extension(t, dir, "naru-volumes", "v9", 6*mib)}

	l, err := s.Layout()

	require.NoError(t, err)
	raw, err := json.Marshal(l)
	require.NoError(t, err)
	// tooling/qemu's release reads this key; its test reads a file of the same form.
	assert.JSONEq(t, `{"extensionRoom":8388608}`, string(raw))
	empty, err := specIn(t, dir).Layout()
	require.NoError(t, err)
	assert.Zero(t, empty.ExtensionRoom, "the control: no extension, no room")
}
