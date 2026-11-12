package node

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/guest"
)

func sum(c string) string { return strings.Repeat(c, 64) }

// guestFiles is a manifest as image/guest/build.sh writes it, for the one kernel naru builds.
var guestFiles = guest.Manifest{
	Kernels:         map[string]guest.File{"naru": {Path: "/usr/lib/naru/guest/kernels/naru/vmlinux", SHA256: sum("k")}},
	DefaultKernel:   "naru",
	GuestInitrd:     guest.File{Path: "/usr/lib/naru/guest/guest-initrd.cpio", SHA256: sum("g")},
	ConverterInitrd: guest.File{Path: "/usr/lib/naru/guest/converter-initrd.cpio", SHA256: sum("c")},
}

func cfg(index, total int, control, probe bool) Config {
	return Config{
		Name: "node-" + string(rune('0'+index)), Index: index, Total: total, ControlPlane: control, Probe: probe,
		Inputs: fixture.Inputs{
			Image:             fixture.Image{Repository: "127.0.0.1:5000/test/entry", Digest: "sha256:img"},
			FirecrackerSHA256: "fc", Guest: guestFiles,
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

	assert.Equal(t, "/var/lib/naru/inputs/registry/ca.pem", n.Spec.Services[0].Env["SSL_CERT_FILE"])
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
	assert.Equal(t, "/var/lib/naru/mvmd", m["state_dir"])
	fc := m["firecracker"].(map[string]any)
	assert.Equal(t, "fc", fc["binary_sha256"])
}

func TestMvmdJSON_NamesTheImagesGuestFilesByTheirPathsAndDigestsInGuestJSON(t *testing.T) {
	m := mvmd(t, cfg(1, 2, true, false))

	fc := m["firecracker"].(map[string]any)
	assert.Equal(t, map[string]any{"naru": map[string]any{
		"path": "/usr/lib/naru/guest/kernels/naru/vmlinux", "sha256": sum("k"),
	}}, fc["kernels"])
	assert.Equal(t, "naru", fc["default_kernel"])
	assert.NotContains(t, fc, "kernel", "mvmd refuses the single-kernel fields")
	assert.NotContains(t, fc, "kernel_sha256")
	assert.Equal(t, "/usr/lib/naru/guest/guest-initrd.cpio", fc["initrd"])
	assert.Equal(t, sum("g"), fc["initrd_sha256"])
	conv := m["conversion"].(map[string]any)
	assert.Equal(t, "/usr/lib/naru/guest/kernels/naru/vmlinux", conv["kernel"])
	assert.Equal(t, sum("k"), conv["kernel_sha256"])
	assert.Equal(t, "/usr/lib/naru/guest/converter-initrd.cpio", conv["initrd"])
	assert.Equal(t, sum("c"), conv["initrd_sha256"])
}

func TestMvmdJSON_NamesTheMkfsExt4TheInitramfsInstallsAndNotOneOnTheDataDisk(t *testing.T) {
	m := mvmd(t, cfg(1, 2, true, false))

	mkfs := m["disk"].(map[string]any)["mkfs"].(string)
	assert.Equal(t, "/bin/mkfs.ext4", mkfs)
	assert.False(t, strings.HasPrefix(mkfs, DataMount), "the data disk carries no mkfs")
	list, err := os.ReadFile("../../../../image/initramfs/initramfs.list")
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^file `+regexp.QuoteMeta(mkfs)+` @OUT@/mkfs\.ext4 0755 `, string(list))
}

func TestMvmdJSON_WritesEveryDeclaredKernelAndTheDefaultToTheDriverAndTheDefaultToTheConverter(t *testing.T) {
	c := cfg(1, 2, true, false)
	c.Inputs.Guest = guest.Manifest{
		Kernels: map[string]guest.File{
			"naru":  {Path: "/a", SHA256: sum("a")},
			"other": {Path: "/b", SHA256: sum("b")},
		},
		DefaultKernel: "other", GuestInitrd: guestFiles.GuestInitrd, ConverterInitrd: guestFiles.ConverterInitrd,
	}

	m := mvmd(t, c)
	fc := m["firecracker"].(map[string]any)

	assert.Equal(t, map[string]any{
		"naru":  map[string]any{"path": "/a", "sha256": sum("a")},
		"other": map[string]any{"path": "/b", "sha256": sum("b")},
	}, fc["kernels"])
	assert.Equal(t, "other", fc["default_kernel"])
	conv := m["conversion"].(map[string]any)
	assert.Equal(t, "/b", conv["kernel"])
	assert.Equal(t, sum("b"), conv["kernel_sha256"])
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

func TestNaruYAML_CarriesMvmdsConfigurationAsAFileUnderRun(t *testing.T) {
	c := cfg(2, 2, false, false)
	n := parse(t, c)

	require.Len(t, n.Spec.Files, 1)
	f := n.Spec.Files[0]
	assert.Equal(t, "/run/mvmd/config.json", f.Path)
	assert.Equal(t, "0600", f.Mode)
	want, err := c.MvmdJSON()
	require.NoError(t, err)
	assert.JSONEq(t, string(want), f.Content)
	assert.Equal(t, []string{"-config", f.Path}, n.Spec.Services[0].Args)
}

func TestNetworkConfig_IsTheProxmoxFormForTheNodesAddress(t *testing.T) {
	var got struct {
		Version int `yaml:"version"`
		Config  []struct {
			Type    string   `yaml:"type"`
			Name    string   `yaml:"name"`
			MAC     string   `yaml:"mac_address"`
			Address []string `yaml:"address"`
			Search  []string `yaml:"search"`
			Subnets []struct {
				Type, Address, Netmask, Gateway string
			} `yaml:"subnets"`
		} `yaml:"config"`
	}
	c := cfg(2, 3, false, false)

	require.NoError(t, yaml.Unmarshal(c.NetworkConfig(), &got))

	assert.Equal(t, 1, got.Version)
	require.Len(t, got.Config, 2)
	nic := got.Config[0]
	assert.Equal(t, "physical", nic.Type)
	assert.Equal(t, c.MAC(), nic.MAC)
	require.Len(t, nic.Subnets, 1)
	assert.Equal(t, "static", nic.Subnets[0].Type)
	assert.Equal(t, "10.78.0.2", nic.Subnets[0].Address)
	assert.Equal(t, "255.255.255.0", nic.Subnets[0].Netmask)
	assert.Equal(t, Gateway, nic.Subnets[0].Gateway)
	assert.Equal(t, "nameserver", got.Config[1].Type)
	assert.Equal(t, []string{Gateway}, got.Config[1].Address)
}

func TestConfig_TheMACIsLocallyAdministeredAndLowerCase(t *testing.T) {
	assert.Equal(t, "02:00:00:00:00:01", cfg(1, 2, true, false).MAC())
	assert.Equal(t, "02:00:00:00:01:00", Config{Index: 256}.MAC())
}
