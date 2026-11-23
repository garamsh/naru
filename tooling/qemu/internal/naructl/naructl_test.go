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
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+out+"\n"), 0o755))
	return naructl.Tool{Bin: bin}, out
}

func TestGenNode_PassesEveryFieldAsItsFlagAndTheBinariesInNameOrder(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenNode(naructl.Node{
		CA: "/ca", Name: "node-1", ControlPlane: true, Address: "10.78.0.1/24", APIServer: "10.78.0.1",
		MachinePrefix: "10.200.1.0/24", Firecracker: "ab", Out: "/o.yaml",
		ServingAddresses: []string{"127.0.0.1"}, ExtraNIC: "02:00:00:01:00:01=10.0.2.15/24",
		TimeServers: []string{"10.0.2.2:5123", "pool.example"}, Storage: []string{"vmdisks=/dev/vdb", "scratch=/dev/vdc"},
		Binaries: map[string]string{"registry": "/r", "etcd": "/e"}, Disable: []string{"gaon-scheduler"},
	}))

	b, err := os.ReadFile(out)
	require.NoError(t, err)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	assert.Equal(t, []string{"gen", "node", "--ca-dir", "/ca", "--name", "node-1", "--address", "10.78.0.1/24"}, got[:8])
	line := strings.Join(got, " ")
	for _, want := range []string{"--control-plane", "--serving-address 127.0.0.1", "--extra-nic 02:00:00:01:00:01=10.0.2.15/24", "--time-server 10.0.2.2:5123 --time-server pool.example", "--storage vmdisks=/dev/vdb --storage scratch=/dev/vdc", "--disable gaon-scheduler", "--binary etcd=/e --binary registry=/r"} {
		assert.Contains(t, line, want)
	}
	assert.NotContains(t, line, "--network-config", "no network-config unless one is asked for")
}

func TestGenNode_AFailureCarriesNaructlsOutput(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho no such guest >&2\nexit 2\n"), 0o755))

	err := naructl.Tool{Bin: bin}.GenNode(naructl.Node{})

	assert.ErrorContains(t, err, "no such guest")
}

func TestTool_WithoutABinaryIsRefused(t *testing.T) {
	assert.ErrorContains(t, naructl.Tool{}.GenCA(t.TempDir()), "no naructl binary")
}

func TestTool_LegacySpeaksTheFlagSpellingsOfAReleaseBeforeCobra(t *testing.T) {
	cases := []struct {
		name string
		run  func(naructl.Tool) error
		want string
	}{
		{"gen ca", func(tl naructl.Tool) error { return tl.GenCA("/ca") }, "gen ca --out /ca"},
		{"gen token", func(tl naructl.Tool) error { return tl.GenToken("/ca", "n") }, "gen token --ca /ca --name n"},
		{"gen client", func(tl naructl.Tool) error { _, err := tl.GenClient("/ca", "cn", "/o"); return err }, "gen client --ca /ca --cn cn --out /o"},
		{"gen etcd-client", func(tl naructl.Tool) error { return tl.GenEtcdClient("/ca", "/o") }, "gen etcd-client --ca /ca --out /o"},
		{"gen node", func(tl naructl.Tool) error { return tl.GenNode(naructl.Node{CA: "/ca", Name: "n"}) }, "gen node --ca /ca --name n"},
		{"gen kubernetes", func(tl naructl.Tool) error { return tl.GenKubernetes("/ca", "1.2.3.4:6443", "") }, "gen kubernetes --ca /ca --endpoint 1.2.3.4:6443"},
		{"gen kubernetes-node", func(tl naructl.Tool) error { return tl.GenKubernetesNode(naructl.KubernetesNode{CA: "/ca", Name: "n"}) }, "gen kubernetes-node --ca /ca --name n"},
		{"gen api-ca", func(tl naructl.Tool) error { return tl.GenAPICA("/ca") }, "gen api-ca --ca-dir /ca"},
		{"gen api-client", func(tl naructl.Tool) error { return tl.GenAPIClient("/ca", "admin", "1.2.3.4:1", "/o") }, "gen api-client --ca-dir /ca --role admin"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			legacy, out := recorder(t)
			legacy.Legacy = true
			require.NoError(t, c.run(legacy))
			b, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.Contains(t, strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " "), c.want)
		})
	}
}

func TestTool_TheCheckoutsNaructlIsDrivenWithTheCurrentSpellings(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenCA("/ca"))
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "gen ca --ca-dir /ca", strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " "))

	require.NoError(t, tool.GenToken("/ca", "n"))
	b, err = os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "gen token --ca-dir /ca --name n", strings.ReplaceAll(strings.TrimSpace(string(b)), "\n", " "))
}

func TestForRelease_OnlyAReleaseBeforeDev12IsDrivenWithTheOldSpellings(t *testing.T) {
	tests := []struct {
		version string
		legacy  bool
	}{
		{"v0.1.0-dev.9", true},
		{"v0.1.0-dev.11", true},
		{"v0.1.0-dev.12", false},
		{"v0.1.0-dev.13", false},
		{"v0.1.0-dev.100", false},
		{"v0.1.0", false},
		{"v0.2.0-dev.3", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			tool := naructl.ForRelease("/bin/naructl", tt.version)

			assert.Equal(t, tt.legacy, tool.Legacy)
			assert.Equal(t, "/bin/naructl", tool.Bin)
		})
	}
}
