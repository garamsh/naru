package fat_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/disk/internal/fat"
)

var stamp = time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)

// volume makes an empty FAT32 volume of 64 MiB with 512-byte clusters, which is what the builder makes.
func volume(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "esp.img")
	require.NoError(t, os.WriteFile(path, nil, 0o644))
	require.NoError(t, os.Truncate(path, 64<<20))
	out, err := exec.Command("/sbin/mkfs.fat", "-F", "32", "-S", "512", "-s", "1", "-n", "NARUESP", "--invariant", "-i", "6a0b4c80", path).CombinedOutput()
	require.NoError(t, err, string(out))
	return path
}

func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func add(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	v, err := fat.Open(path, stamp)
	require.NoError(t, err)
	for _, name := range []string{"EFI/BOOT/BOOTX64.EFI", "INITRD.GZ"} {
		require.NoError(t, v.Add(name, bytes.NewReader(files[name]), int64(len(files[name]))))
	}
	require.NoError(t, v.Close())
}

func TestAdd_FilesReadBackAndFsckAcceptsTheVolume(t *testing.T) {
	path := volume(t)
	files := map[string][]byte{"EFI/BOOT/BOOTX64.EFI": content(3<<20 + 123), "INITRD.GZ": content(512)}

	add(t, path, files)

	v, err := fat.Open(path, stamp)
	require.NoError(t, err)
	defer func() { _ = v.Close() }()
	for name, want := range files {
		got, err := v.ReadFile(name)
		require.NoError(t, err, name)
		assert.True(t, bytes.Equal(want, got), "%s reads back as it was written", name)
	}
	out, err := exec.Command("/sbin/fsck.fat", "-n", "-v", path).CombinedOutput()
	require.NoError(t, err, string(out))
	// 3 MiB + 123 bytes is 6145 clusters of 512 bytes, 1 for INITRD.GZ, 1 each for EFI, BOOT and the root directory.
	assert.Contains(t, string(out), "6149/129022 clusters", "fsck found the chains the writer made, and no others")
}

func TestAdd_TheSameInputsGiveTheSameBytes(t *testing.T) {
	files := map[string][]byte{"EFI/BOOT/BOOTX64.EFI": content(100000), "INITRD.GZ": content(70000)}
	first, second := volume(t), volume(t)

	add(t, first, files)
	add(t, second, files)

	a, err := os.ReadFile(first)
	require.NoError(t, err)
	b, err := os.ReadFile(second)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(a, b))
}

func TestAdd_RefusesWhatTheVolumeCannotHold(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"a lower-case name", "EFI/boot/BOOTX64.EFI"},
		{"a base longer than eight characters", "BOOTX64EFI.EFI"},
		{"an extension longer than three characters", "BOOT.EFIX"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := fat.Open(volume(t), stamp)
			require.NoError(t, err)
			defer func() { _ = v.Close() }()

			assert.Error(t, v.Add(tc.path, bytes.NewReader([]byte("x")), 1))
			assert.NoError(t, v.Add("OK.EFI", bytes.NewReader([]byte("x")), 1), "control: a short upper-case name is accepted")
		})
	}
}

func TestAdd_RefusesAFileThatExists(t *testing.T) {
	v, err := fat.Open(volume(t), stamp)
	require.NoError(t, err)
	defer func() { _ = v.Close() }()
	require.NoError(t, v.Add("INITRD.GZ", bytes.NewReader([]byte("a")), 1))

	assert.Error(t, v.Add("INITRD.GZ", bytes.NewReader([]byte("b")), 1))
}

func TestOpen_RefusesAFileThatIsNotFAT32(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zero.img")
	require.NoError(t, os.WriteFile(path, make([]byte, 1<<20), 0o644))

	_, err := fat.Open(path, stamp)

	assert.Error(t, err)
}
