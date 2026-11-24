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

// naruInit builds naru-init from the node module alone, as a release would, once for the package.
func naruInit(t *testing.T) string {
	t.Helper()
	initOnce.Do(func() {
		dir, err := os.MkdirTemp("", "naru36-init-")
		if err != nil {
			initErr = err
			return
		}
		initBin = filepath.Join(dir, "naru-init")
		build := exec.Command("go", "build", "-o", initBin, "./cmd/naru-init")
		build.Dir = "../../../node"
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
	out, err := exec.Command(naruInit(t), "-validate", path).CombinedOutput()
	require.NoError(t, err, "%s: %s", path, out)
	return string(out)
}

func d(c string) string { return strings.Repeat(c, 64) }

type deployment struct {
	ca string
}

// newDeployment is an operator's authorities directory with no authority in it yet.
func newDeployment(t *testing.T) deployment {
	t.Helper()
	dep := deployment{ca: filepath.Join(t.TempDir(), "ca")}
	require.NoError(t, os.Mkdir(dep.ca, 0o700))
	return dep
}

// withHost writes the host authority, so that a Node given --host-region serves the host API.
func (dep deployment) withHost(t *testing.T) deployment {
	t.Helper()
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	return dep
}

// node runs gen node for a Node of the example deployment, with extra flags, and returns the file.
func (dep deployment) node(t *testing.T, name string, extra ...string) string {
	t.Helper()
	index := strings.TrimPrefix(name, "node-")
	out := filepath.Join(t.TempDir(), name+".yaml")
	args := []string{
		"gen", "node", "--ca-dir", dep.ca, "--name", name, "--address", "192.168.0." + index + "0/24",
		"--machine-prefix", "10.200." + index + ".0/24",
		"--firecracker-sha256", d("7"), "--out", out,
	}
	mustRun(t, append(args, extra...)...)
	return out
}

type userData struct {
	Spec struct {
		Services []struct {
			Name   string
			Binary string
			Args   []string
			Env    map[string]string
			// RestartOnChange and LogFiles are the files init watches and follows for the service.
			RestartOnChange []string `yaml:"restartOnChange"`
			LogFiles        []string `yaml:"logFiles"`
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
		Files []struct {
			Path, Mode, Content, Template string
			Restart                       []string
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

func TestGenNode_ServingAddressIsNamedByTheHostAPIsCertificate(t *testing.T) {
	dep := newDeployment(t).withHost(t)
	u := parse(t, dep.node(t, "node-1", "--host-region", "eu-1", "--serving-address", "10.1.1.1"))

	pemText, ok := u.file("/run/naru-hostd/tls.crt")
	require.True(t, ok)
	var ips []string
	for _, ip := range certOf(t, pemText).IPAddresses {
		ips = append(ips, ip.String())
	}
	assert.Equal(t, []string{"192.168.0.10", "10.1.1.1", "127.0.0.1"}, ips, "the Node's address, the serving address, then the loopback")
}

func TestGenNode_MvmdConfigNamesTheMachinePrefixAndTheFirecrackerDigestAndLeavesTheGuestToTheImage(t *testing.T) {
	dep := newDeployment(t)
	u := parse(t, dep.node(t, "node-2"))

	body, ok := u.file("/run/mvmd/config.json")
	require.True(t, ok)
	var cfg struct {
		Firecracker struct {
			Binary string `json:"binary_sha256"`
		}
		Network struct {
			Prefix   string   `json:"ipv4_prefix"`
			Gateway  string   `json:"ipv4_gateway"`
			Resolver []string `json:"resolvers"`
		}
	}
	require.NoError(t, json.Unmarshal(mvmdJSON(body), &cfg))
	assert.Equal(t, d("7"), cfg.Firecracker.Binary)
	assert.Equal(t, "10.200.2.0/24", cfg.Network.Prefix)
	assert.Equal(t, "10.200.2.1", cfg.Network.Gateway)
	assert.Equal(t, []string{"10.200.2.1"}, cfg.Network.Resolver)
	for _, f := range u.Spec.Files {
		if f.Path == "/run/mvmd/config.json" {
			assert.Equal(t, "guest", f.Template, "init renders the guest's kernels and initrds from the image it booted")
		} else {
			assert.Empty(t, f.Template, "control: no other file is a template")
		}
	}
	assert.Contains(t, body, "{{json .kernels}}")
}

func TestGenNode_NetworkConfigNamesTheCardTheRouteTheResolversAndASecondCard(t *testing.T) {
	dep := newDeployment(t)
	nc := filepath.Join(t.TempDir(), "network-config")
	dep.node(t, "node-1", "--network-config", nc, "--mac", "02:00:00:00:03:E9", "--gateway", "192.168.0.1",
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
	out, err := run(t, "gen", "node", "--ca-dir", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
		"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
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

	_, err = exec.Command(naruInit(t), "-validate", broken).CombinedOutput()
	assert.Error(t, err, "the validation rejects what init would")

	for _, bad := range [][]string{
		{"--address", "not-a-cidr"},
		{"--machine-prefix", "10.0.0.0"},
		{"--firecracker-sha256", "abc"},
	} {
		args := []string{
			"gen", "node", "--ca-dir", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
			"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
		}
		_, err := run(t, append(args, bad...)...)
		assert.Error(t, err, strings.Join(bad, " "))
	}
}

func TestGenNode_UserDataIsPrivateAndTwoRunsDifferOnlyInTheirKeysAndCertificates(t *testing.T) {
	dep := newDeployment(t).withHost(t)
	one, two := dep.node(t, "node-2", "--host-region", "eu-1"), dep.node(t, "node-2", "--host-region", "eu-1")

	info, err := os.Stat(one)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "user-data carries a key")
	a, b := parse(t, one), parse(t, two)
	assert.Equal(t, a.Spec.Services, b.Spec.Services)
	mvmdA, _ := a.file("/run/mvmd/config.json")
	mvmdB, _ := b.file("/run/mvmd/config.json")
	assert.Equal(t, mvmdA, mvmdB)
	keyA, _ := a.file("/run/naru-hostd/tls.key")
	assert.NotEmpty(t, keyA)
	keyB, _ := b.file("/run/naru-hostd/tls.key")
	assert.NotEqual(t, keyA, keyB, "each run issues a new pair")
}

func TestGenNode_OverAnExistingWorldReadableFileTheUserDataIsStillPrivate(t *testing.T) {
	dep := newDeployment(t).withHost(t)
	out := filepath.Join(t.TempDir(), "node-1.yaml")
	require.NoError(t, os.WriteFile(out, []byte("stale"), 0o644))

	mustRun(t, "gen", "node", "--ca-dir", dep.ca, "--name", "node-1", "--host-region", "eu-1", "--address", "192.168.0.10/24",
		"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", d("7"), "--out", out)

	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the user-data carries naru-hostd's key")
	validate(t, out)
}

func TestGenNode_ANameThatIsNoDNSLabelIsRefused(t *testing.T) {
	dep := newDeployment(t)
	for _, name := range []string{"Node_1", "node 1", "-node", "node-", "a.b", strings.Repeat("a", 64)} {
		out, err := run(t, "gen", "node", "--ca-dir", dep.ca, "--name", name, "--address", "192.168.0.10/24",
			"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"))
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
		{"--host-label", "disk=ssd", "--host-region"},
	} {
		args := []string{
			"gen", "node", "--ca-dir", dep.ca, "--name", "node-1", "--address", "192.168.0.10/24",
			"--machine-prefix", "10.200.1.0/24", "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "x"),
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

func serviceOf(t *testing.T, u userData, name string) (svc struct {
	Name            string
	Binary          string
	Args            []string
	Env             map[string]string
	RestartOnChange []string `yaml:"restartOnChange"`
	LogFiles        []string `yaml:"logFiles"`
}) {
	t.Helper()
	for _, s := range u.Spec.Services {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no %s service", name)
	return svc
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
	require.NoError(t, json.Unmarshal(mvmdJSON(cfg), &m))
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
	for _, k := range []string{"AWS_CONFIG_FILE", "AWS_EC2_METADATA_SERVICE_ENDPOINT", "AWS_REGION"} {
		assert.NotContains(t, mvmdOf(t, u), k)
	}
	assert.Equal(t, "/run/naru/registry/home", mvmdOf(t, u)["HOME"])
	for _, s := range u.Spec.Services {
		assert.NotEqual(t, "aws-credentials", s.Name, "a Node without spec.aws runs no aws-credentials")
	}
}

func TestGenNode_DisableRegistryLeavesTheRegistryOutAndNoOtherServiceCanBeLeftOut(t *testing.T) {
	dep := newDeployment(t)

	assert.Equal(t, []string{"mvmd", "registry"}, parse(t, dep.node(t, "node-1")).names(), "the control: a Node runs the registry")
	assert.Equal(t, []string{"mvmd"}, parse(t, dep.node(t, "node-2", "--disable", "registry")).names())
	for _, s := range []string{"mvmd", "etcd"} {
		out, err := run(t, "gen", "node", "--ca-dir", dep.ca, "--name", "node-3", "--address", "192.168.0.12/24",
			"--machine-prefix", "10.200.3.0/24", "--firecracker-sha256", strings.Repeat("7", 64),
			"--out", filepath.Join(t.TempDir(), "n.yaml"), "--disable", s)
		require.Error(t, err, out)
		assert.Contains(t, err.Error(), "registry is the only service")
	}
}

// awsHost are the flags of a host with an IAM Roles Anywhere profile.
var awsHost = []string{"--host-region", "eu-1", "--aws-region", "ap-northeast-2", "--aws-trust-anchor", "arn:ta", "--aws-profile", "arn:pr", "--aws-role", "arn:ro"}

func TestGenNode_AWSFlagsGiveNaruHostdsPairAndMvmdsEnvironment(t *testing.T) {
	u := parse(t, newDeployment(t).withHost(t).node(t, "node-2", awsHost...))

	require.NotNil(t, u.Spec.AWS)
	assert.Equal(t, "/run/naru-hostd/tls.crt", u.Spec.AWS.Certificate)
	assert.Equal(t, "/run/naru-hostd/tls.key", u.Spec.AWS.Key)
	assert.Equal(t, "arn:ro", u.Spec.AWS.RoleArn)
	env := mvmdOf(t, u)
	assert.Equal(t, map[string]string{
		"PATH": "/bin:/run/bin", "HOME": "/run/naru/registry/home",
		"AWS_EC2_METADATA_SERVICE_ENDPOINT": "http://127.0.0.1:9911", "AWS_REGION": "ap-northeast-2",
	}, env, "no credential_process file: the SDK reads the credential from the endpoint")
}

func TestGenNode_AWSFlagsAddTheAWSCredentialsServiceLast(t *testing.T) {
	u := parse(t, newDeployment(t).withHost(t).node(t, "node-2", awsHost...))

	assert.Equal(t, []string{"mvmd", "naru-hostd", "registry", "aws-credentials"}, u.names())
	svc := serviceOf(t, u, "aws-credentials")
	cert := "/run/naru-hostd/tls.crt"
	assert.Equal(t, "/bin/aws_signing_helper", svc.Binary)
	assert.Equal(t, []string{
		"serve", "--certificate", cert, "--private-key", "/run/naru-hostd/tls.key",
		"--trust-anchor-arn", "arn:ta", "--profile-arn", "arn:pr", "--role-arn", "arn:ro",
		"--region", "ap-northeast-2", "--port", "9911",
	}, svc.Args)
	assert.Equal(t, []string{cert}, svc.RestartOnChange, "a certificate rotate-host replaces restarts the helper")
	assert.Empty(t, svc.Env, "the helper has no environment of its own")
	assert.Equal(t, []string{"/run/naru/registry/home/.ecr/log/ecr-login.log"}, serviceOf(t, u, "mvmd").LogFiles)
}

func TestGenNode_RefusesARegistryOrAWSFlagThatIsWrong(t *testing.T) {
	dep := newDeployment(t).withHost(t)
	base := []string{"gen", "node", "--ca-dir", dep.ca, "--name", "node-2", "--address", "192.168.0.20/24",
		"--machine-prefix", "10.200.2.0/24", "--firecracker-sha256", d("7"), "--out", filepath.Join(t.TempDir(), "o.yaml")}
	_, err := run(t, append(append([]string{}, base...), awsHost...)...)
	require.NoError(t, err, "the control: a host with the four --aws flags is accepted")
	for name, extra := range map[string][]string{
		"a helper and a username":     {"--registry", "host=r.example,helper=h,username=u"},
		"a username without password": {"--registry", "host=r.example,username=u"},
		"an unknown key":              {"--registry", "host=r.example,colour=red"},
		"a ca that is not there":      {"--registry", "host=r.example,ca=/nonexistent"},
		"some of the aws flags":       {"--aws-region", "r"},
		"the aws flags with no host":  awsHost[2:],
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
	_, err = run(t, "gen", "node", "--ca-dir", "/nonexistent")
	assert.Error(t, err, "the required flags are checked first")
}
