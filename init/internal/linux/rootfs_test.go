package linux

import (
	"os"
	"path/filepath"
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
