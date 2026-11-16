package command_test

import (
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const removal = "v0.1.0-dev.13"

// deprecation is the line naructl prints to stderr when a command line uses the old spelling.
func deprecation(old, new string) string {
	return "naructl: --" + old + " is deprecated, use --" + new + " (it stops working in " + removal + ")"
}

func TestAlias_NodeStillAddressesTheNodeAPIAndSaysUseEndpoint(t *testing.T) {
	dep := newDeployment(t)
	apiAuthority(t, dep)
	node := newFakeNode(t, dep)

	out, err := run(t, "version", "--config", node.clientFile(t, dep, "reader"), "--node", node.addr())

	require.NoError(t, err, out)
	assert.Contains(t, out, "v9.9.9", "the old spelling still reaches the Node")
	assert.Contains(t, out, deprecation("node", "endpoint"))
	assert.Equal(t, []string{"GET /v1/version"}, node.requests)
}

func TestAlias_HostStillAddressesTheHostAPIAndSaysUseEndpoint(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"GET /v1/microvms/web": answer(200, map[string]any{"name": "web", "state": "running"}),
	})

	out, err := run(t, "microvm", "get", "--config", f.cfg, "--host", f.addr, "web")

	require.NoError(t, err, out)
	assert.Contains(t, out, deprecation("host", "endpoint"))
	assert.Equal(t, []string{"GET /v1/microvms/web"}, f.requests)
}

func TestAlias_OutOfGenCAStillNamesTheDirectoryAndSaysUseCADir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	out, err := run(t, "gen", "ca", "--out", dir)

	require.NoError(t, err, out)
	assert.FileExists(t, filepath.Join(dir, "ca.pem"))
	assert.Contains(t, out, deprecation("out", "ca-dir"))
}

func TestAlias_CAStillNamesTheAuthorityDirectoryAndSaysUseCADir(t *testing.T) {
	dep := newDeployment(t)
	for _, args := range [][]string{
		{"gen", "token", "--name", "node-2"},
		{"gen", "client", "--cn", "alice", "--out", filepath.Join(t.TempDir(), "client")},
		{"gen", "etcd-client", "--out", filepath.Join(t.TempDir(), "etcd-client")},
	} {
		out, err := run(t, append(append([]string{}, args...), "--ca", dep.ca)...)
		require.NoError(t, err, "%v: %s", args, out)
		assert.Contains(t, out, deprecation("ca", "ca-dir"), args)
	}
	out, err := run(t, "gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
		"--api-server", "192.168.0.10", "--machine-prefix", "10.200.1.0/24", "--guest", dep.guest,
		"--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "node-1.yaml"))
	require.NoError(t, err, out)
	assert.Contains(t, out, deprecation("ca", "ca-dir"))
}

func TestAlias_TheNewSpellingPrintsNoDeprecation(t *testing.T) {
	out, err := run(t, "gen", "ca", "--ca-dir", filepath.Join(t.TempDir(), "ca"))
	require.NoError(t, err, out)
	assert.NotContains(t, out, "deprecated")
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
