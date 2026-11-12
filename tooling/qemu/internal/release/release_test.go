package release_test

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/guest"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/pki"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// allInitramfsBinaries is an initramfs list that installs every binary the user-data names there.
const allInitramfsBinaries = "file /bin/mvmd x\nfile /bin/etcd x\nfile /bin/gaon-apiserver x\nfile /bin/gaon-scheduler x\nfile /bin/gaon-controllers x\nfile /bin/gaon-node-agent x\n"

var inputs = fixture.Inputs{
	Dir:               "/staged",
	Image:             fixture.Image{Repository: fixture.RegistryAddr + "/test/entry", Digest: "sha256:" + strings.Repeat("a", 64)},
	FirecrackerSHA256: strings.Repeat("1", 64),
	Guest: guest.Manifest{
		Kernels:         map[string]guest.File{"naru": {Path: "/usr/lib/naru/guest/kernels/naru/vmlinux", SHA256: strings.Repeat("2", 64)}},
		DefaultKernel:   "naru",
		GuestInitrd:     guest.File{Path: "/usr/lib/naru/guest/guest-initrd.cpio", SHA256: strings.Repeat("3", 64)},
		ConverterInitrd: guest.File{Path: "/usr/lib/naru/guest/converter-initrd.cpio", SHA256: strings.Repeat("4", 64)},
	},
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
		assert.Contains(t, string(out), "valid; node=naru-"+strings.TrimSuffix(strings.TrimPrefix(name, "naru-"), ".test.yaml"))
	}
}

func TestUserData_ABrokenFileIsRefusedByNaruInit(t *testing.T) {
	initBin := buildInit(t)
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "naru-node-1.test.yaml")
	require.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(files["naru-node-1.test.yaml"]), "kind: NodeConfig", "kind: Other", 1)), 0o600))

	_, err = exec.Command(initBin, "-validate", p).CombinedOutput() //nolint:gosec // the binary built above

	assert.Error(t, err, "the validation rejects what init would")
}

type userData struct {
	Spec struct {
		ControlPlane bool `yaml:"controlPlane"`
		Services     []struct {
			Name string
			Args []string
		}
		Files []struct{ Path, Content string }
	}
}

func parseUserData(t *testing.T, b []byte) userData {
	t.Helper()
	var n userData
	require.NoError(t, yaml.Unmarshal(b, &n))
	return n
}

func (u userData) file(path string) string {
	for _, f := range u.Spec.Files {
		if f.Path == path {
			return f.Content
		}
	}
	return ""
}

func TestUserData_CarriesTheDeploymentsAddressesAndRoles(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)

	for _, tc := range []struct {
		file, prefix, gateway, api string
		controlPlane               bool
	}{
		{"naru-node-1.test.yaml", "10.200.1.0/24", "10.200.1.1", "https://192.168.0.40:6443/apis/compute.gaon.garam.sh/v1alpha1", true},
		{"naru-node-2.test.yaml", "10.200.2.0/24", "10.200.2.1", "https://192.168.0.40:6443/apis/compute.gaon.garam.sh/v1alpha1", false},
	} {
		n := parseUserData(t, files[tc.file])
		assert.Equal(t, tc.controlPlane, n.Spec.ControlPlane, tc.file)
		for _, s := range n.Spec.Services {
			if s.Name == "gaon-node-agent" {
				assert.Contains(t, strings.Join(s.Args, " "), "--api-server "+tc.api, "%s: every agent dials node-1's address", tc.file)
			}
		}
		var mvmd struct {
			Network struct {
				Prefix   string   `json:"ipv4_prefix"`
				Gateway  string   `json:"ipv4_gateway"`
				Resolver []string `json:"resolvers"`
			}
		}
		require.NoError(t, json.Unmarshal([]byte(n.file("/run/mvmd/config.json")), &mvmd), tc.file)
		assert.Equal(t, tc.prefix, mvmd.Network.Prefix, tc.file)
		assert.Equal(t, tc.gateway, mvmd.Network.Gateway, tc.file)
		assert.Equal(t, []string{tc.gateway}, mvmd.Network.Resolver, tc.file)
	}
}

