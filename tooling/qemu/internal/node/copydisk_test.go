package node

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyDisk_GivesTheNodeItsOwnFileWithTheBytesOfTheImage(t *testing.T) {
	dir := t.TempDir()
	from, to := filepath.Join(dir, "naru.img"), filepath.Join(dir, "node-1", "naru.img")
	require.NoError(t, os.WriteFile(from, []byte("disk"), 0o600))

	require.NoError(t, copyDisk(from, to))

	got, err := os.ReadFile(to)
	require.NoError(t, err)
	assert.Equal(t, "disk", string(got))
	require.NoError(t, os.WriteFile(to, []byte("written"), 0o600))
	orig, err := os.ReadFile(from)
	require.NoError(t, err)
	assert.Equal(t, "disk", string(orig), "a write to the copy leaves the image as it was")
}

func TestCopyDisk_RefusesAMissingImage(t *testing.T) {
	assert.Error(t, copyDisk(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "x", "naru.img")))
}
