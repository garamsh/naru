package node

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

func cfg(index, total int, control, probe bool) Config {
	return Config{
		Name: "node-" + string(rune('0'+index)), Index: index, Total: total, ControlPlane: control, Probe: probe,
		Inputs: fixture.Inputs{
			Image:             fixture.Image{Repository: "127.0.0.1:5000/test/entry", Digest: "sha256:img"},
			FirecrackerSHA256: "fc", KernelSHA256: "k", GuestInitrdSHA256: "g", ConverterInitrdSHA256: "c",
		},
	}
}

func parse(t *testing.T, c Config) nodeConfig {
	t.Helper()
	b, err := c.NaruYAML()
	require.NoError(t, err)
	var n nodeConfig
	require.NoError(t, yaml.Unmarshal(b, &n))
	return n
}

func names(n nodeConfig) []string {
	var out []string
	for _, s := range n.Spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func TestNaruYAML_DeclaresTheControlServiceOnEveryNodeAndTheFlagOnOne(t *testing.T) {
	first, second := parse(t, cfg(1, 2, true, false)), parse(t, cfg(2, 2, false, false))

	assert.True(t, first.Spec.ControlPlane)
	assert.False(t, second.Spec.ControlPlane)
	for _, n := range []nodeConfig{first, second} {
		var control *service
		for i := range n.Spec.Services {
			if n.Spec.Services[i].Name == "control" {
				control = &n.Spec.Services[i]
			}
		}
		require.NotNil(t, control, "every Node's file lists the control-plane service; init leaves it out")
		assert.True(t, control.ControlPlane)
	}
}

func TestNaruYAML_StandinsFormARing(t *testing.T) {
	var addrs []string
	for i := 1; i <= 3; i++ {
		n := parse(t, cfg(i, 3, i == 1, false))
		for _, s := range n.Spec.Services {
			if s.Name == "standin" {
				addrs = append(addrs, s.Args[5]+" -> "+s.Args[7])
			}
		}
	}

	assert.Equal(t, []string{
		"10.78.0.1:7000 -> 10.78.0.2:7000", "10.78.0.2:7000 -> 10.78.0.3:7000", "10.78.0.3:7000 -> 10.78.0.1:7000",
	}, addrs)
}

func TestNaruYAML_ProbeRunsOnlyWhereAsked(t *testing.T) {
	assert.Equal(t, []string{"mvmd", "registry", "standin", "control", "probe"}, names(parse(t, cfg(1, 2, true, true))))
	assert.Equal(t, []string{"mvmd", "registry", "standin", "control"}, names(parse(t, cfg(2, 2, false, false))))
}

func TestNaruYAML_MvmdTrustsTheRegistryAuthorityAndForwardsIPv4(t *testing.T) {
	n := parse(t, cfg(1, 2, true, false))

	assert.Equal(t, "/var/lib/mvmd/inputs/registry/ca.pem", n.Spec.Services[0].Env["SSL_CERT_FILE"])
	assert.Equal(t, []string{"ipv4"}, n.Spec.Network.Forwarding)
	assert.Equal(t, DataLabel, n.Spec.DataDisk.Label)
}

func mvmd(t *testing.T, c Config) map[string]any {
	t.Helper()
	b, err := c.MvmdJSON()
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(b, &m))
	return m
}

func TestMvmdJSON_UsesTheFirecrackerDriverAndKeepsSupervisionOffTmpfs(t *testing.T) {
	m := mvmd(t, cfg(1, 2, true, false))

	assert.Equal(t, "firecracker", m["driver"])
	sup := m["supervision"].(map[string]any)
	assert.NotContains(t, sup, "dir", "the default is under state_dir, on the data disk")
	assert.Equal(t, "/sys/fs/cgroup/naru/detached", sup["cgroup"])
	assert.Equal(t, "/var/lib/mvmd/state", m["state_dir"])
	fc := m["firecracker"].(map[string]any)
	assert.Equal(t, "fc", fc["binary_sha256"])
	assert.Equal(t, "g", fc["initrd_sha256"])
}

func TestMvmdJSON_GivesEachNodeItsOwnMachineNetwork(t *testing.T) {
	one := mvmd(t, cfg(1, 2, true, false))["network"].(map[string]any)
	two := mvmd(t, cfg(2, 2, false, false))["network"].(map[string]any)

	assert.Equal(t, "10.77.1.0/24", one["ipv4_prefix"])
	assert.Equal(t, "10.77.1.1", one["ipv4_gateway"])
	assert.Equal(t, "10.77.2.0/24", two["ipv4_prefix"])
}

func TestConfig_AddressesAndMACsDifferPerNode(t *testing.T) {
	a, b := cfg(1, 2, true, false), cfg(2, 2, false, false)

	assert.NotEqual(t, a.IP(), b.IP())
	assert.NotEqual(t, a.MAC(), b.MAC())
	assert.Equal(t, b.IP(), a.PeerIP())
	assert.Equal(t, a.IP(), b.PeerIP())
}