func TestUserData_HasTheServicesOfTheGaonNodesTheQEMUTestsBoot(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	certs, err := pki.NewCluster([]string{"node-1", "node-2"}, net.ParseIP("10.78.0.1"))
	require.NoError(t, err)

	for i := 1; i <= 2; i++ {
		qemu, err := node.Config{
			Name: release.Config(i, inputs).Name, Index: i, Total: 2, ControlPlane: i == 1, Inputs: inputs,
			Gaon: &node.Gaon{Certs: certs, APIServerIP: "10.78.0.1"},
		}.NaruYAML()
		require.NoError(t, err)

		got, err := release.ServiceBinaries(files[release.UserDataName(i)])
		require.NoError(t, err)
		want, err := release.ServiceBinaries(qemu)
		require.NoError(t, err)

		assert.Equal(t, want, got, "node-%d: the same generator, so the same services and binaries", i)
		assert.NotContains(t, got, "standin", "a deployed Node runs no stand-in")
		assert.NotContains(t, got, "control")
		assert.NotContains(t, got, "probe")
		assert.Contains(t, got, "registry", "the scheduler may bind a Machine to either Node")
		assert.Contains(t, got, "gaon-node-agent")
	}
}

func TestUserData_PublishesTheServingAndClientKeysAndNeverTheAuthoritysOrAnOperators(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	first, second := parseUserData(t, files["naru-node-1.test.yaml"]), parseUserData(t, files["naru-node-2.test.yaml"])

	keys := func(u userData) []string {
		var out []string
		for _, f := range u.Spec.Files {
			if strings.HasPrefix(f.Path, "/run/gaon/") && strings.HasSuffix(f.Path, ".key") {
				out = append(out, strings.TrimPrefix(f.Path, "/run/gaon/"))
			}
		}
		return out
	}
	assert.ElementsMatch(t, []string{"agent.key", "apiserver.key", "scheduler.key", "controllers.key"}, keys(first))
	assert.ElementsMatch(t, []string{"agent.key"}, keys(second), "node-2 holds its own agent key and no control-plane key")

	ca := first.file("/run/gaon/ca.pem")
	require.NotEmpty(t, ca)
	assert.Equal(t, ca, second.file("/run/gaon/ca.pem"), "both Nodes trust one authority")
	caBlock, _ := pem.Decode([]byte(ca))
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	require.NoError(t, err)
	for name, content := range files {
		assert.NotContains(t, string(content), "client.key", name)
		for _, block := range pemBlocks(content) {
			if block.Type != "EC PRIVATE KEY" {
				continue
			}
			key, err := x509.ParseECPrivateKey(block.Bytes)
			require.NoError(t, err)
			assert.False(t, key.PublicKey.Equal(caCert.PublicKey), "%s holds the authority's private key", name)
		}
	}
}

// pemBlocks returns the PEM blocks in text, including those a YAML scalar holds.
func pemBlocks(text []byte) []*pem.Block {
	var out []*pem.Block
	var n userData
	if yaml.Unmarshal(text, &n) != nil {
		return nil
	}
	for _, f := range n.Spec.Files {
		rest := []byte(f.Content)
		for {
			var b *pem.Block
			b, rest = pem.Decode(rest)
			if b == nil {
				break
			}
			out = append(out, b)
		}
	}
	return out
}

