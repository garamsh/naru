package naructl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

// recorder is an executable that writes its arguments, one per line, to a file beside it.
func recorder(t *testing.T) (naructl.Tool, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+out+"\n"), 0o755)) //nolint:gosec // a test executable
	return naructl.Tool{Bin: bin}, out
}

func TestGenNode_PassesEveryFieldAsItsFlagAndTheBinariesInNameOrder(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenNode(naructl.Node{
		CA: "/ca", Name: "node-1", ControlPlane: true, Address: "10.78.0.1/24", APIServer: "10.78.0.1",
		MachinePrefix: "10.200.1.0/24", Guest: "/g.json", Firecracker: "ab", Out: "/o.yaml",
		ServingAddresses: []string{"127.0.0.1"}, ExtraNIC: "02:00:00:01:00:01=10.0.2.15/24",
		Binaries: map[string]string{"registry": "/r", "etcd": "/e"}, Disable: []string{"gaon-scheduler"},
	}))

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	assert.Equal(t, []string{"gen", "node", "--ca", "/ca", "--name", "node-1", "--address", "10.78.0.1/24"}, got[:8])
	line := strings.Join(got, " ")
	for _, want := range []string{"--control-plane", "--serving-address 127.0.0.1", "--extra-nic 02:00:00:01:00:01=10.0.2.15/24", "--disable gaon-scheduler", "--binary etcd=/e --binary registry=/r"} {
		assert.Contains(t, line, want)
	}
	assert.NotContains(t, line, "--network-config", "no network-config unless one is asked for")
}

func TestGenNode_AFailureCarriesNaructlsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho no such guest >&2\nexit 2\n"), 0o755)) //nolint:gosec // a test executable

	err := naructl.Tool{Bin: bin}.GenNode(naructl.Node{})

	assert.ErrorContains(t, err, "no such guest")
}

func TestTool_WithoutABinaryIsRefused(t *testing.T) {
	assert.ErrorContains(t, naructl.Tool{}.GenCA(t.TempDir()), "no naructl binary")
}
