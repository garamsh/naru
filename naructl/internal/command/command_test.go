package command_test

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/naructl/internal/command"
)

var (
	initOnce sync.Once
	initBin  string
	initErr  error
)

// naruInit builds naru-init from the init module alone, as a release would, once for the package.
func naruInit(t *testing.T) string {
	t.Helper()
	initOnce.Do(func() {
		dir, err := os.MkdirTemp("", "naru36-init-")
		if err != nil {
			initErr = err
			return
		}
		initBin = filepath.Join(dir, "naru-init")
		build := exec.Command("go", "build", "-o", initBin, "./cmd/naru-init") //nolint:gosec // fixed arguments
		build.Dir = "../../../init"
		build.Env = append(os.Environ(), "GOWORK=off")
		if out, err := build.CombinedOutput(); err != nil {
			initErr = &buildError{err, string(out)}
		}
	})
	require.NoError(t, initErr)
	return initBin
}

type buildError struct {
	err error
	out string
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.out }

func TestMain(m *testing.M) {
	code := m.Run()
	if initBin != "" {
		_ = os.RemoveAll(filepath.Dir(initBin))
	}
	os.Exit(code)
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := command.Run(args, &out, &errOut)
	if err != nil {
		errOut.WriteString(err.Error())
	}
	return out.String() + errOut.String(), err
}

func mustRun(t *testing.T, args ...string) {
	t.Helper()
	out, err := run(t, args...)
	require.NoError(t, err, out)
}

// validate runs naru-init -validate on the file and returns its output.
func validate(t *testing.T, path string) string {
	t.Helper()
	out, err := exec.Command(naruInit(t), "-validate", path).CombinedOutput() //nolint:gosec // the binary built above
	require.NoError(t, err, "%s: %s", path, out)
	return string(out)
}

func d(c string) string { return strings.Repeat(c, 64) }

// guestJSON writes a guest manifest of two kernels and returns its path.
func guestJSON(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "guest.json")
	require.NoError(t, os.WriteFile(p, []byte(`{"kernels":{"naru":{"path":"/k1","sha256":"`+d("a")+`"},"alt":{"path":"/k2","sha256":"`+d("b")+`"}},
"default_kernel":"naru","guest_initrd":{"path":"/g","sha256":"`+d("c")+`"},"converter_initrd":{"path":"/c","sha256":"`+d("d")+`"}}`), 0o600))
	return p
}

type deployment struct {
	ca, guest string
}

func newDeployment(t *testing.T) deployment {
	t.Helper()
	dep := deployment{ca: filepath.Join(t.TempDir(), "ca"), guest: guestJSON(t)}
	mustRun(t, "gen", "ca", "--out", dep.ca)
	return dep
}

// node runs gen node for a Node of the example deployment, with extra flags, and returns the file.
func (dep deployment) node(t *testing.T, name string, extra ...string) string {
	t.Helper()
	index := strings.TrimPrefix(name, "node-")
	out := filepath.Join(t.TempDir(), name+".yaml")
	args := []string{
		"gen", "node", "--ca", dep.ca, "--name", name, "--address", "192.168.0." + index + "0/24",
		"--api-server", "192.168.0.10", "--machine-prefix", "10.200." + index + ".0/24",
		"--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out,
	}
	mustRun(t, append(args, extra...)...)
	return out
}

type userData struct {
	Spec struct {
		ControlPlane bool `yaml:"controlPlane"`
		Services     []struct {
			Name   string
			Binary string
			Args   []string
		}
		Files []struct{ Path, Mode, Content string }
		PKI   *struct {
			Authorities  []struct{ Name, Certificate, Key, CertificatePath string }
			Certificates []struct {
				Name, Usage   string
				CommonName    string   `yaml:"commonName"`
				IPAddresses   []string `yaml:"ipAddresses"`
				NodeAddresses bool     `yaml:"nodeAddresses"`
				Restart       []string
			}
		}
	}
}

func parse(t *testing.T, path string) userData {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var u userData
	require.NoError(t, yaml.Unmarshal(b, &u))
	return u
}

func (u userData) file(path string) (string, bool) {
	for _, f := range u.Spec.Files {
		if f.Path == path {
			return f.Content, true
		}
	}
	return "", false
}

func (u userData) names() []string {
	var out []string
	for _, s := range u.Spec.Services {
		out = append(out, s.Name)
	}
	return out
}

