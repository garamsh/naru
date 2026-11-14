package release_test

import (
	"archive/tar"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/guest"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
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

var (
	naructlOnce sync.Once
	naructlBin  string
	naructlErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if naructlBin != "" {
		_ = os.RemoveAll(filepath.Dir(naructlBin))
	}
	os.Exit(code)
}

// tool builds naructl from the checkout once, static, as a release does, and returns it.
func tool(t *testing.T) naructl.Tool {
	t.Helper()
	naructlOnce.Do(func() {
		dir, err := os.MkdirTemp("", "naru36-naructl-")
		if err != nil {
			naructlErr = err
			return
		}
		naructlBin = filepath.Join(dir, "naructl")
		naructlErr = naructl.Build(repoRoot(t), naructlBin)
	})
	require.NoError(t, naructlErr)
	return naructl.Tool{Bin: naructlBin}
}

// guestFile writes the guest manifest of inputs where naructl reads it.
func guestFile(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(inputs.Guest)
	require.NoError(t, err)
	p := filepath.Join(t.TempDir(), "guest.json")
	require.NoError(t, os.WriteFile(p, b, 0o600))
	return p
}

// userDataFiles is release.UserData with naructl and the test inputs.
func userDataFiles(t *testing.T) (map[string][]byte, error) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, fixture.WriteRoot(dir))
	b, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	testCA = string(b)
	return release.UserData(tool(t), guestFile(t), inputs.FirecrackerSHA256, filepath.Join(dir, "ca.pem"))
}

// testCA is the registry authority of the user-data the tests last asked for.
var testCA string

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
	files, err := userDataFiles(t)
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
	files, err := userDataFiles(t)
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
		Files   []struct{ Path, Content string }
		Network struct {
			Routes []struct{ To, Via string }
		}
		Registries []struct{ Host, CA string }
		PKI        *struct {
			Authorities []struct {
				Certificate, Key string
				KeyPath          string `yaml:"keyPath"`
			}
			Certificates []struct {
				Name        string
				IPAddresses []string `yaml:"ipAddresses"`
			}
		}
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
	files, err := userDataFiles(t)
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

func TestUserData_RoutesToTheOtherNodesMachinePrefixAndKeepsTheSourceAddress(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)

	for _, tc := range []struct {
		file, to, via string
	}{
		{"naru-node-1.test.yaml", "10.200.2.0/24", "192.168.0.61"},
		{"naru-node-2.test.yaml", "10.200.1.0/24", "192.168.0.40"},
	} {
		n := parseUserData(t, files[tc.file])
		require.Len(t, n.Spec.Network.Routes, 1, tc.file)
		assert.Equal(t, tc.to, n.Spec.Network.Routes[0].To, tc.file)
		assert.Equal(t, tc.via, n.Spec.Network.Routes[0].Via, tc.file)
		var mvmd struct {
			Network struct {
				NoMasquerade []string `json:"no_masquerade"`
			}
		}
		require.NoError(t, json.Unmarshal([]byte(n.file("/run/mvmd/config.json")), &mvmd), tc.file)
		assert.Equal(t, []string{tc.to}, mvmd.Network.NoMasquerade, tc.file)
	}
}

func TestUserData_HasTheServicesOfTheGaonNodesAndNoStandIn(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)

	for i := 1; i <= 2; i++ {
		got, err := release.ServiceBinaries(files[release.UserDataName(i)])
		require.NoError(t, err)

		assert.NotContains(t, got, "standin", "a deployed Node runs no stand-in")
		assert.NotContains(t, got, "control")
		assert.NotContains(t, got, "probe")
		assert.Contains(t, got, "registry", "the scheduler may bind a Machine to either Node")
		assert.Contains(t, got, "gaon-node-agent")
	}
}

