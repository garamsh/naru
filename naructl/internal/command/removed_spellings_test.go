package command_test

import (
	"path/filepath"
	"slices"
	"strings"
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
	} {
		out, err := run(t, args...)
		require.Error(t, err, "%v: %s", args, out)
		assert.Contains(t, out, "unknown flag", args)
	}
	assert.Empty(t, node.requests)
	assert.Empty(t, host.requests)
}

func TestGenCA_TheTwoAuthoritiesShareOneImplementation(t *testing.T) {
	dep := newDeployment(t)
	for _, c := range []struct{ cmd, sub string }{{"api-ca", "api"}, {"host-ca", "host"}} {
		out, err := run(t, "gen", c.cmd, "--ca-dir", dep.ca)
		require.NoError(t, err, out)
		assert.FileExists(t, filepath.Join(dep.ca, c.sub, "ca.pem"))
		out, err = run(t, "gen", c.cmd, "--ca-dir", dep.ca)
		require.Error(t, err, c.cmd)
		assert.Contains(t, out, "already holds", c.cmd+" refuses to replace what it made")
	}
}

// naru runs no cluster on its Nodes (ADR 0114): the commands of the cluster's authority, tokens and
// topology are gone, and gen node takes none of their flags.
func TestRun_TheClusterCommandsAndFlagsAreGone(t *testing.T) {
	dep := newDeployment(t)
	node := []string{"gen", "node", "--ca-dir", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
		"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", strings.Repeat("7", 64), "--out", filepath.Join(t.TempDir(), "n.yaml")}
	_, err := run(t, node...)
	require.NoError(t, err, "the control: gen node without them is accepted")
	for _, args := range [][]string{
		{"gen", "ca", "--ca-dir", dep.ca},
		{"gen", "client", "--ca-dir", dep.ca, "--cn", "alice", "--out", t.TempDir()},
		{"gen", "etcd-client", "--ca-dir", dep.ca, "--out", t.TempDir()},
		{"gen", "token", "--ca-dir", dep.ca, "--name", "node-2"},
		{"token", "create", "--ca-dir", dep.ca, "--name", "node-2"},
		{"topology"},
		append(slices.Clone(node), "--control-plane"),
		append(slices.Clone(node), "--api-server", "192.168.0.10"),
		append(slices.Clone(node), "--pairs-as-files"),
		append(slices.Clone(node), "--disable", "etcd"),
	} {
		out, err := run(t, args...)
		assert.Error(t, err, "%v: %s", args, out)
	}
}