func certOf(t *testing.T, pemText string) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode([]byte(pemText))
	require.NotNil(t, b)
	c, err := x509.ParseCertificate(b.Bytes)
	require.NoError(t, err)
	return c
}

func roots(t *testing.T, caPath string) *x509.CertPool {
	t.Helper()
	b, err := os.ReadFile(caPath)
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(b))
	return pool
}

func TestGenCA_WritesTheCertificateReadableAndTheKeyPrivateAndRefusesToReplaceThem(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")

	mustRun(t, "gen", "ca", "--out", dir)

	cert, err := os.Stat(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	key, err := os.Stat(filepath.Join(dir, "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), cert.Mode().Perm())
	assert.Equal(t, os.FileMode(0o600), key.Mode().Perm(), "the CA's key is private")
	b, err := os.ReadFile(filepath.Join(dir, "ca.pem"))
	require.NoError(t, err)
	assert.True(t, certOf(t, string(b)).IsCA)

	out, err := run(t, "gen", "ca", "--out", dir)
	require.Error(t, err, "a second gen ca would lose the first's key")
	assert.Contains(t, out, "already holds an authority")
}

func TestGenClient_IssuesAClientPairTheCAVerifiesWithTheNamedCN(t *testing.T) {
	dep := newDeployment(t)
	out := filepath.Join(t.TempDir(), "me")

	mustRun(t, "gen", "client", "--ca", dep.ca, "--cn", "alice", "--out", out)

	b, err := os.ReadFile(filepath.Join(out, "client.pem"))
	require.NoError(t, err)
	c := certOf(t, string(b))
	assert.Equal(t, "alice", c.Subject.CommonName)
	_, err = c.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err)
	key, err := os.Stat(filepath.Join(out, "client.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), key.Mode().Perm())
	_, err = run(t, "gen", "client", "--ca", dep.ca, "--out", out)
	assert.Error(t, err, "a client needs a CN")
}

func TestGenNode_ControlPlaneIssuesFromSpecPKIAndCarriesTheCAKeyOnlyThere(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--control-plane")

	assert.Contains(t, validate(t, path), "valid; node=naru-node-1")
	u := parse(t, path)
	assert.True(t, u.Spec.ControlPlane)
	assert.Equal(t, []string{"mvmd", "registry", "etcd", "gaon-apiserver", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}, u.names())
	require.NotNil(t, u.Spec.PKI)
	caKey, err := os.ReadFile(filepath.Join(dep.ca, "ca.key"))
	require.NoError(t, err)
	assert.Equal(t, string(caKey), u.Spec.PKI.Authorities[0].Key, "the control-plane Node carries the CA's key")
	issued := map[string][]string{}
	for _, c := range u.Spec.PKI.Certificates {
		issued[c.Name] = c.Restart
		if c.Name == "agent" {
			assert.Equal(t, "node-1", c.CommonName)
		}
		if c.Name == "apiserver" {
			assert.True(t, c.NodeAddresses)
			assert.Equal(t, "server", c.Usage)
		}
	}
	assert.Equal(t, map[string][]string{
		"apiserver": {"gaon-apiserver"}, "scheduler": {"gaon-scheduler"}, "controllers": {"gaon-controllers"}, "agent": {"gaon-node-agent"},
	}, issued)
	for _, f := range u.Spec.Files {
		assert.NotContains(t, f.Path, "/run/gaon/", "no pair is carried as a file")
	}
}

func TestGenNode_WorkerCarriesTheCACertificateAndItsOwnAgentPairAndNeverTheCAKey(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-2")

	assert.Contains(t, validate(t, path), "valid; node=naru-node-2")
	u := parse(t, path)
	assert.False(t, u.Spec.ControlPlane)
	assert.Nil(t, u.Spec.PKI)
	caKey, err := os.ReadFile(filepath.Join(dep.ca, "ca.key"))
	require.NoError(t, err)
	var keys []string
	for _, f := range u.Spec.Files {
		assert.NotContains(t, f.Content, strings.TrimSpace(string(caKey)), f.Path)
		if strings.HasSuffix(f.Path, ".key") {
			keys = append(keys, f.Path)
			assert.Equal(t, "0600", f.Mode)
		}
	}
	assert.Equal(t, []string{"/run/gaon/agent.key"}, keys, "a worker holds one key, its agent's")
	ca, ok := u.file("/run/gaon/ca.pem")
	require.True(t, ok)
	agent, ok := u.file("/run/gaon/agent.pem")
	require.True(t, ok)
	c := certOf(t, agent)
	assert.Equal(t, "node-2", c.Subject.CommonName)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(ca)))
	_, err = c.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "the cluster's CA signed the agent's certificate")
	agentArgs := ""
	for _, s := range u.Spec.Services {
		if s.Name == "gaon-node-agent" {
			agentArgs = strings.Join(s.Args, " ")
		}
	}
	assert.Contains(t, agentArgs, "--api-server https://192.168.0.10:6443/apis/compute.gaon.garam.sh/v1alpha1 --node-name node-2")
}

func TestGenNode_PairsAsFilesGivesTheControlPlaneItsPairsAndNoSpecPKI(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--control-plane", "--pairs-as-files", "--serving-address", "127.0.0.1")

	assert.Contains(t, validate(t, path), "valid; node=naru-node-1")
	u := parse(t, path)
	assert.Nil(t, u.Spec.PKI)
	for _, name := range []string{"ca", "agent", "apiserver", "scheduler", "controllers"} {
		_, ok := u.file("/run/gaon/" + name + ".pem")
		assert.True(t, ok, name)
	}
	_, ok := u.file("/run/gaon/ca.key")
	assert.False(t, ok, "the CA's key is no file of a Node")
	serving, _ := u.file("/run/gaon/apiserver.pem")
	c := certOf(t, serving)
	var ips []string
	for _, ip := range c.IPAddresses {
		ips = append(ips, ip.String())
	}
	assert.ElementsMatch(t, []string{"192.168.0.10", "127.0.0.1"}, ips, "the serving certificate names the API server's address and the extra one")
	_, err := c.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, CurrentTime: time.Now()})
	assert.NoError(t, err)
}

