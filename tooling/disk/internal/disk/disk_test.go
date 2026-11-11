package disk_test

import (
	"bytes"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/disk/internal/disk"
)

const (
	diskGUID = "3F6A1B52-0C9E-4D7A-9B21-6E5D8A4C7F10"
	espGUID  = "A4D2E8C1-5B37-4F90-8E16-2C7B9D0A3E45"
)

func spec(t *testing.T, dir string) disk.Spec {
	t.Helper()
	kernel, initrd := filepath.Join(dir, "bzImage"), filepath.Join(dir, "initramfs.cpio.gz")
	require.NoError(t, os.WriteFile(kernel, bytes.Repeat([]byte("kernel"), 300000), 0o644))
	require.NoError(t, os.WriteFile(initrd, bytes.Repeat([]byte("initrd"), 100000), 0o644))
	return disk.Spec{
		Kernel: kernel, Initrd: initrd, Out: filepath.Join(dir, "naru.img"),
		Stamp: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC), DiskGUID: diskGUID, ESPGUID: espGUID,
	}
}

func digest(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(b)
}

func TestBuild_WritesAGPTDiskWithOneEFISystemPartition(t *testing.T) {
	s := spec(t, t.TempDir())

	require.NoError(t, disk.Build(s))

	out, err := exec.Command("/sbin/sgdisk", "--print", "--info=1", s.Out).CombinedOutput()
	require.NoError(t, err, string(out))
	text := string(out)
	assert.Contains(t, text, "Disk identifier (GUID): "+diskGUID)
	assert.Contains(t, text, "Partition GUID code: C12A7328-F81F-11D2-BA4B-00A0C93EC93B (EFI system partition)")
	assert.Contains(t, text, "Partition unique GUID: "+espGUID)
	assert.Contains(t, text, "Main partition table begins at sector 2", "sgdisk read a GPT")
	assert.Len(t, regexp.MustCompile(`(?m)^\s+\d+\s+\d+\s+\d+\s`).FindAllString(text, -1), 1, "the table holds one partition")
}

func TestBuild_TwoBuildsInTwoDirectoriesGiveTheSameDisk(t *testing.T) {
	a, b := spec(t, t.TempDir()), spec(t, t.TempDir())

	require.NoError(t, disk.Build(a))
	require.NoError(t, disk.Build(b))

	assert.Equal(t, digest(t, a.Out), digest(t, b.Out))
}

func TestBuild_ADifferentGUIDGivesADifferentDisk(t *testing.T) {
	a, b := spec(t, t.TempDir()), spec(t, t.TempDir())
	b.DiskGUID = "3F6A1B52-0C9E-4D7A-9B21-6E5D8A4C7F11"

	require.NoError(t, disk.Build(a))
	require.NoError(t, disk.Build(b))

	assert.NotEqual(t, digest(t, a.Out), digest(t, b.Out))
}

func TestBuild_FailsWhenTheKernelIsMissing(t *testing.T) {
	s := spec(t, t.TempDir())
	s.Kernel = filepath.Join(t.TempDir(), "absent")

	assert.Error(t, disk.Build(s))
}
