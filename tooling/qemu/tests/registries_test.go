//go:build qemu_real

package tests_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/gaoncli"
	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The registry service of these tests asks for this user and password, as a registry that wants a
// credential does.
const (
	regUser     = "naru-user"
	regPassword = "naru-password"
)

// userDataEdit is a change to the user-data naructl wrote for a Node.
type userDataEdit func(doc *yaml.Node) error

// editUserData returns the ClusterSpec.EditUserData that applies edits to every Node's file.
func editUserData(edits ...userDataEdit) func(node.Config, string) error {
	return func(_ node.Config, file string) error {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var doc yaml.Node
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return err
		}
		for _, e := range edits {
			if err := e(&doc); err != nil {
				return err
			}
		}
		out, err := yaml.Marshal(&doc)
		if err != nil {
			return err
		}
		return os.WriteFile(file, out, 0o600)
	}
}

func field(m *yaml.Node, key string) *yaml.Node {
	for i := 0; m != nil && i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func appendTo(doc *yaml.Node, key string, entry any) error {
	spec := field(doc.Content[0], "spec")
	list := field(spec, key)
	if list == nil {
		return fmt.Errorf("the user-data has no spec.%s", key)
	}
	var n yaml.Node
	b, err := yaml.Marshal(entry)
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(b, &n); err != nil {
		return err
	}
	list.Content = append(list.Content, n.Content[0])
	return nil
}

// withFile adds a spec.files entry.
func withFile(path, mode, content string) userDataEdit {
	return func(doc *yaml.Node) error {
		return appendTo(doc, "files", map[string]string{"path": path, "mode": mode, "content": content})
	}
}

// withArgs appends args to the arguments of the service named name.
func withArgs(name string, args ...string) userDataEdit {
	return func(doc *yaml.Node) error {
		for _, s := range field(field(doc.Content[0], "spec"), "services").Content {
			if field(s, "name").Value != name {
				continue
			}
			list := field(s, "args")
			for _, a := range args {
				list.Content = append(list.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: a})
			}
			return nil
		}
		return fmt.Errorf("the user-data has no service %q", name)
	}
}

// registryEntry is a gen node --registry value for the registry service, trusting its authority.
func registryEntry(extra ...string) []string {
	ca := filepath.Join(inputs.Dir, "registry", "ca.pem")
	return []string{strings.Join(append([]string{"host=" + fixture.RegistryAddr, "ca=" + ca}, extra...), ",")}
}

// passwordFile writes password to a file of the test's directory and returns the gen node
// --registry entry that reads it.
func passwordFile(t *testing.T, password string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "password")
	require.NoError(t, os.WriteFile(f, []byte(password), 0o600))
	return "password-file=" + f
}

// pullMachine boots one Node that runs gaon with spec, admits it, and applies the Machine, whose
// image is on the Node's registry service. It returns the Node and the cluster's host API.
func pullMachine(t *testing.T, spec node.ClusterSpec) (*node.Node, *hostapi.Client, *gaoncli.Client) {
	t.Helper()
	cliBin := filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "gaon", "gaon")
	require.FileExists(t, cliBin, "make image builds gaon's CLI from the pin")
	spec.N, spec.Gaon, spec.BootPKI = 1, true, true
	c := start(t, spec)
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	cli := gaoncli.New(cliBin, c.HostAddr, c.Client)
	ctx := context.Background()
	nd := c.Nodes[0]
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=registry"))
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"))
	eventually(t, nd.Name+" is registered and Ready", func() bool {
		n, err := api.Node(ctx, nd.Name)
		return err == nil && n.Ready()
	})
	_, err = cli.Apply(ctx, t.TempDir(), fmt.Sprintf("apiVersion: compute.gaon.garam.sh/v1alpha1\nkind: Node\nmetadata:\n  name: %s\nspec:\n  admitted: true\n", nd.Name))
	require.NoError(t, err)
	eventually(t, nd.Name+" is Admitted", func() bool {
		n, err := api.Node(ctx, nd.Name)
		return err == nil && n.Status.State == "Admitted"
	})
	_, err = cli.Apply(ctx, t.TempDir(), fmt.Sprintf(machineManifest, inputs.Image.Repository, inputs.Image.Digest))
	require.NoError(t, err)
	return nd, api, cli
}

// served waits for the registry service to log the image's manifest as served: mvmd pulled it.
func served(t *testing.T, nd *node.Node) {
	t.Helper()
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "served", inputs.Image.Digest), "the registry served the manifest")
}