func TestGenNode_ServingAddressIsNamedByTheCertificateInitIssues(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-1", "--control-plane", "--serving-address", "127.0.0.1", "--serving-address", "10.1.1.1"))

	for _, c := range u.Spec.PKI.Certificates {
		if c.Name == "apiserver" {
			assert.Equal(t, []string{"127.0.0.1", "10.1.1.1"}, c.IPAddresses)
			return
		}
	}
	require.Fail(t, "no apiserver certificate")
}

func TestGenNode_DisableLeavesAServiceOutOfTheListAndOfTheRestartListsAndNaruInitAccepts(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--control-plane", "--disable", "gaon-controllers", "--disable", "registry")

	validate(t, path)
	u := parse(t, path)
	assert.NotContains(t, u.names(), "gaon-controllers")
	assert.NotContains(t, u.names(), "registry")
	assert.Contains(t, u.names(), "gaon-scheduler")
	for _, c := range u.Spec.PKI.Certificates {
		assert.NotContains(t, c.Restart, "gaon-controllers", c.Name)
	}
	out, err := run(t, "gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"), "--disable", "mvmd")
	require.Error(t, err, "mvmd cannot be left out")
	assert.Contains(t, out, `cannot disable "mvmd"`)
}

func TestGenNode_BinaryReplacesTheImagesAndOnlyThatOne(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-1", "--control-plane", "--binary", "gaon-apiserver=/var/lib/naru/inputs/gaon/gaon-apiserver"))

	bins := map[string]string{}
	for _, s := range u.Spec.Services {
		bins[s.Name] = s.Binary
	}
	assert.Equal(t, "/var/lib/naru/inputs/gaon/gaon-apiserver", bins["gaon-apiserver"])
	assert.Equal(t, "/bin/gaon-scheduler", bins["gaon-scheduler"])
	assert.Equal(t, "/bin/etcd", bins["etcd"])
	assert.Equal(t, "/var/lib/naru/inputs/bin/naru-registry", bins["registry"], "the data disk's registry")
}

func TestGenNode_MvmdConfigNamesEveryKernelTheMachinePrefixAndTheFirecrackerDigest(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-2"))

	body, ok := u.file("/run/mvmd/config.json")
	require.True(t, ok)
	var cfg struct {
		Firecracker struct {
			Kernels       map[string]struct{ Path, SHA256 string }
			DefaultKernel string `json:"default_kernel"`
			Binary        string `json:"binary_sha256"`
		}
		Network struct {
			Prefix   string   `json:"ipv4_prefix"`
			Gateway  string   `json:"ipv4_gateway"`
			Resolver []string `json:"resolvers"`
		}
	}
	require.NoError(t, json.Unmarshal([]byte(body), &cfg))
	assert.Len(t, cfg.Firecracker.Kernels, 2, "every kernel guest.json declares")
	assert.Equal(t, "naru", cfg.Firecracker.DefaultKernel)
	assert.Equal(t, d("7"), cfg.Firecracker.Binary)
	assert.Equal(t, "10.200.2.0/24", cfg.Network.Prefix)
	assert.Equal(t, "10.200.2.1", cfg.Network.Gateway)
	assert.Equal(t, []string{"10.200.2.1"}, cfg.Network.Resolver)
}

