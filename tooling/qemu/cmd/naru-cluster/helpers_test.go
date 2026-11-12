package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
}

func TestStageGaonBinaries_CopiesTheOnesFoundExecutableAndNamesThem(t *testing.T) {
	from, inputs := t.TempDir(), t.TempDir()
	write(t, filepath.Join(from, "gaon-apiserver"), "api", 0o644)
	write(t, filepath.Join(from, "gaon-node-agent"), "agent", 0o644)
	write(t, filepath.Join(from, "unrelated"), "x", 0o644)

	found, err := stageGaonBinaries(from, inputs)

	require.NoError(t, err)
	assert.Equal(t, []string{"gaon-apiserver", "gaon-node-agent"}, found, "a binary the directory lacks is not an error")
	info, err := os.Stat(filepath.Join(inputs, "gaon", "gaon-apiserver"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	assert.NoFileExists(t, filepath.Join(inputs, "gaon", "unrelated"))
	assert.NoFileExists(t, filepath.Join(inputs, "gaon", "gaon-scheduler"))
}

func TestStageGaonBinaries_ADirectoryHoldingNoneOfTheFourIsAnError(t *testing.T) {
	from := t.TempDir()
	write(t, filepath.Join(from, "unrelated"), "x", 0o644)

	_, err := stageGaonBinaries(from, t.TempDir())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "holds none of")
}

func TestStageGaonBinaries_NoDirectoryStagesNothing(t *testing.T) {
	found, err := stageGaonBinaries("", t.TempDir())

	require.NoError(t, err)
	assert.Empty(t, found)
}

func TestCopyTree_KeepsTheExecutableBitAndMakesTheRestReadable(t *testing.T) {
	from, to := t.TempDir(), filepath.Join(t.TempDir(), "extra")
	write(t, filepath.Join(from, "run.sh"), "#!/bin/sh", 0o700)
	write(t, filepath.Join(from, "sub", "data.txt"), "data", 0o600)

	require.NoError(t, copyTree(from, to))

	script, err := os.Stat(filepath.Join(to, "run.sh"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), script.Mode().Perm(), "executable stays executable, readable by other users")
	data, err := os.Stat(filepath.Join(to, "sub", "data.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), data.Mode().Perm())
	dir, err := os.Stat(filepath.Join(to, "sub"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), dir.Mode().Perm())
}

func TestCopyTree_RefusesASymbolicLinkAndNamesIt(t *testing.T) {
	from := t.TempDir()
	write(t, filepath.Join(from, "real", "a.txt"), "a", 0o644)
	require.NoError(t, os.Symlink(filepath.Join(from, "real"), filepath.Join(from, "link")))

	err := copyTree(from, filepath.Join(t.TempDir(), "extra"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(from, "link"))
	assert.Contains(t, err.Error(), "symbolic link is refused")
}

// slowNode takes startup to have each service "started", honouring the timeout and the context as
// Node.WaitFor does.
type slowNode struct{ startup time.Duration }

func (s slowNode) Console() string { return "line 1\nline 2\n" }

func (s slowNode) WaitFor(ctx context.Context, timeout time.Duration, _ ...string) error {
	select {
	case <-time.After(s.startup):
		return nil
	case <-time.After(timeout):
		return errors.New("timed out waiting for the console")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWaitServices_TheDeadlineIsOneForEveryNodeAndServiceTogether(t *testing.T) {
	// Each wait takes 150 ms, which fits a deadline of 500 ms; six of them do not.
	waits := []serviceWait{
		{name: "node-1", node: slowNode{150 * time.Millisecond}, services: []string{"registry", "etcd", "gaon-apiserver"}},
		{name: "node-2", node: slowNode{150 * time.Millisecond}, services: []string{"registry", "gaon-node-agent", "gaon-scheduler"}},
	}
	start := time.Now()

	err := waitServices(context.Background(), start.Add(500*time.Millisecond), waits, slog.New(slog.NewTextHandler(io.Discard, nil)))

	require.Error(t, err, "six waits of 150 ms overrun a 500 ms deadline")
	assert.Less(t, time.Since(start), 800*time.Millisecond, "and it fails at the deadline, not after each wait has had its own")
	assert.Contains(t, err.Error(), "line 2", "the error carries the console's tail")
}

func TestWaitServices_ReturnsOnceEveryServiceStartedBeforeTheDeadline(t *testing.T) {
	waits := []serviceWait{{name: "node-1", node: slowNode{10 * time.Millisecond}, services: []string{"registry", "etcd"}}}

	err := waitServices(context.Background(), time.Now().Add(5*time.Second), waits, slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.NoError(t, err)
}

func TestTail_KeepsTheLastLinesOnly(t *testing.T) {
	assert.Equal(t, "c\nd", tail("a\nb\nc\nd\n", 2))
	assert.Equal(t, "a\nb", tail("a\nb", 5), "fewer lines than asked for are all kept")
	assert.False(t, strings.HasSuffix(tail("a\nb\n", 5), "\n"))
}

func TestCheckDisable_ControllersNeedBootPKIFalseAndNothingElseDoes(t *testing.T) {
	err := checkDisable([]string{"registry", "gaon-controllers"}, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "-boot-pki=false")

	assert.NoError(t, checkDisable([]string{"gaon-controllers"}, false))
	assert.NoError(t, checkDisable([]string{"gaon-scheduler", "registry"}, true))
	assert.ErrorContains(t, checkDisable([]string{"nothing"}, true), "is not a service")
}