func TestUserData_PublishesTheTestAuthorityOnNodeOneAndNoKeyOnNodeTwoAndNoOperatorsPair(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)
	first, second := parseUserData(t, files["naru-node-1.test.yaml"]), parseUserData(t, files["naru-node-2.test.yaml"])

	require.NotNil(t, first.Spec.PKI, "node-1 issues its pairs at boot")
	require.Len(t, first.Spec.PKI.Authorities, 1)
	authority := first.Spec.PKI.Authorities[0]
	keyBlock, _ := pem.Decode([]byte(authority.Key))
	require.NotNil(t, keyBlock, "the test authority's private key is published, as ADR 0016 says")
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	require.NoError(t, err)
	caBlock, _ := pem.Decode([]byte(authority.Certificate))
	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	require.NoError(t, err)
	assert.True(t, key.PublicKey.Equal(caCert.PublicKey), "it is the key of the CA node-2 trusts")
	assert.Equal(t, "/run/gaon/ca.key", authority.KeyPath, "the controllers sign with it")
	assert.Equal(t, authority.Certificate, second.file("/run/gaon/ca.pem"), "both Nodes trust one authority")

	assert.Nil(t, second.Spec.PKI, "node-2 carries no authority")
	for name, content := range files {
		assert.NotContains(t, string(content), "client.key", name)
	}
	for _, f := range second.Spec.Files {
		assert.NotContains(t, f.Content, "PRIVATE KEY", "node-2 holds no key: %s", f.Path)
	}
	for _, f := range first.Spec.Files {
		assert.NotContains(t, f.Content, "PRIVATE KEY", "node-1's files hold no key, its pairs are issued by init: %s", f.Path)
	}
}

func TestUserData_EveryNodeHasATokenAndNodeOnesAPIServerListsBoth(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)
	first, second := parseUserData(t, files["naru-node-1.test.yaml"]), parseUserData(t, files["naru-node-2.test.yaml"])

	token := regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	one, two := strings.TrimSpace(first.file("/run/gaon/bootstrap-token")), strings.TrimSpace(second.file("/run/gaon/bootstrap-token"))
	assert.Regexp(t, token, one)
	assert.Regexp(t, token, two)
	assert.NotEqual(t, one, two)
	assert.Equal(t, []string{one, two}, strings.Fields(first.file("/run/gaon/bootstrap-tokens")), "the list is the API server's")
	assert.Empty(t, second.file("/run/gaon/bootstrap-tokens"))
}

func TestUserData_TheServingCertificateIsIssuedForNodeOnesAddress(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)
	first := parseUserData(t, files["naru-node-1.test.yaml"])

	var names []string
	for _, c := range first.Spec.PKI.Certificates {
		names = append(names, c.Name)
		if c.Name == "apiserver" {
			assert.Equal(t, []string{"192.168.0.40"}, c.IPAddresses)
		}
	}
	assert.ElementsMatch(t, []string{"apiserver", "scheduler", "controllers", "etcd-server", "etcd-peer", "apiserver-etcd"}, names, "the agents' pairs come through signing requests")
}

func TestUserData_TwoCallsDifferInTheAuthorityTheTokensAndNothingElse(t *testing.T) {
	one, err := userDataFiles(t)
	require.NoError(t, err)
	two, err := userDataFiles(t)
	require.NoError(t, err)

	for name := range one {
		assert.NotEqual(t, one[name], two[name], "%s: a fresh authority and fresh tokens", name)
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

func TestMissingBinaries_NamesAServiceWhoseBinaryIsNotOnTheDataDisk(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)
	list := allInitramfsBinaries
	have := func(p string) bool { return p != fixture.GuestRoot+"/bin/naru-registry" }

	missing, err := release.MissingBinaries(files["naru-node-1.test.yaml"], have, list)

	require.NoError(t, err)
	require.Len(t, missing, 1)
	assert.Contains(t, missing[0], "registry: "+fixture.GuestRoot+"/bin/naru-registry is not on the data disk")
}

func TestMissingBinaries_NamesAServiceWhoseBinaryTheInitramfsDoesNotInstall(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)

	missing, err := release.MissingBinaries(files["naru-node-2.test.yaml"], func(string) bool { return true }, "file /bin/mvmd x\n")

	require.NoError(t, err)
	require.Len(t, missing, 5, "the file names etcd, the control plane and the Node Agent (init skips the control plane on node-2), and the list holds mvmd")
	assert.Contains(t, strings.Join(missing, "\n"), "/bin/gaon-node-agent is neither on the data disk nor in the initramfs list")
}

func TestMissingBinaries_FindsNothingMissingWhenEveryBinaryIsThere(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)
	list := allInitramfsBinaries

	for name, content := range files {
		missing, err := release.MissingBinaries(content, func(string) bool { return true }, list)

		require.NoError(t, err)
		assert.Empty(t, missing, name)
	}
}