func TestGenNode_NetworkConfigNamesTheCardTheRouteTheResolversAndASecondCard(t *testing.T) {
	dep := newDeployment(t)
	nc := filepath.Join(t.TempDir(), "network-config")
	dep.node(t, "node-1", "--control-plane", "--network-config", nc, "--mac", "02:00:00:00:03:E9", "--gateway", "192.168.0.1",
		"--nameserver", "1.1.1.1", "--nameserver", "8.8.8.8", "--search", "naru.test", "--extra-nic", "02:00:00:01:00:01=10.0.2.15/24")

	b, err := os.ReadFile(nc)
	require.NoError(t, err)
	var parsed struct {
		Version int
		Config  []map[string]any
	}
	require.NoError(t, yaml.Unmarshal(b, &parsed), "a network-config is YAML")
	assert.Equal(t, 1, parsed.Version)
	require.Len(t, parsed.Config, 3, "two cards and the nameservers")
	text := string(b)
	assert.Contains(t, text, "mac_address: '02:00:00:00:03:E9'")
	assert.Contains(t, text, "address: '192.168.0.10'")
	assert.Contains(t, text, "netmask: '255.255.255.0'")
	assert.Equal(t, 1, strings.Count(text, "gateway:"), "the default route stays on the first card")
	assert.Contains(t, text, "name: eth1")
	assert.Contains(t, text, "address: '10.0.2.15'")
	assert.Contains(t, text, "- '8.8.8.8'")
	assert.Contains(t, text, "- 'naru.test'")
}

func TestGenNode_NetworkConfigNeedsTheCardAndTheRoute(t *testing.T) {
	dep := newDeployment(t)
	out, err := run(t, "gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
		"--network-config", filepath.Join(t.TempDir(), "n"))

	require.Error(t, err)
	assert.Contains(t, out, "network-config needs")
}

func TestGenNode_ABrokenDescriptionIsRefusedAndAFileInitRejectsIsNotMistakenForValid(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-2")
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	broken := filepath.Join(t.TempDir(), "broken.yaml")
	require.NoError(t, os.WriteFile(broken, []byte(strings.Replace(string(b), "kind: NodeConfig", "kind: Other", 1)), 0o600))

	_, err = exec.Command(naruInit(t), "-validate", broken).CombinedOutput() //nolint:gosec // the binary built above
	assert.Error(t, err, "the validation rejects what init would")

	for _, bad := range [][]string{
		{"--address", "not-a-cidr"},
		{"--api-server", "nope"},
		{"--machine-prefix", "10.0.0.0"},
		{"--firecracker-sha256", "abc"},
		{"--binary", "gaon-apiserver=relative"},
		{"--binary", "unknown=/x"},
	} {
		args := []string{
			"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
			"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
		}
		_, err := run(t, append(args, bad...)...)
		assert.Error(t, err, strings.Join(bad, " "))
	}
}

func TestGenNode_UserDataIsPrivateAndTwoRunsDifferOnlyInTheirKeysAndCertificates(t *testing.T) {
	dep := newDeployment(t)
	one, two := dep.node(t, "node-2"), dep.node(t, "node-2")

	info, err := os.Stat(one)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "user-data carries a key")
	a, b := parse(t, one), parse(t, two)
	assert.Equal(t, a.Spec.Services, b.Spec.Services)
	mvmdA, _ := a.file("/run/mvmd/config.json")
	mvmdB, _ := b.file("/run/mvmd/config.json")
	assert.Equal(t, mvmdA, mvmdB)
	agentA, _ := a.file("/run/gaon/agent.key")
	agentB, _ := b.file("/run/gaon/agent.key")
	assert.NotEqual(t, agentA, agentB, "each run issues a new pair")
}

func TestRun_AnUnknownCommandIsRefused(t *testing.T) {
	_, err := run(t, "gen", "moon")
	assert.Error(t, err)
	_, err = run(t)
	assert.Error(t, err)
	_, err = run(t, "gen", "node", "--ca", "/nonexistent")
	assert.Error(t, err, "the required flags are checked first")
}
