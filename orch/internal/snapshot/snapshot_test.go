package snapshot_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/snapshot"
)

type source struct{ data string }

func (s source) Snapshot(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader([]byte(s.data))), nil
}

type sealer struct{}

func (sealer) Seal(p []byte) ([]byte, error) { return append([]byte("SEALED|"), p...), nil }

func taker(dir string, s snapshot.Sealer, now func() time.Time) *snapshot.Taker {
	return &snapshot.Taker{Source: source{"inventory"}, Sealer: s, Dir: dir, Keep: 3, Now: now, Log: slog.Default()}
}

func TestSnapshotIsSealedBeforeItIsWritten(t *testing.T) {
	dir := t.TempDir()
	res, err := taker(dir, sealer{}, time.Now).Take(t.Context())
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(dir, res.File))
	require.NoError(t, err)
	require.Equal(t, "SEALED|inventory", string(b))
	st, err := os.Stat(filepath.Join(dir, res.File))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

func TestNothingIsWrittenWithoutARecoveryKey(t *testing.T) {
	dir := t.TempDir()
	_, err := taker(dir, nil, time.Now).Take(t.Context())
	require.ErrorIs(t, err, snapshot.ErrNoSealer)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestOnlyTheNewestSnapshotsAreKept(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	var names []string
	for i := range 5 {
		res, err := taker(dir, sealer{}, func() time.Time { return at.Add(time.Duration(i) * time.Hour) }).Take(t.Context())
		require.NoError(t, err)
		names = append(names, res.File)
	}
	got, err := snapshot.List(dir)
	require.NoError(t, err)
	require.Equal(t, []string{names[4], names[3], names[2]}, got, "the newest three, newest first")
}