// notServed holds for a while that no request for the image's manifest was answered, and that mvmd
// did ask when want is not empty: the registry's log line "refused" for it.
func notServed(t *testing.T, nd *node.Node, want ...string) {
	t.Helper()
	if len(want) > 0 {
		require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, want...), "mvmd asked and was refused")
	}
	assert.Error(t, nd.WaitFor(context.Background(), 20*time.Second, "served", inputs.Image.Digest), "the registry did not serve the image")
}

var authArgs = withArgs("registry", "-user", regUser, "-password", regPassword)

// TestNode_AMachineImageIsPulledWithTheCredentialSpecRegistriesGives has the registry service ask
// for a user and password, gives the Node that credential and the registry's authority in
// spec.registries, and checks that mvmd pulled the image.
func TestNode_AMachineImageIsPulledWithTheCredentialSpecRegistriesGives(t *testing.T) {
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry("username="+regUser, passwordFile(t, regPassword)),
		EditUserData: editUserData(authArgs),
	})

	served(t, nd)
}

// TestNode_AMachineImageIsRefusedWhenSpecRegistriesGivesNoCredential is the control of the test
// above: the same registry, the same authority, and no credential. The registry logs that it
// refused the request, and never serves the image.
func TestNode_AMachineImageIsRefusedWhenSpecRegistriesGivesNoCredential(t *testing.T) {
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry(),
		EditUserData: editUserData(authArgs),
	})

	notServed(t, nd, "refused", "credential=false")
}

// TestNode_AMachineImageIsRefusedWithAWrongPassword is a second control: a credential that the
// registry does not accept.
func TestNode_AMachineImageIsRefusedWithAWrongPassword(t *testing.T) {
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry("username="+regUser, passwordFile(t, "wrong")),
		EditUserData: editUserData(authArgs),
	})

	notServed(t, nd, "refused", "credential=true")
}

// TestNode_MvmdTrustsARegistryAuthorityOnlyThroughSpecRegistries gives the Node the registry's
// credential and no authority. The registry service's certificate is signed by an authority that
// is neither in the image's bundle nor in the Node's configuration, so mvmd does not trust it and
// the registry sees no request. With the authority (the test above) the same pull succeeds.
func TestNode_MvmdTrustsARegistryAuthorityOnlyThroughSpecRegistries(t *testing.T) {
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries:   []string{"host=" + fixture.RegistryAddr + ",username=" + regUser + "," + passwordFile(t, regPassword)},
		EditUserData: editUserData(authArgs),
	})

	notServed(t, nd)
	assert.False(t, strings.Contains(nd.Console(), "credential="), "TLS failed first, so no request reached the registry")
}

// TestNode_AMachineImageIsPulledWithACredentialFromAHelperOnMvmdsPath gives the Node a stand-in
// docker-credential-<name> in spec.files, in /run/bin, which is on mvmd's PATH, and names it for
// the registry's host. The registry asks for a credential, so the pull succeeds only if mvmd ran
// the helper.
func TestNode_AMachineImageIsPulledWithACredentialFromAHelperOnMvmdsPath(t *testing.T) {
	helper := "#!" + fixture.GuestRoot + "/bin/naru-credstub\n" + regUser + ":" + regPassword + "\n"
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry("helper=naru"),
		EditUserData: editUserData(authArgs, withFile("/run/bin/docker-credential-naru", "0755", helper)),
	})

	served(t, nd)
}

// testRoot is the throwaway root make test-qemu appended to the test image's bundle.
func testRoot(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("NARU_TESTROOT")
	if dir == "" {
		t.Fatal("NARU_TESTROOT is required; make test-qemu sets it")
	}
	return dir
}

// TestNode_MvmdTrustsARootThatTheImagesBundleHolds serves the registry with a certificate that the
// throwaway root of the test image's bundle signed, and gives the Node no authority in
// spec.registries, so that the pull succeeds only through the bundle at /etc/ssl/certs.
func TestNode_MvmdTrustsARootThatTheImagesBundleHolds(t *testing.T) {
	crt, key, err := fixture.IssueServer(testRoot(t))
	require.NoError(t, err)
	nd, _, _ := pullMachine(t, node.ClusterSpec{
		Registries: []string{},
		EditUserData: editUserData(
			withFile("/run/regtest/cert.pem", "0644", string(crt)),
			withFile("/run/regtest/key.pem", "0600", string(key)),
			withArgs("registry", "-cert", "/run/regtest/cert.pem", "-key", "/run/regtest/key.pem"),
		),
	})

	served(t, nd)
}

