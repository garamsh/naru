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

// recorder is an executable that appends its arguments, on one line, to a file beside it.
func recorder(t *testing.T) (naructl.Tool, string) {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho \"$@\" >> "+out+"\n"), 0o755))
	return naructl.Tool{Bin: bin}, out
}

func calls(t *testing.T, out string) []string {
	t.Helper()
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestGenNode_PassesEveryFieldAsItsFlag(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenNode(naructl.Node{
		CA: "/ca", Name: "node-1", Address: "10.78.0.1/24",
		MachinePrefix: "10.200.1.0/24", Firecracker: "ab", Out: "/o.yaml",
		ServingAddresses: []string{"127.0.0.1"}, ExtraNIC: "02:00:00:01:00:01=10.0.2.15/24",
		TimeServers: []string{"10.0.2.2:5123", "pool.example"}, Storage: []string{"vmdisks=/dev/vdb", "scratch=/dev/vdc"},
	}))

	got := calls(t, out)
	require.Len(t, got, 1)
	assert.True(t, strings.HasPrefix(got[0], "gen node --ca-dir /ca --name node-1 --address 10.78.0.1/24 "), got[0])
	for _, want := range []string{"--serving-address 127.0.0.1", "--extra-nic 02:00:00:01:00:01=10.0.2.15/24", "--time-server 10.0.2.2:5123 --time-server pool.example", "--storage vmdisks=/dev/vdb --storage scratch=/dev/vdc"} {
		assert.Contains(t, got[0], want)
	}
	for _, gone := range []string{"--network-config", "--api-server", "--pairs-as-files", "--disable"} {
		assert.NotContains(t, got[0], gone)
	}
}

// The naructl of a release before ADR 0114 writes a Node only from the cluster's authority and an API
// server, so it is given both and leaves out every service a Node of this checkout does not run.
func TestGenNode_AnOlderReleasesNaructlWritesAWorkerWithItsClusterServicesLeftOut(t *testing.T) {
	tool, out := recorder(t)
	tool.Gaon = true
	ca := t.TempDir()

	require.NoError(t, tool.GenNode(naructl.Node{CA: ca, Name: "node-1", Address: "10.78.0.1/24", MachinePrefix: "10.200.1.0/24", Firecracker: "ab", Out: "/o.yaml"}))

	got := calls(t, out)
	require.Len(t, got, 2)
	assert.Equal(t, "gen ca --ca-dir "+ca, got[0], "the cluster's authority first")
	for _, want := range []string{"--api-server 10.78.0.1", "--pairs-as-files", "--disable etcd --disable gaon-apiserver --disable gaon-scheduler --disable gaon-controllers --disable gaon-node-agent"} {
		assert.Contains(t, got[1], want)
	}

	require.NoError(t, os.WriteFile(filepath.Join(ca, "ca.pem"), []byte("pem"), 0o644))
	require.NoError(t, os.Remove(out))
	require.NoError(t, tool.GenNode(naructl.Node{CA: ca, Name: "node-2"}))
	assert.Len(t, calls(t, out), 1, "an authority already there is kept")
}

func TestGenNode_AFailureCarriesNaructlsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho no such guest >&2\nexit 2\n"), 0o755))

	err := naructl.Tool{Bin: bin}.GenNode(naructl.Node{})

	assert.ErrorContains(t, err, "no such guest")
}

func TestTool_WithoutABinaryIsRefused(t *testing.T) {
	assert.ErrorContains(t, naructl.Tool{}.GenAPICA(t.TempDir()), "no naructl binary")
}

func TestTool_LegacySpeaksTheFlagSpellingsOfAReleaseBeforeCobra(t *testing.T) {
	cases := []struct {
		name string
		run  func(naructl.Tool) error
		want string
	}{
		{"gen node", func(tl naructl.Tool) error { return tl.GenNode(naructl.Node{CA: "/ca", Name: "n"}) }, "gen node --ca /ca --name n"},
		{"gen ca", func(tl naructl.Tool) error {
			tl.Gaon = true
			return tl.GenNode(naructl.Node{CA: "/ca", Name: "n"})
		}, "gen ca --out /ca"},
		{"gen api-ca", func(tl naructl.Tool) error { return tl.GenAPICA("/ca") }, "gen api-ca --ca-dir /ca"},
		{"gen api-client", func(tl naructl.Tool) error { return tl.GenAPIClient("/ca", "admin", "1.2.3.4:1", "/o") }, "gen api-client --ca-dir /ca --role admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			legacy, out := recorder(t)
			legacy.Legacy = true
			require.NoError(t, c.run(legacy))
			assert.Contains(t, strings.Join(calls(t, out), " "), c.want)
		})
	}
}

func TestForRelease_DrivesEachReleasesNaructlWithWhatItKnows(t *testing.T) {
	tests := []struct {
		version      string
		legacy, gaon bool
	}{
		{"v0.1.0-dev.9", true, true},
		{"v0.1.0-dev.11", true, true},
		{"v0.1.0-dev.12", false, true},
		{"v0.1.0-dev.14", false, true},
		{"v0.1.0-dev.15", false, false},
		{"v0.1.0-dev.100", false, false},
		{"v0.1.0", false, false},
		{"v0.2.0-dev.3", false, false},
		{"", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			tool := naructl.ForRelease("/bin/naructl", tt.version)

			assert.Equal(t, tt.legacy, tool.Legacy)
			assert.Equal(t, tt.gaon, tool.Gaon)
			assert.Equal(t, "/bin/naructl", tool.Bin)
		})
	}
}
