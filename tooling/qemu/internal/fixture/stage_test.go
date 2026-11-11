package fixture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadEnv_ReadsKeyValueLinesAndSkipsComments(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.env")
	require.NoError(t, os.WriteFile(p, []byte("# note\nA=1\n\nB=two=2\n"), 0o644))

	env, err := readEnv(p)

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1", "B": "two=2"}, env)
}

func TestReadEnv_RejectsALineWithoutAnEquals(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.env")
	require.NoError(t, os.WriteFile(p, []byte("nonsense\n"), 0o644))

	_, err := readEnv(p)

	assert.ErrorContains(t, err, "is not KEY=VALUE")
}

func TestCopyFile_FollowsLinksAndSetsTheMode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "real"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "link")))

	require.NoError(t, copyFile(filepath.Join(dir, "link"), filepath.Join(dir, "out"), 0o755))

	info, err := os.Lstat(filepath.Join(dir, "out"))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
}

func TestStage_FailsNamingADigestThatIsMissing(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "inputs.env"), []byte("MVMD_KERNEL_SHA256=a\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "guest-inputs.env"), []byte(""), 0o644))

	_, err := Stage(Sources{ImageDir: dir, GuestDir: dir}, t.TempDir())

	assert.ErrorContains(t, err, "is not in inputs.env")
}
