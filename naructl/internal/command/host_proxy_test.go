package command_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostGrantAndRevokeProxy_AskOrchAndSayWhatTheHostHolds(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "host", "grant-proxy", "host-a")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host host-a holds the proxy role")
	out, err = run(t, op.args(f, "host", "revoke-proxy", "host-a")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host host-a holds no proxy role")
	_, err = run(t, op.args(f, "host", "grant-proxy")...)
	require.Error(t, err, "the host's ID is needed")

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Equal(t, []string{"grant host-a", "revoke host-a"}, f.grants)
}