func TestUserData_TheServingCertificateNamesNodeOnesAddressAndTheAgentsAreSignedByTheCA(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	first := parseUserData(t, files["naru-node-1.test.yaml"])

	cert := func(path string) *x509.Certificate {
		b, _ := pem.Decode([]byte(first.file(path)))
		require.NotNil(t, b, path)
		c, err := x509.ParseCertificate(b.Bytes)
		require.NoError(t, err, path)
		return c
	}
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(first.file("/run/gaon/ca.pem"))))

	serving := cert("/run/gaon/apiserver.pem")
	assert.True(t, serving.IPAddresses[0].Equal(net.ParseIP("192.168.0.40")), "%v", serving.IPAddresses)
	_, err = serving.Verify(x509.VerifyOptions{Roots: pool, DNSName: "", CurrentTime: time.Now(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	assert.NoError(t, err, "the CA signed the serving certificate")
	agent := cert("/run/gaon/agent.pem")
	assert.Equal(t, "node-1", agent.Subject.CommonName)
	_, err = agent.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "the CA signed the agent certificate")
}

func TestUserData_TwoCallsDifferOnlyInTheCertificatesAndKeys(t *testing.T) {
	one, err := release.UserData(inputs)
	require.NoError(t, err)
	two, err := release.UserData(inputs)
	require.NoError(t, err)

	for name := range one {
		assert.NotEqual(t, one[name], two[name], "%s: fresh certificates and keys", name)
		a, b := parseUserData(t, one[name]), parseUserData(t, two[name])
		assert.Equal(t, a.Spec.Services, b.Spec.Services, name)
		for i, f := range a.Spec.Files {
			if strings.HasPrefix(f.Path, "/run/gaon/") {
				continue
			}
			assert.Equal(t, f, b.Spec.Files[i], "%s: %s is the same file", name, f.Path)
		}
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
	list := allInitramfsBinaries
	have := func(p string) bool { return p != fixture.GuestRoot+"/bin/naru-registry" }

	missing, err := release.MissingBinaries(files["naru-node-1.test.yaml"], have, list)

	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "registry: "+fixture.GuestRoot+"/bin/naru-registry is not on the data disk")
}

func TestMissingBinaries_NamesAServiceWhoseBinaryTheInitramfsDoesNotInstall(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)

	missing, err := release.MissingBinaries(files["naru-node-2.test.yaml"], func(string) bool { return true }, "file /bin/mvmd x\n")

	require.NoError(t, err)
	require.Len(t, missing, 5, "the file names etcd, the control plane and the Node Agent (init skips the control plane on node-2), and the list holds mvmd")
	assert.Contains(t, strings.Join(missing, "\n"), "/bin/gaon-node-agent is neither on the data disk nor in the initramfs list")
}

func TestMissingBinaries_FindsNothingMissingWhenEveryBinaryIsThere(t *testing.T) {
	files, err := release.UserData(inputs)
	require.NoError(t, err)
	list := allInitramfsBinaries

	for name, content := range files {
		missing, err := release.MissingBinaries(content, func(string) bool { return true }, list)

		require.NoError(t, err)
		assert.Empty(t, missing, name)
	}
}

// fakeBuildInputs writes what make image and make disk would leave, as small files: the inputs file
// with a Firecracker digest, the guest manifest, and a boot disk.
func fakeBuildInputs(t *testing.T, firecracker string) release.Options {
	t.Helper()
	imageDir, diskDir, guestDir := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "inputs.env"), []byte("MVMD_FIRECRACKER_SHA256="+firecracker+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(diskDir, "naru.qcow2"), []byte("boot"), 0o600))
	d := strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(filepath.Join(guestDir, "guest.json"), []byte(`{"kernels":{"naru":{"path":"/k","sha256":"`+d+`"}},"default_kernel":"naru",
"guest_initrd":{"path":"/g","sha256":"`+d+`"},"converter_initrd":{"path":"/c","sha256":"`+d+`"}}`), 0o600))
	return release.Options{Root: repoRoot(t), ImageDir: imageDir, DiskDir: diskDir, GuestDir: guestDir, Out: t.TempDir(), Version: "v9"}
}

func TestBuild_WritesTheInputsAssetListsItInSHA256SUMSAndCarriesWhatWasStaged(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	assets, err := release.Build(o)

	require.NoError(t, err)
	name := fixture.ReleaseInputsName("v9")
	b, err := os.ReadFile(filepath.Join(o.Out, name))
	require.NoError(t, err, "the release holds the inputs asset")
	var got fixture.ReleaseInputs
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, strings.Repeat("7", 64), got.FirecrackerSHA256, "the digest inputs.env named")
	assert.Equal(t, fixture.RegistryAddr+"/test/entry", got.ImageRepository)
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, got.ImageDigest, "the digest of the test image the data disk carries")
	var listed []string
	for _, a := range assets {
		listed = append(listed, a.Name)
	}
	assert.Contains(t, listed, name)
	sums, err := os.ReadFile(filepath.Join(o.Out, "SHA256SUMS"))
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^[0-9a-f]{64}  `+regexp.QuoteMeta(name)+`$`, string(sums), "SHA256SUMS lists it")
}