// fakeBuildInputs writes what make image and make disk would leave, as small files: the inputs file
// with a Firecracker digest, the kernel, the guest manifest, and a boot disk.
func fakeBuildInputs(t *testing.T, firecracker string) release.Options {
	t.Helper()
	imageDir, diskDir, guestDir := t.TempDir(), t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "inputs.env"), []byte("MVMD_FIRECRACKER_SHA256="+firecracker+"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "bzImage"), []byte("kernel"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(diskDir, "naru.qcow2"), []byte("boot"), 0o600))
	// naru.img is a GPT whose first partition entry spans 2 GiB: sectors 2048 to 2048+4 Mi-1.
	img := make([]byte, 4096)
	binary.LittleEndian.PutUint64(img[1024+32:], 2048)
	binary.LittleEndian.PutUint64(img[1024+40:], 2048+4*1024*1024-1)
	require.NoError(t, os.WriteFile(filepath.Join(diskDir, "naru.img"), img, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(imageDir, "node-initrd.cpio.gz"), []byte("node initramfs"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(guestDir, "boot-sets", release.NodeBootSet), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), []byte("node kernel"), 0o600))
	d := strings.Repeat("a", 64)
	require.NoError(t, os.WriteFile(filepath.Join(guestDir, "guest.json"), []byte(`{"kernels":{"naru":{"path":"/k","sha256":"`+d+`"}},"default_kernel":"naru",
"guest_initrd":{"path":"/g","sha256":"`+d+`"},"converter_initrd":{"path":"/c","sha256":"`+d+`"}}`), 0o600))
	return release.Options{Root: repoRoot(t), ImageDir: imageDir, DiskDir: diskDir, GuestDir: guestDir, Out: t.TempDir(), Version: "v9", Naructl: tool(t).Bin}
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

func TestBuild_ShipsNaructlAsAnExecutableAssetListedInSHA256SUMS(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	assets, err := release.Build(o)

	require.NoError(t, err)
	name := "naructl-v9-linux-amd64"
	st, err := os.Stat(filepath.Join(o.Out, name))
	require.NoError(t, err, "the release holds the naructl asset")
	assert.Equal(t, os.FileMode(0o755), st.Mode().Perm())
	want, err := os.ReadFile(o.Naructl)
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(o.Out, name))
	require.NoError(t, err)
	assert.Equal(t, want, got, "the asset is the binary that wrote the user-data")
	var listed []string
	for _, a := range assets {
		listed = append(listed, a.Name)
	}
	assert.Contains(t, listed, name)
	sums, err := os.ReadFile(filepath.Join(o.Out, "SHA256SUMS"))
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^[0-9a-f]{64}  `+regexp.QuoteMeta(name)+`$`, string(sums))
}

func TestBuild_ShipsTheUpgradeBundleListedInSHA256SUMS(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	_, err := release.Build(o)

	require.NoError(t, err)
	name := release.BundleName("v9")
	st, err := os.Stat(filepath.Join(o.Out, name))
	require.NoError(t, err, "the release holds the upgrade bundle")
	assert.Positive(t, st.Size())
	sums, err := os.ReadFile(filepath.Join(o.Out, "SHA256SUMS"))
	require.NoError(t, err)
	assert.Regexp(t, `(?m)^[0-9a-f]{64}  `+regexp.QuoteMeta(name)+`$`, string(sums), "SHA256SUMS lists it")
}

func TestBuild_TheBundleManifestRecordsTheESPOfTheBootDisk(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	_, err := release.Build(o)

	require.NoError(t, err)
	f, err := os.Open(filepath.Join(o.Out, release.BundleName("v9")))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		require.NoError(t, err, "the bundle holds a manifest")
		if h.Name != release.BundleManifest {
			continue
		}
		var m release.BundleManifestJSON
		require.NoError(t, json.NewDecoder(tr).Decode(&m))
		assert.EqualValues(t, 2<<30, m.ESP.Bytes, "the partition of the disk's GPT")
		return
	}
}

func hexOf(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestBuild_ShipsTheNodeBootSetAsKernelInitramfsAndAManifestThatNamesTheirDigests(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))

	assets, err := release.Build(o)

	require.NoError(t, err)
	kernel, err := os.ReadFile(filepath.Join(o.Out, "naru-node-kernel-v9"))
	require.NoError(t, err, "the release holds the boot set's kernel")
	assert.Equal(t, "node kernel", string(kernel))
	initramfs, err := os.ReadFile(filepath.Join(o.Out, "naru-node-initramfs-v9"))
	require.NoError(t, err, "the release holds the boot set's initramfs")
	assert.Equal(t, "node initramfs", string(initramfs))
	b, err := os.ReadFile(filepath.Join(o.Out, "naru-node-v9.json"))
	require.NoError(t, err, "the release holds the boot set's manifest")
	var m release.BootSetJSON
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, release.BootSetJSON{
		Name: "naru-node-v9", Version: "v9",
		Kernel:    release.BootSetFileJSON{File: "naru-node-kernel-v9", SHA256: hexOf(kernel), Bytes: int64(len(kernel))},
		Initramfs: release.BootSetFileJSON{File: "naru-node-initramfs-v9", SHA256: hexOf(initramfs), Bytes: int64(len(initramfs))},
	}, m)
	sums, err := os.ReadFile(filepath.Join(o.Out, "SHA256SUMS"))
	require.NoError(t, err)
	for _, name := range []string{"naru-node-kernel-v9", "naru-node-initramfs-v9", "naru-node-v9.json"} {
		assert.Regexp(t, `(?m)^[0-9a-f]{64}  `+regexp.QuoteMeta(name)+`$`, string(sums), "SHA256SUMS lists "+name)
	}
	assert.Len(t, assets, 11, "the eight assets of before and the three of the boot set, which SHA256SUMS makes 12 files")
}

func TestBuild_WritesTheNotesFiguresOutsideTheAssetsWhenAsked(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))
	o.Figures = filepath.Join(t.TempDir(), "figures.env")

	_, err := release.Build(o)

	require.NoError(t, err)
	b, err := os.ReadFile(o.Figures)
	require.NoError(t, err)
	esp := int64(2 << 30)
	want := esp - esp/64 - 1<<20 - int64(len("kernel"))
	assert.Equal(t, fmt.Sprintf("ESP_BYTES=%d\nLARGEST_UPGRADE_KERNEL_BYTES=%d\n", esp, want), string(b))
	entries, err := os.ReadDir(o.Out)
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotEqual(t, "figures.env", e.Name(), "the figures are not an asset")
	}
}

func TestBootSetName_NormalizesAVersionToWhatMvmdTakesAndRefusesOneThatIsTooLong(t *testing.T) {
	for version, want := range map[string]string{
		"v0.4.0": "naru-node-v0-4-0", "V1.0.0-RC1": "naru-node-v1-0-0-rc1", "v1+build_2": "naru-node-v1-build-2",
	} {
		got, err := release.BootSetName(version)
		require.NoError(t, err, version)
		assert.Equal(t, want, got)
		assert.Regexp(t, `^[a-z0-9-]{1,63}$`, got)
	}
	_, err := release.BootSetName(strings.Repeat("a", 54))
	assert.Error(t, err, "naru-node- and 54 characters is 64")
	_, err = release.BootSetName(strings.Repeat("a", 53))
	assert.NoError(t, err, "63 is the longest")
}

func TestBuild_RefusesAKernelThePreviousReleasesNodesCannotTakeUnlessRecreationIsRequired(t *testing.T) {
	o := fakeBuildInputs(t, strings.Repeat("7", 64))
	var prev release.BundleManifestJSON
	prev.Version = "v8"
	prev.Kernel.Bytes = 1 // the formula's ESP is 64 MiB, so the 64 MiB kernel below does not fit its trial slot
	b, err := json.Marshal(prev)
	require.NoError(t, err)
	o.PrevManifest = filepath.Join(t.TempDir(), "manifest.json")
	require.NoError(t, os.WriteFile(o.PrevManifest, b, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(o.ImageDir, "bzImage"), make([]byte, 64<<20), 0o600))

	_, err = release.Build(o)

	require.ErrorContains(t, err, "does not fit the trial slot of a Node installed from v8")
	_, statErr := os.Stat(filepath.Join(o.Out, release.BundleName("v9")))
	assert.True(t, os.IsNotExist(statErr), "nothing is written for a refused release")

	o.RequiresRecreation = true
	_, err = release.Build(o)

	assert.NoError(t, err, "the same release marked as requiring recreation is built")
}

func TestUserData_EveryNodeTrustsTheRegistryServiceThroughSpecRegistries(t *testing.T) {
	files, err := userDataFiles(t)
	require.NoError(t, err)

	for name, b := range files {
		n := parseUserData(t, b)
		if assert.Len(t, n.Spec.Registries, 1, name) {
			assert.Equal(t, fixture.RegistryAddr, n.Spec.Registries[0].Host, name)
			assert.Equal(t, testCA, n.Spec.Registries[0].CA, name)
		}
	}
}
