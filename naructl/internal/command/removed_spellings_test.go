package command_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemovedSpellings_AreRefusedAsUnknownFlags(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)
	host := newHostFake(t, nil)
	for _, args := range [][]string{
		{"version", "--config", node.clientFile(t, dep, "reader"), "--node", node.addr()},
		{"microvm", "get", "--config", host.cfg, "--host", host.addr, "web"},
		{"gen", "ca", "--out", filepath.Join(t.TempDir(), "ca")},
		{"gen", "token", "--name", "node-2", "--ca", dep.ca},
	} {
		out, err := run(t, args...)
		require.Error(t, err, "%v: %s", args, out)
		assert.Contains(t, out, "unknown flag", args)
	}
	assert.Empty(t, node.requests)
	assert.Empty(t, host.requests)
}

func TestGenCA_TheThreeAuthoritiesShareOneImplementation(t *testing.T) {
	dep := newDeployment(t)
	for _, c := range []struct{ cmd, sub string }{{"api-ca", "api"}, {"host-ca", "host"}} {
		out, err := run(t, "gen", c.cmd, "--ca-dir", dep.ca)
		require.NoError(t, err, out)
		assert.FileExists(t, filepath.Join(dep.ca, c.sub, "ca.pem"))
		out, err = run(t, "gen", c.cmd, "--ca-dir", dep.ca)
		require.Error(t, err, c.cmd)
		assert.Contains(t, out, "already holds", c.cmd+" refuses to replace what it made")
	}
	out, err := run(t, "gen", "ca", "--ca-dir", dep.ca)
	require.Error(t, err)
	assert.Contains(t, out, "already holds")
}
