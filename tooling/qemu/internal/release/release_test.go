package release_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

var inputs = fixture.Inputs{
	Dir:                   "/staged",
	Image:                 fixture.Image{Repository: fixture.RegistryAddr + "/test/entry", Digest: "sha256:" + strings.Repeat("a", 64)},
	FirecrackerSHA256:     strings.Repeat("1", 64),
	KernelSHA256:          strings.Repeat("2", 64),
	GuestInitrdSHA256:     strings.Repeat("3", 64),
	ConverterInitrdSHA256: strings.Repeat("4", 64),
}

// repoRoot is the directory holding go.work, above the test's working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "no go.work above the test")
		dir = parent
	}
}

// buildInit builds naru-init from the init module alone, as a release would, and returns its path.
func buildInit(t *testing.T) string {
	t.Helper()
	initBin := filepath.Join(t.TempDir(), "naru-init")
	build := exec.Command("go", "build", "-o", initBin, "./cmd/naru-init") //nolint:gosec // fixed arguments
	build.Dir = filepath.Join(repoRoot(t), "init")
	build.Env = append(os.Environ(), "GOWORK=off")
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))
	return initBin
}

func TestUserData_EachFileIsOneNaruInitParses(t *testing.T) {
	initBin := buildInit(t)
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	require.Len(t, files, 2)

	for name, content := range files {
		p := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(p, content, 0o600))

		out, err := exec.Command(initBin, "-validate", p).CombinedOutput() //nolint:gosec // the binary built above

		require.NoError(t, err, "%s: %s", name, out)
		assert.Contains(t, string(out), "valid; node=naru-"+strings.TrimSuffix(strings.TrimPrefix(name, "naru-"), ".yaml"))
	}
}

func TestUserData_ABrokenFileIsRefusedByNaruInit(t *testing.T) {
	initBin := buildInit(t)
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "naru-node-1.yaml")
	require.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(files["naru-node-1.yaml"]), "kind: NodeConfig", "kind: Other", 1)), 0o600))

	_, err = exec.Command(initBin, "-validate", p).CombinedOutput() //nolint:gosec // the binary built above

	assert.Error(t, err, "the validation rejects what init would")
}

func TestUserData_CarriesTheDeploymentsAddressesAndRoles(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)

	for _, tc := range []struct {
		file, own, peer, prefix, gateway string
		controlPlane                     bool
	}{
		{"naru-node-1.yaml", "192.168.0.40", "192.168.0.61", "10.200.1.0/24", "10.200.1.1", true},
		{"naru-node-2.yaml", "192.168.0.61", "192.168.0.40", "10.200.2.0/24", "10.200.2.1", false},
	} {
		var n struct {
			Spec struct {
				ControlPlane bool `yaml:"controlPlane"`
				Services     []struct {
					Name string
					Args []string
				}
				Files []struct{ Path, Content string }
			}
		}
		require.NoError(t, yaml.Unmarshal(files[tc.file], &n), tc.file)
		assert.Equal(t, tc.controlPlane, n.Spec.ControlPlane, tc.file)
		for _, s := range n.Spec.Services {
			if s.Name != "standin" {
				continue
			}
			args := strings.Join(s.Args, " ")
			assert.Contains(t, args, "-listen "+tc.own+":7000", tc.file)
			assert.Contains(t, args, "-peer "+tc.peer+":7000", tc.file)
		}
		require.Len(t, n.Spec.Files, 1, tc.file)
		var mvmd struct {
			Network struct {
				Prefix   string   `json:"ipv4_prefix"`
				Gateway  string   `json:"ipv4_gateway"`
				Resolver []string `json:"resolvers"`
			}
		}
		require.NoError(t, json.Unmarshal([]byte(n.Spec.Files[0].Content), &mvmd), tc.file)
		assert.Equal(t, tc.prefix, mvmd.Network.Prefix, tc.file)
		assert.Equal(t, tc.gateway, mvmd.Network.Gateway, tc.file)
		assert.Equal(t, []string{tc.gateway}, mvmd.Network.Resolver, tc.file)
	}
}

func TestUserData_HasTheServicesOfTheNodesTheQEMUTestsBoot(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)

	for i, name := range []string{"naru-node-1.yaml", "naru-node-2.yaml"} {
		qemu, err := node.Config{Name: release.Config(i+1, inputs).Name, Index: i + 1, Total: 2, ControlPlane: i == 0, Probe: i == 0, Inputs: inputs}.NaruYAML()
		require.NoError(t, err)

		got, err := release.ServiceBinaries(files[name])
		require.NoError(t, err)
		want, err := release.ServiceBinaries(qemu)
		require.NoError(t, err)

		assert.Equal(t, want, got, "%s: the same generator, so the same services and binaries", name)
	}
}

func TestProxmox_GivesTheNetworkConfigTheDeploymentsGatewayResolversAndMACs(t *testing.T) {
	first := string(release.Config(1, inputs).NetworkConfig())
	second := string(release.Config(2, inputs).NetworkConfig())

	assert.Contains(t, first, "mac_address: '02:00:00:00:03:E9'")
	assert.Contains(t, first, "address: '192.168.0.40'")
	assert.Contains(t, second, "mac_address: '02:00:00:00:03:F4'")
	assert.Contains(t, second, "address: '192.168.0.61'")
	for _, c := range []string{first, second} {
		assert.Contains(t, c, "gateway: '192.168.0.1'")
		assert.Contains(t, c, "- '1.1.1.1'")
		assert.Contains(t, c, "- '8.8.8.8'")
	}
}

func TestMissingBinaries_NamesAServiceWhoseBinaryIsNotOnTheDataDisk(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	list := "file /bin/mvmd x\nfile /bin/naru-standin x\n"
	have := func(p string) bool { return p != fixture.GuestRoot+"/bin/naru-probe" }

	missing, err := release.MissingBinaries(files["naru-node-1.yaml"], have, list)

	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "probe: "+fixture.GuestRoot+"/bin/naru-probe is not on the data disk")
}

func TestMissingBinaries_NamesAServiceWhoseBinaryTheInitramfsDoesNotInstall(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)

	missing, err := release.MissingBinaries(files["naru-node-2.yaml"], func(string) bool { return true }, "file /bin/mvmd x\n")

	require.NoError(t, err)
	require.Len(t, missing, 2, "standin and control share a binary")
	assert.Contains(t, strings.Join(missing, "\n"), "/bin/naru-standin is neither on the data disk nor in the initramfs list")
}

func TestMissingBinaries_FindsNothingMissingWhenEveryBinaryIsThere(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	list := "file /bin/mvmd x\nfile /bin/naru-standin x\n"

	for name, content := range files {
		missing, err := release.MissingBinaries(content, func(string) bool { return true }, list)

		require.NoError(t, err)
		assert.Empty(t, missing, name)
	}
}