// agentPair writes a node-agent.pem as the Node Agent keeps it, a certificate and then a PKCS#8 key,
// whose certificate has the organization org (none when empty), into dir.
func agentPair(t *testing.T, dir, org string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	subject := pkix.Name{CommonName: "node-1"}
	if org != "" {
		subject.Organization = []string{org}
	}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: subject,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: subject}, &key.PublicKey, key)
	require.NoError(t, err)
	k8, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	b := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k8})...)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "node-agent.pem"), b, 0o600))
}

// subjectOrganizations parses the first certificate of a node-agent.pem.
func subjectOrganizations(t *testing.T, b []byte) []string {
	t.Helper()
	block, _ := pem.Decode(b)
	require.NotNil(t, block, "the file holds a certificate")
	c, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	return c.Subject.Organization
}

// TestNode_ANodeThatKeepsAnAgentPairWithoutTheOrganizationEndsWithOneThatHasIt boots a Node whose
// data disk keeps a node-agent.pem whose certificate has no organization, as the first releases
// of the agent wrote it. init sets the pair aside, the agent asks for another with its bootstrap
// token, and the Node registers and is Ready. The file the agent keeps at the end has
// O=gaon:nodes, and the old one is beside it as node-agent.pem.o-less.
func TestNode_ANodeThatKeepsAnAgentPairWithoutTheOrganizationEndsWithOneThatHasIt(t *testing.T) {
	pair := t.TempDir()
	agentPair(t, pair, "")
	c := start(t, node.ClusterSpec{
		N: 1, Gaon: true, BootPKI: true,
		Configure: func(c *node.Config) { c.AgentPair = pair },
	})
	nd := c.Nodes[0]
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	ctx := context.Background()

	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "kept pair set aside", "service=gaon-node-agent"))
	eventually(t, nd.Name+" is registered and Ready", func() bool {
		n, err := api.Node(ctx, nd.Name)
		return err == nil && n.Ready()
	})

	var kept []byte
	eventually(t, "the data disk keeps a pair with the organization", func() bool {
		sn, err := nd.SnapshotData(filepath.Join(t.TempDir(), "data.img"))
		if err != nil {
			return false
		}
		kept, err = sn.Read(node.DataMount + "/gaon-node-agent/node-agent.pem")
		if err != nil {
			return false
		}
		return len(subjectOrganizations(t, kept)) > 0
	})
	assert.Equal(t, []string{"gaon:nodes"}, subjectOrganizations(t, kept))
	sn, err := nd.SnapshotData(filepath.Join(t.TempDir(), "data-after.img"))
	require.NoError(t, err)
	old, err := sn.Read(node.DataMount + "/gaon-node-agent/node-agent.pem.o-less")
	require.NoError(t, err)
	assert.Empty(t, subjectOrganizations(t, old), "the pair that was set aside is the one the data disk had")
}

// TestNode_AwsSigningHelperAcceptsTheAgentPairAsCertificateAndKey checks, with the helper the image
// carries, that one node-agent.pem serves as both --certificate and --private-key, which is how
// init's AWS profile uses it. It stops before any network call: read-certificate-data and
// sign-string only read the files.
func TestNode_AwsSigningHelperAcceptsTheAgentPairAsCertificateAndKey(t *testing.T) {
	helper := filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "regtools", "aws_signing_helper")
	require.FileExists(t, helper, "make image builds the helper from the pin")
	dir := t.TempDir()
	agentPair(t, dir, "gaon:nodes")
	pairFile := filepath.Join(dir, "node-agent.pem")

	run := func(args ...string) (string, error) {
		out, err := exec.Command(helper, args...).CombinedOutput() //nolint:gosec // the helper of the image, on arguments built here
		return string(out), err
	}
	out, err := run("read-certificate-data", "--certificate", pairFile)
	require.NoError(t, err, out)
	var data struct {
		CertificateData string `json:"certificateData"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &data), out)
	der, err := base64.StdEncoding.DecodeString(data.CertificateData)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	assert.Equal(t, []string{"gaon:nodes"}, cert.Subject.Organization, "the helper read the certificate of the pair")
	out, err = run("sign-string", "--certificate", pairFile, "--private-key", pairFile)
	require.NoError(t, err, out)
	assert.Regexp(t, `^"[0-9a-f]{16,}"\s*$`, out, "the helper signed with the key of the same file")

	_, err = run("sign-string", "--certificate", pairFile, "--private-key", filepath.Join(dir, "missing.pem"))
	var exit *exec.ExitError
	assert.True(t, errors.As(err, &exit), "control: with no key the helper refuses")
}
