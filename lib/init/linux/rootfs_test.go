package linux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoveContents_CopiesTreesAndLinksThenRemovesTheOriginals(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(from, "bin"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(from, "bin", "tool"), []byte("payload"), 0o750))
	require.NoError(t, os.Symlink("tool", filepath.Join(from, "bin", "alias")))
	require.NoError(t, os.Mkdir(filepath.Join(from, "keep"), 0o755))

	require.NoError(t, moveContents(from, to, "keep"))

	b, err := os.ReadFile(filepath.Join(to, "bin", "tool"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(b))
	info, err := os.Stat(filepath.Join(to, "bin", "tool"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o750), info.Mode().Perm())
	dest, err := os.Readlink(filepath.Join(to, "bin", "alias"))
	require.NoError(t, err)
	assert.Equal(t, "tool", dest)
	assert.NoDirExists(t, filepath.Join(from, "bin"), "the original is removed")
	assert.DirExists(t, filepath.Join(from, "keep"), "the kept entry stays")
	assert.NoDirExists(t, filepath.Join(to, "keep"), "and is not copied")
}

func TestMoveContents_KeepsTheSpecialModeBitsAndTheHardLinks(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(from, "bin"), 0o755))
	su := filepath.Join(from, "bin", "su")
	require.NoError(t, os.WriteFile(su, []byte("x"), 0o755))
	require.NoError(t, os.Chmod(su, 0o755|os.ModeSetuid|os.ModeSetgid))
	require.NoError(t, os.Link(su, filepath.Join(from, "bin", "su2")))
	tmp := filepath.Join(from, "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o755))
	require.NoError(t, os.Chmod(tmp, 0o777|os.ModeSticky))

	require.NoError(t, moveContents(from, to, "none"))

	info, err := os.Stat(filepath.Join(to, "bin", "su"))
	require.NoError(t, err)
	assert.Equal(t, 0o755|os.ModeSetuid|os.ModeSetgid, info.Mode()&(os.ModePerm|os.ModeSetuid|os.ModeSetgid), "set-user-ID and set-group-ID")
	info, err = os.Stat(filepath.Join(to, "tmp"))
	require.NoError(t, err)
	assert.NotZero(t, info.Mode()&os.ModeSticky, "the sticky bit")
	a, err := os.Stat(filepath.Join(to, "bin", "su"))
	require.NoError(t, err)
	b, err := os.Stat(filepath.Join(to, "bin", "su2"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(a, b), "the second name is a link to the first")
}

func TestMoveContents_KeepsTheExtendedAttributes(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	f := filepath.Join(from, "f")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0o644))
	if err := syscall.Setxattr(f, "user.naru", []byte("v"), 0); err != nil {
		t.Skipf("this file system keeps no user extended attributes: %v", err)
	}

	require.NoError(t, moveContents(from, to, "none"))

	buf := make([]byte, 8)
	n, err := syscall.Getxattr(filepath.Join(to, "f"), "user.naru", buf)
	require.NoError(t, err)
	assert.Equal(t, "v", string(buf[:n]))
}

const shareHelperEnv = "NARU_SHARE_MOUNT_HELPER"

// TestShareMount_MakesTheMountAndTheMountsUnderItShared runs shareMountHelper in a mount namespace of
// its own, where it may mount.
func TestShareMount_MakesTheMountAndTheMountsUnderItShared(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestShareMountHelper$")
	cmd.Env = append(os.Environ(), shareHelperEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	out, err := cmd.CombinedOutput()
	if err != nil && strings.Contains(string(out), "SKIP") {
		t.Skipf("no mount namespace for this user: %s", out)
	}
	require.NoError(t, err, string(out))
}

func TestShareMountHelper(t *testing.T) {
	if os.Getenv(shareHelperEnv) == "" {
		t.Skip("runs inside TestShareMount_MakesTheMountAndTheMountsUnderItShared")
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		t.Fatalf("SKIP make the mounts private: %v", err)
	}
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	require.NoError(t, syscall.Mount("tmpfs", dir, "tmpfs", 0, ""))
	t.Cleanup(func() { _ = syscall.Unmount(dir, syscall.MNT_DETACH) })
	require.NoError(t, os.Mkdir(child, 0o755))
	require.NoError(t, syscall.Mount("tmpfs", child, "tmpfs", 0, ""))
	assert.False(t, isShared(t, dir), "a mount is private until it is shared (control)")
	assert.False(t, isShared(t, child), "so is the one under it (control)")

	require.NoError(t, shareMount(dir))

	assert.True(t, isShared(t, dir), "the mount")
	assert.True(t, isShared(t, child), "the mount under it")
	later := filepath.Join(dir, "later")
	require.NoError(t, os.Mkdir(later, 0o755))
	require.NoError(t, syscall.Mount("tmpfs", later, "tmpfs", 0, ""))
	assert.True(t, isShared(t, later), "a mount made afterwards")
}

// isShared reports whether /proc/self/mountinfo lists the mount at dir with a shared peer group.
func isShared(t *testing.T, dir string) bool {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/mountinfo")
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 6 && fields[4] == dir {
			for _, f := range fields[6:] {
				if f == "-" {
					break
				}
				if strings.HasPrefix(f, "shared:") {
					return true
				}
			}
			return false
		}
	}
	t.Fatalf("%s is not a mount point", dir)
	return false
}
