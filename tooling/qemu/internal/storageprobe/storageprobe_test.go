package storageprobe_test

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/storageprobe"
)

func report(t *testing.T, dir string) string {
	t.Helper()
	var out bytes.Buffer

	require.NoError(t, storageprobe.Run(dir, slog.New(slog.NewTextHandler(&out, nil))))

	return out.String()
}

func TestRun_WritesTheMarkerOnTheFirstRunAndFindsItOnTheNext(t *testing.T) {
	dir := t.TempDir()

	first := report(t, dir)
	second := report(t, dir)

	assert.Contains(t, first, "marker=written")
	assert.Contains(t, second, "marker=kept")
	got, err := os.ReadFile(filepath.Join(dir, "marker"))
	require.NoError(t, err)
	assert.Equal(t, "naru66", string(got))
}

func TestRun_ReportsTheFileSystemItsSizeAndWhetherItCloned(t *testing.T) {
	out := report(t, t.TempDir())

	// The host's file system decides the values; the fields are always there.
	for _, field := range []string{"fs=", "reflink=", "size="} {
		assert.Contains(t, out, field)
	}
}

func TestRun_FailsOnADirectoryThatIsNotThere(t *testing.T) {
	err := storageprobe.Run(filepath.Join(t.TempDir(), "absent"), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	require.Error(t, err)
}
