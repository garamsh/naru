package gaoncli_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/gaoncli"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// fake writes a CLI that appends a line to dir/runs on each run and sets n to the number of runs
// before it executes script.
func fake(t *testing.T, dir, script string) *gaoncli.Client {
	t.Helper()
	runs := filepath.Join(dir, "runs")
	bin := filepath.Join(dir, "gaon")
	body := "#!/bin/sh\necho x >> " + runs + "\nn=$(wc -l < " + runs + ")\n" + script
	require.NoError(t, os.WriteFile(bin, []byte(body), 0o700)) //nolint:gosec // a test program
	return gaoncli.New(bin, "127.0.0.1:1", naructl.Client{})
}

func runs(t *testing.T, dir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "runs"))
	require.NoError(t, err)
	return len(b) / 2
}

func TestApply_RunsTheCLIAgainWhileTheObjectChangesUnderIt(t *testing.T) {
	dir := t.TempDir()
	c := fake(t, dir, `if [ "$n" -lt 3 ]; then echo "gaon: apply node/n: not applied: Conflict: changed"; exit 1; fi
echo "node/n configured"`)
	out, err := c.Apply(context.Background(), dir, "kind: Node\n")
	require.NoError(t, err)
	assert.Contains(t, out, "configured")
	assert.Equal(t, 3, runs(t, dir))
}

func TestApply_DoesNotRepeatAnyOtherRefusal(t *testing.T) {
	dir := t.TempDir()
	c := fake(t, dir, `echo "gaon: invalid"; exit 1`)
	_, err := c.Apply(context.Background(), dir, "kind: Node\n")
	require.Error(t, err)
	assert.Equal(t, 1, runs(t, dir))
}
