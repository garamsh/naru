package command_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostDrainAndUndrain_AskOrchAndSayWhatTheHostIs(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "host", "drain", "host-a")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host host-a is draining")
	out, err = run(t, op.args(f, "host", "undrain", "host-a")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host host-a is not draining")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"drain host-a", "undrain host-a"}, f.drains)
}

func TestHostDrain_NeedsTheHostID(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	_, err := run(t, op.args(f, "host", "drain")...)
	require.Error(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.drains, "nothing was sent")
}
