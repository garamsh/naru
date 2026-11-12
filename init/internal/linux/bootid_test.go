package linux_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/linux"
)

func TestReadBootID_ReturnsTheIDWithoutItsNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boot_id")
	require.NoError(t, os.WriteFile(path, []byte("0f1e2d3c-aaaa-bbbb-cccc-0123456789ab\n"), 0o644))

	id, err := linux.ReadBootID(path)

	require.NoError(t, err)
	assert.Equal(t, "0f1e2d3c-aaaa-bbbb-cccc-0123456789ab", id)
}

func TestReadBootID_RefusesAMissingOrEmptyFile(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(empty, []byte("\n"), 0o644))

	_, err := linux.ReadBootID(filepath.Join(dir, "missing"))
	assert.Error(t, err)
	_, err = linux.ReadBootID(empty)
	assert.Error(t, err)
}
