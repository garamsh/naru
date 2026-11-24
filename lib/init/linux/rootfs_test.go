package linux

import (
	"os"
	"path/filepath"
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
