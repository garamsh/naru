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
	"github.com/garamsh/naru/naructl/internal/layout"
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
			Env    map[string]string
			// KeptPair is the pair init sets aside when its subject lacks the Organization.
			KeptPair *struct{ File, Organization string } `yaml:"keptPair"`
		}
		Registries []struct {
			Host, CA   string
			Credential *struct{ Username, Password, Helper string }
		}
		AWS *struct {
			Region, Certificate, Key string
			TrustAnchorArn           string `yaml:"trustAnchorArn"`
			ProfileArn               string `yaml:"profileArn"`
			RoleArn                  string `yaml:"roleArn"`
		} `yaml:"aws"`
		Files []struct{ Path, Mode, Content string }
		PKI   *struct {
			Authorities []struct {
				Name, Certificate, Key, CertificatePath string
				KeyPath                                 string `yaml:"keyPath"`
			}
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

func TestGenEtcdClient_WritesTheKeysOfGaonsSecretWithAPairTheClusterCASignsForClientAuth(t *testing.T) {
	dep := newDeployment(t)
	out := filepath.Join(t.TempDir(), "etcd")

	mustRun(t, "gen", "etcd-client", "--ca", dep.ca, "--out", out)

	b, err := os.ReadFile(filepath.Join(out, "tls.crt"))
	require.NoError(t, err)
	c := certOf(t, string(b))
	assert.Equal(t, command.EtcdClientCN, c.Subject.CommonName)
	_, err = c.Verify(x509.VerifyOptions{Roots: roots(t, filepath.Join(dep.ca, "ca.pem")), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	assert.NoError(t, err, "signed by the CA etcd's members trust, for client authentication")
	ca, err := os.ReadFile(filepath.Join(out, "ca.crt"))
	require.NoError(t, err)
	want, err := os.ReadFile(filepath.Join(dep.ca, "ca.pem"))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(ca), "ca.crt is the CA that signs the members' server certificates")
	key, err := os.Stat(filepath.Join(out, "tls.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), key.Mode().Perm())
	_, err = run(t, "gen", "etcd-client", "--ca", dep.ca)
	assert.Error(t, err, "--out is required")
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
		"apiserver": {"gaon-apiserver"}, "scheduler": {"gaon-scheduler"}, "controllers": {"gaon-controllers"},
		"etcd-server": {"etcd"}, "etcd-peer": {"etcd"}, "apiserver-etcd": {"gaon-apiserver"},
	}, issued, "the agent's pair is not issued here: it comes through a signing request")
	assert.Equal(t, "/run/gaon/ca.key", u.Spec.PKI.Authorities[0].KeyPath, "the controllers sign with the key init writes")
	var files []string
	for _, f := range u.Spec.Files {
		files = append(files, f.Path)
	}
	assert.ElementsMatch(t, []string{"/run/mvmd/config.json", "/run/gaon/bootstrap-token", "/run/gaon/bootstrap-tokens"}, files, "no pair is carried as a file")
}

func TestGenNode_TheControlPlaneListsTheTokenOfEveryNodeThatHasOneAndEachNodeHoldsItsOwn(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "token", "--ca", dep.ca, "--name", "node-2")
	first := parse(t, dep.node(t, "node-1", "--control-plane"))
	second := parse(t, dep.node(t, "node-2"))

	own1, _ := first.file("/run/gaon/bootstrap-token")
	own2, _ := second.file("/run/gaon/bootstrap-token")
	list, _ := first.file("/run/gaon/bootstrap-tokens")
	assert.Regexp(t, `^[a-z0-9]{6}\.[a-z0-9]{16}\n$`, own1)
	assert.NotEqual(t, own1, own2)
	assert.Equal(t, strings.Join([]string{own1, own2}, ""), list, "node-1, then node-2, one per line")
	for _, f := range append(first.Spec.Files, second.Spec.Files...) {
		if strings.Contains(f.Path, "bootstrap-token") {
			assert.Equal(t, "0600", f.Mode, f.Path)
		}
	}
	_, ok := second.file("/run/gaon/bootstrap-tokens")
	assert.False(t, ok, "a worker holds no list")

	again := parse(t, dep.node(t, "node-2"))
	own2again, _ := again.file("/run/gaon/bootstrap-token")
	assert.Equal(t, own2, own2again, "a second gen node reads the token it made")
}

func TestGenNode_TheServicesTakeTheTokensAndTheSigner(t *testing.T) {
	dep := newDeployment(t)
	args := func(u userData, name string) string {
		for _, s := range u.Spec.Services {
			if s.Name == name {
				return strings.Join(s.Args, " ")
			}
		}
		return ""
	}
	first, second := parse(t, dep.node(t, "node-1", "--control-plane")), parse(t, dep.node(t, "node-2"))

	assert.Contains(t, args(first, "gaon-apiserver"), "--bootstrap-token-file /run/gaon/bootstrap-tokens")
	assert.Contains(t, args(first, "gaon-controllers"), "--signing-cert /run/gaon/ca.pem --signing-key /run/gaon/ca.key")
	assert.NotContains(t, args(first, "gaon-scheduler"), "--signing")
	for _, u := range []userData{first, second} {
		agent := args(u, "gaon-node-agent")
		assert.Contains(t, agent, "--tls-ca /run/gaon/ca.pem --bootstrap-token-file /run/gaon/bootstrap-token --cert-dir /var/lib/naru/gaon-node-agent")
		assert.NotContains(t, agent, "--tls-cert")
		assert.NotContains(t, agent, "--tls-key")
	}
}

func TestGenToken_NeedsTheAuthorityAndANameAndKeepsTheTokenItWrote(t *testing.T) {
	dep := newDeployment(t)
	_, err := run(t, "gen", "token", "--ca", dep.ca)
	assert.Error(t, err, "a name is required")
	_, err = run(t, "gen", "token", "--ca", t.TempDir(), "--name", "node-2")
	assert.Error(t, err, "a directory with no authority is refused")
	_, err = run(t, "gen", "token", "--ca", dep.ca, "--name", "Node_2")
	assert.Error(t, err, "the name is a DNS label")

	mustRun(t, "gen", "token", "--ca", dep.ca, "--name", "node-2")
	before, err := os.ReadFile(filepath.Join(dep.ca, "tokens", "node-2"))
	require.NoError(t, err)
	mustRun(t, "gen", "token", "--ca", dep.ca, "--name", "node-2")
	after, err := os.ReadFile(filepath.Join(dep.ca, "tokens", "node-2"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "a second run does not replace the token")
}

func TestGenNode_WorkerWithPairsAsFilesCarriesTheCACertificateAndItsOwnAgentPairAndNeverTheCAKey(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-2", "--pairs-as-files")

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
	assert.Contains(t, agentArgs, "--tls-cert /run/gaon/agent.pem --tls-key /run/gaon/agent.key")
	assert.NotContains(t, agentArgs, "--bootstrap-token-file", "an operator's pair and a token are two ways in")
	_, ok = u.file("/run/gaon/bootstrap-token")
	assert.False(t, ok)
	_, err = os.Stat(filepath.Join(dep.ca, "tokens"))
	assert.ErrorIs(t, err, os.ErrNotExist, "no token is made for a Node that has none")
}

func TestGenNode_WorkerWithATokenCarriesTheCACertificateAndTheTokenAndNoKey(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-2")

	assert.Contains(t, validate(t, path), "valid; node=naru-node-2")
	u := parse(t, path)
	assert.Nil(t, u.Spec.PKI)
	var files []string
	for _, f := range u.Spec.Files {
		files = append(files, f.Path)
	}
	assert.ElementsMatch(t, []string{"/run/mvmd/config.json", "/run/gaon/ca.pem", "/run/gaon/bootstrap-token"}, files, "a worker holds no key")
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

func TestGenNode_APIServerIsNamedByTheCertificateInitIssuesEvenWhenItIsNoAddressOfTheNode(t *testing.T) {
	dep := newDeployment(t)
	// The Node's own address is 192.168.0.20 and agents dial 192.168.0.10, a forward or a VIP.
	out := filepath.Join(t.TempDir(), "n.yaml")
	mustRun(t, "gen", "node", "--ca", dep.ca, "--name", "node-1", "--control-plane", "--address", "192.168.0.20/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out, "--serving-address", "192.168.0.10")

	u := parse(t, out)
	for _, c := range u.Spec.PKI.Certificates {
		if c.Name == "apiserver" {
			assert.Equal(t, []string{"192.168.0.10"}, c.IPAddresses, "named once although --serving-address repeats it")
			return
		}
	}
	require.Fail(t, "no apiserver certificate")
}

func TestGenNode_ServingAddressIsNamedByTheCertificateInitIssues(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-1", "--control-plane", "--serving-address", "127.0.0.1", "--serving-address", "10.1.1.1"))

	for _, c := range u.Spec.PKI.Certificates {
		if c.Name == "apiserver" {
			assert.Equal(t, []string{"192.168.0.10", "127.0.0.1", "10.1.1.1"}, c.IPAddresses, "the API server's address first, then the extra ones")
			return
		}
	}
	require.Fail(t, "no apiserver certificate")
}

func TestGenNode_DisableLeavesAServiceOutOfTheListAndOfTheRestartListsAndNaruInitAccepts(t *testing.T) {
	dep := newDeployment(t)
	path := dep.node(t, "node-1", "--control-plane", "--disable", "gaon-scheduler", "--disable", "registry")

	validate(t, path)
	u := parse(t, path)
	assert.NotContains(t, u.names(), "gaon-scheduler")
	assert.NotContains(t, u.names(), "registry")
	assert.Contains(t, u.names(), "gaon-controllers")
	for _, c := range u.Spec.PKI.Certificates {
		assert.NotContains(t, c.Restart, "gaon-scheduler", c.Name)
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
	one, two := dep.node(t, "node-2", "--pairs-as-files"), dep.node(t, "node-2", "--pairs-as-files")

	info, err := os.Stat(one)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "user-data carries a key")
	a, b := parse(t, one), parse(t, two)
	assert.Equal(t, a.Spec.Services, b.Spec.Services)
	mvmdA, _ := a.file("/run/mvmd/config.json")
	mvmdB, _ := b.file("/run/mvmd/config.json")
	assert.Equal(t, mvmdA, mvmdB)
	agentA, _ := a.file("/run/gaon/agent.key")
	assert.NotEmpty(t, agentA)
	agentB, _ := b.file("/run/gaon/agent.key")
	assert.NotEqual(t, agentA, agentB, "each run issues a new pair")
}

func TestGenNode_OverAnExistingWorldReadableFileTheUserDataIsStillPrivate(t *testing.T) {
	dep := newDeployment(t)
	out := filepath.Join(t.TempDir(), "node-1.yaml")
	require.NoError(t, os.WriteFile(out, []byte("stale"), 0o644)) //nolint:gosec // the case under test

	mustRun(t, "gen", "node", "--ca", dep.ca, "--name", "node-1", "--control-plane", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
		"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out)

	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the control-plane user-data carries the CA's key")
	validate(t, out)
}

func TestGenClient_OverAnExistingWorldReadableKeyTheKeyIsStillPrivate(t *testing.T) {
	dep := newDeployment(t)
	out := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(out, "client.key"), []byte("stale"), 0o644)) //nolint:gosec // the case under test

	mustRun(t, "gen", "client", "--ca", dep.ca, "--cn", "alice", "--out", out)

	info, err := os.Stat(filepath.Join(out, "client.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestGenNode_ANameThatIsNoDNSLabelIsRefused(t *testing.T) {
	dep := newDeployment(t)
	for _, name := range []string{"Node_1", "node 1", "-node", "node-", "a.b", strings.Repeat("a", 64)} {
		out, err := run(t, "gen", "node", "--ca", dep.ca, "--name", name, "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
			"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"))
		require.Error(t, err, name)
		assert.Contains(t, out, "not a DNS label", name)
	}
}

func TestGenNode_AFlagGivenWithoutWhatMakesItApplyIsRefused(t *testing.T) {
	dep := newDeployment(t)
	for _, c := range []struct{ flag, value, needs string }{
		{"--mac", "02:00:00:00:00:01", "--network-config"},
		{"--gateway", "192.168.0.1", "--network-config"},
		{"--nameserver", "1.1.1.1", "--network-config"},
		{"--search", "naru.test", "--network-config"},
		{"--extra-nic", "02:00:00:00:00:02=10.0.2.15/24", "--network-config"},
		{"--serving-address", "127.0.0.1", "--control-plane"},
	} {
		args := []string{
			"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--api-server", "192.168.0.10",
			"--machine-prefix", "10.200.1.0/24", "--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
			c.flag,
		}
		if c.value != "" {
			args = append(args, c.value)
		}
		out, err := run(t, args...)
		require.Error(t, err, c.flag)
		assert.Contains(t, out, c.flag+" has no effect without "+c.needs)
	}
}

func mvmdOf(t *testing.T, u userData) (env map[string]string) {
	t.Helper()
	for _, s := range u.Spec.Services {
		if s.Name == "mvmd" {
			return s.Env
		}
	}
	t.Fatal("no mvmd service")
	return nil
}

func TestGenNode_RegistryFlagsBecomeSpecRegistriesAndMvmdsRegistrySection(t *testing.T) {
	dep := newDeployment(t)
	dir := t.TempDir()
	caFile, pwFile := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "pw")
	require.NoError(t, os.WriteFile(caFile, []byte("CA-PEM\n"), 0o600))
	require.NoError(t, os.WriteFile(pwFile, []byte("hunter2\n"), 0o600))

	path := dep.node(t, "node-2",
		"--registry", "host=r.example:5000,ca="+caFile+",username=alice,password-file="+pwFile,
		"--registry", "host=1.dkr.ecr.ap-northeast-2.amazonaws.com,helper=ecr-login")
	u := parse(t, path)

	require.Len(t, u.Spec.Registries, 2)
	assert.Equal(t, "CA-PEM\n", u.Spec.Registries[0].CA)
	assert.Equal(t, "alice", u.Spec.Registries[0].Credential.Username)
	assert.Equal(t, "hunter2", u.Spec.Registries[0].Credential.Password, "the trailing newline of the file is not part of the password")
	assert.Equal(t, "ecr-login", u.Spec.Registries[1].Credential.Helper)
	cfg, ok := u.file("/run/mvmd/config.json")
	require.True(t, ok)
	var m struct{ Registry map[string]any }
	require.NoError(t, json.Unmarshal([]byte(cfg), &m))
	assert.Equal(t, "/run/naru/registry/credentials.json", m.Registry["credentials_file"])
	assert.Equal(t, "/run/naru/registry/ca.pem", m.Registry["ca_file"])
	assert.Equal(t, map[string]any{"1.dkr.ecr.ap-northeast-2.amazonaws.com": "ecr-login"}, m.Registry["credential_helpers"])
	assert.NotContains(t, mvmdOf(t, u), "SSL_CERT_FILE")
	assert.Equal(t, "/bin:/run/bin", mvmdOf(t, u)["PATH"])
}

func TestGenNode_WithoutARegistryCAMvmdNamesNoCAFile(t *testing.T) {
	u := parse(t, newDeployment(t).node(t, "node-2"))

	cfg, _ := u.file("/run/mvmd/config.json")
	assert.NotContains(t, cfg, "ca_file")
	assert.Contains(t, cfg, "credentials_file", "init always writes the credentials file")
	assert.Empty(t, u.Spec.Registries)
	assert.Nil(t, u.Spec.AWS)
	assert.NotContains(t, mvmdOf(t, u), "AWS_CONFIG_FILE")
}

func TestGenNode_AWSFlagsGiveTheAgentsPairAndMvmdsEnvironment(t *testing.T) {
	u := parse(t, newDeployment(t).node(t, "node-2",
		"--aws-region", "ap-northeast-2", "--aws-trust-anchor", "arn:ta", "--aws-profile", "arn:pr", "--aws-role", "arn:ro"))

	require.NotNil(t, u.Spec.AWS)
	assert.Equal(t, "/var/lib/naru/gaon-node-agent/node-agent.pem", u.Spec.AWS.Certificate)
	assert.Equal(t, u.Spec.AWS.Certificate, u.Spec.AWS.Key, "one file holds both")
	assert.Equal(t, "arn:ro", u.Spec.AWS.RoleArn)
	env := mvmdOf(t, u)
	assert.Equal(t, "/run/naru/aws/config", env["AWS_CONFIG_FILE"])
	assert.Equal(t, "ap-northeast-2", env["AWS_REGION"])
}

func TestGenNode_TheNodeAgentsKeptPairMustCarryTheOrganizationItsSignerRequires(t *testing.T) {
	u := parse(t, newDeployment(t).node(t, "node-2"))

	for _, s := range u.Spec.Services {
		if s.Name == "gaon-node-agent" {
			require.NotNil(t, s.KeptPair)
			assert.Equal(t, "/var/lib/naru/gaon-node-agent/node-agent.pem", s.KeptPair.File)
			assert.Equal(t, "gaon:nodes", s.KeptPair.Organization)
			return
		}
	}
	t.Fatal("no gaon-node-agent")
}

func TestGenNode_RefusesARegistryOrAWSFlagThatIsWrong(t *testing.T) {
	dep := newDeployment(t)
	base := []string{"gen", "node", "--ca", dep.ca, "--name", "node-2", "--address", "192.168.0.20/24",
		"--api-server", "192.168.0.10", "--machine-prefix", "10.200.2.0/24", "--guest", dep.guest,
		"--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "o.yaml")}
	for name, extra := range map[string][]string{
		"a helper and a username":     {"--registry", "host=r.example,helper=h,username=u"},
		"a username without password": {"--registry", "host=r.example,username=u"},
		"an unknown key":              {"--registry", "host=r.example,colour=red"},
		"a ca that is not there":      {"--registry", "host=r.example,ca=/nonexistent"},
		"some of the aws flags":       {"--aws-region", "r"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := run(t, append(append([]string{}, base...), extra...)...)
			assert.Error(t, err)
		})
	}
}

func TestRun_AnUnknownCommandIsRefused(t *testing.T) {
	_, err := run(t, "gen", "moon")
	assert.Error(t, err)
	_, err = run(t)
	assert.Error(t, err)
	_, err = run(t, "gen", "node", "--ca", "/nonexistent")
	assert.Error(t, err, "the required flags are checked first")
}

func TestTopology_PrintsTheLayoutsServicesAsJSON(t *testing.T) {
	out, err := run(t, "topology")
	require.NoError(t, err, out)

	var got layout.Topology
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	assert.Equal(t, layout.Gaon, got)
}

func TestTopology_TakesNoArguments(t *testing.T) {
	_, err := run(t, "topology", "extra")

	require.Error(t, err)
}

// haMembers are the flags of a control plane of three at .10, .20 and .30 with the virtual address .100.
var haMembers = []string{
	"--control-plane", "--api-server", "192.168.0.100",
	"--member", "node-1=192.168.0.10", "--member", "node-2=192.168.0.20", "--member", "node-3=192.168.0.30",
}

func (u userData) args(name string) string {
	for _, s := range u.Spec.Services {
		if s.Name == name {
			return strings.Join(s.Args, " ")
		}
	}
	return ""
}

func TestGenNode_ControlPlaneOfThreeRunsEtcdOnTheMembersWithTLSAndHoldsTheVirtualAddress(t *testing.T) {
	dep := newDeployment(t)
	for _, name := range []string{"node-1", "node-2", "node-3"} {
		mustRun(t, "gen", "token", "--ca", dep.ca, "--name", name)
	}
	u := parse(t, dep.node(t, "node-2", haMembers...))

	assert.Equal(t, []string{"mvmd", "registry", "fence", "etcd", "gaon-apiserver", "vip", "gaon-scheduler", "gaon-controllers", "gaon-node-agent"}, u.names())
	etcd := u.args("etcd")
	assert.Contains(t, etcd, "--listen-client-urls https://192.168.0.20:2379,https://127.0.0.1:2379 --advertise-client-urls https://192.168.0.20:2379")
	assert.Contains(t, etcd, "--listen-peer-urls https://192.168.0.20:2380 --initial-advertise-peer-urls https://192.168.0.20:2380")
	assert.Contains(t, etcd, "--initial-cluster node-1=https://192.168.0.10:2380,node-2=https://192.168.0.20:2380,node-3=https://192.168.0.30:2380")
	assert.Contains(t, etcd, "--client-cert-auth --trusted-ca-file /run/gaon/ca.pem")
	assert.Contains(t, etcd, "--peer-client-cert-auth --peer-trusted-ca-file /run/gaon/ca.pem")
	assert.Contains(t, etcd, "--enable-grpc-gateway=false")
	assert.Contains(t, etcd, "--peer-cert-file /run/gaon/etcd-server.pem --peer-key-file /run/gaon/etcd-server.key --peer-client-cert-file /run/gaon/etcd-peer.pem --peer-client-key-file /run/gaon/etcd-peer.key")
	api := u.args("gaon-apiserver")
	assert.Contains(t, api, "--etcd-endpoints https://192.168.0.10:2379,https://192.168.0.20:2379,https://192.168.0.30:2379 ", "every member, the same list on every Node")
	assert.Contains(t, api, "--etcd-ca-file /run/gaon/ca.pem --etcd-cert-file /run/gaon/apiserver-etcd.pem --etcd-key-file /run/gaon/apiserver-etcd.key")
	assert.Equal(t, "-allow 192.168.0.10,192.168.0.20,192.168.0.30,192.168.0.100 -allow-machines 10.200.2.0/24", u.args("fence"))
	assert.Equal(t, "-address 192.168.0.100/24 -interface eth0 -self 192.168.0.20 -member 192.168.0.10,192.168.0.20,192.168.0.30 -port 6443 -etcd-ca-file /run/gaon/ca.pem -etcd-cert-file /run/gaon/apiserver-etcd.pem -etcd-key-file /run/gaon/apiserver-etcd.key", u.args("vip"))
	assert.Contains(t, u.args("gaon-node-agent"), "--api-server https://192.168.0.100:6443/", "the agent dials the virtual address")
	for _, c := range u.Spec.PKI.Certificates {
		if c.Name == "etcd-server" {
			assert.Equal(t, "server", c.Usage)
			assert.True(t, c.NodeAddresses)
		}
		if c.Name == "etcd-peer" {
			assert.Equal(t, "client", c.Usage)
			assert.True(t, c.NodeAddresses, "etcd checks the dialling peer's address against the certificate")
		}
		if c.Name == "apiserver" {
			assert.Contains(t, c.IPAddresses, "192.168.0.100", "the API server's certificate names the virtual address")
		}
	}
}

func TestGenNode_ALoneControlPlaneRunsEtcdOnTheLoopbackWithTLSAndNeitherFenceNorVIP(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-1", "--control-plane"))

	assert.NotContains(t, u.names(), "fence")
	assert.NotContains(t, u.names(), "vip")
	assert.Contains(t, u.args("etcd"), "--listen-client-urls https://127.0.0.1:2379 --advertise-client-urls https://127.0.0.1:2379")
	assert.Contains(t, u.args("etcd"), "--initial-cluster node-1=https://127.0.0.1:2380")
	assert.Contains(t, u.args("gaon-apiserver"), "--etcd-endpoints https://127.0.0.1:2379 ")
}

func TestGenNode_MembersOfFilesIssueEtcdPairsNamingTheNodesAddresses(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-3", append(haMembers, "--pairs-as-files", "--serving-address", "127.0.0.1")...))

	server, ok := u.file("/run/gaon/etcd-server.pem")
	require.True(t, ok)
	var ips []string
	for _, ip := range certOf(t, server).IPAddresses {
		ips = append(ips, ip.String())
	}
	assert.ElementsMatch(t, []string{"192.168.0.30", "127.0.0.1"}, ips)
	peer, ok := u.file("/run/gaon/etcd-peer.pem")
	require.True(t, ok)
	assert.Equal(t, "192.168.0.30", certOf(t, peer).IPAddresses[0].String())
	assert.Contains(t, certOf(t, peer).ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	for _, f := range []string{"etcd-server.key", "etcd-peer.key", "apiserver-etcd.key"} {
		for _, file := range u.Spec.Files {
			if file.Path == "/run/gaon/"+f {
				assert.Equal(t, "0600", file.Mode)
			}
		}
	}
	_, ok = u.file("/run/gaon/apiserver-etcd.pem")
	assert.True(t, ok)
}

func TestGenNode_RefusesMembersTheNodeCannotBeOneOf(t *testing.T) {
	dep := newDeployment(t)
	out := filepath.Join(t.TempDir(), "x.yaml")
	gen := func(extra ...string) error {
		_, err := run(t, append([]string{
			"gen", "node", "--ca", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24", "--machine-prefix", "10.200.1.0/24",
			"--guest", dep.guest, "--firecracker-sha256", d("7"), "--out", out,
		}, extra...)...)
		return err
	}
	assert.ErrorContains(t, gen("--api-server", "192.168.0.100", "--member", "node-1=192.168.0.10"), "has no effect without --control-plane")
	assert.ErrorContains(t, gen("--control-plane", "--api-server", "192.168.0.100", "--member", "node-2=192.168.0.20", "--member", "node-3=192.168.0.30"), "not among its members")
	assert.ErrorContains(t, gen("--control-plane", "--api-server", "192.168.0.20", "--member", "node-1=192.168.0.10", "--member", "node-2=192.168.0.20"), "virtual address")
	assert.ErrorContains(t, gen("--control-plane", "--api-server", "192.168.0.100", "--member", "node-1=192.168.0.10", "--member", "node-1=192.168.0.20"), "repeats")
	assert.Error(t, gen("--control-plane", "--api-server", "192.168.0.100", "--member", "node-1"), "a member is <name>=<address>")
}
