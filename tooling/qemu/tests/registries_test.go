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

// withService replaces the binary and the arguments of the service named name.
func withService(name, binary string, args ...string) userDataEdit {
	return func(doc *yaml.Node) error {
		for _, s := range field(field(doc.Content[0], "spec"), "services").Content {
			if field(s, "name").Value != name {
				continue
			}
			field(s, "binary").Value = binary
			list := field(s, "args")
			list.Content = nil
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

// pullMachine boots one host Node with spec and has its host API create a microVM whose image is on
// the Node's registry service. It returns the cluster, whose Node API the test may read, and the Node.
func pullMachine(t *testing.T, spec node.ClusterSpec) (*node.Cluster, *node.Node) {
	t.Helper()
	spec.N, spec.GenNode, spec.NodeAPI, spec.Host = 1, true, true, true
	c := start(t, spec)
	nd := c.Nodes[0]
	ctx := context.Background()
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=registry"))
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=naru-hostd"))
	eventually(t, "the host API answers a reader", func() bool { return hctl(c.Host.Reader, []string{"host"}).Err == nil })
	made := hctl(c.Host.Provisioner, []string{"microvm", "create"}, "--name", "hello", "--image", inputs.Image.Repository+"@"+inputs.Image.Digest, "--disk", "data=1048576")
	require.NoError(t, made.Err, made.refusal())
	return c, nd
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
	_, nd := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry("username="+regUser, passwordFile(t, regPassword)),
		EditUserData: editUserData(authArgs),
	})

	served(t, nd)
}

// TestNode_AMachineImageIsRefusedWhenSpecRegistriesGivesNoCredential is the control of the test
// above: the same registry, the same authority, and no credential. The registry logs that it
// refused the request, and never serves the image.
func TestNode_AMachineImageIsRefusedWhenSpecRegistriesGivesNoCredential(t *testing.T) {
	_, nd := pullMachine(t, node.ClusterSpec{
		Registries:   registryEntry(),
		EditUserData: editUserData(authArgs),
	})

	notServed(t, nd, "refused", "credential=false")
}

// TestNode_AMachineImageIsRefusedWithAWrongPassword is a second control: a credential that the
// registry does not accept.
func TestNode_AMachineImageIsRefusedWithAWrongPassword(t *testing.T) {
	_, nd := pullMachine(t, node.ClusterSpec{
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
	_, nd := pullMachine(t, node.ClusterSpec{
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
	_, nd := pullMachine(t, node.ClusterSpec{
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
	_, nd := pullMachine(t, node.ClusterSpec{
		Registries: []string{},
		EditUserData: editUserData(
			withFile("/run/regtest/cert.pem", "0644", string(crt)),
			withFile("/run/regtest/key.pem", "0600", string(key)),
			withArgs("registry", "-cert", "/run/regtest/cert.pem", "-key", "/run/regtest/key.pem"),
		),
	})

	served(t, nd)
}

// hostPair writes a certificate and its key into dir as naructl writes naru-hostd's serving pair,
// tls.crt and tls.key, and returns their paths.
func hostPair(t *testing.T, dir string) (crt, key string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	subject := pkix.Name{CommonName: "node-1"}
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: subject,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: subject}, &k.PublicKey, k)
	require.NoError(t, err)
	sec1, err := x509.MarshalECPrivateKey(k)
	require.NoError(t, err)
	crt, key = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	require.NoError(t, os.WriteFile(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644))
	require.NoError(t, os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1}), 0o600))
	return crt, key
}

// TestNode_AwsSigningHelperAcceptsNaruHostdsPairAsCertificateAndKey checks, with the helper the image
// carries, that naru-hostd's tls.crt and tls.key serve as --certificate and --private-key, which is
// how aws-credentials uses them (ADR 0029). It stops before any network call: read-certificate-data
// and sign-string only read the files.
func TestNode_AwsSigningHelperAcceptsNaruHostdsPairAsCertificateAndKey(t *testing.T) {
	helper := filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "regtools", "aws_signing_helper")
	require.FileExists(t, helper, "make image builds the helper from the pin")
	dir := t.TempDir()
	crt, key := hostPair(t, dir)

	run := func(args ...string) (string, error) {
		out, err := exec.Command(helper, args...).CombinedOutput()
		return string(out), err
	}
	out, err := run("read-certificate-data", "--certificate", crt)
	require.NoError(t, err, out)
	var data struct {
		CertificateData string `json:"certificateData"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &data), out)
	der, err := base64.StdEncoding.DecodeString(data.CertificateData)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	assert.Equal(t, "node-1", cert.Subject.CommonName, "the helper read the certificate of the pair")
	out, err = run("sign-string", "--certificate", crt, "--private-key", key)
	require.NoError(t, err, out)
	assert.Regexp(t, `^"[0-9a-f]{16,}"\s*$`, out, "the helper signed with the key of the pair")

	_, err = run("sign-string", "--certificate", crt, "--private-key", filepath.Join(dir, "missing.pem"))
	var exit *exec.ExitError
	assert.True(t, errors.As(err, &exit), "control: with no key the helper refuses")
}

// The tests below boot a Node whose spec.aws names ARNs that do not exist, so that the
// aws-credentials service (ADR 0029) runs, and fails the way a Node with no route to AWS does.
const (
	fakeRegion = "ap-northeast-2"
	fakeTA     = "arn:aws:rolesanywhere:ap-northeast-2:111111111111:trust-anchor/00000000-0000-0000-0000-000000000000"
	fakeProf   = "arn:aws:rolesanywhere:ap-northeast-2:111111111111:profile/00000000-0000-0000-0000-000000000001"
	fakeRole   = "arn:aws:iam::111111111111:role/naru84-fake"
)

func fakeAWS(spec node.ClusterSpec) node.ClusterSpec {
	spec.AWSRegion, spec.AWSTrustAnchor, spec.AWSProfile, spec.AWSRole = fakeRegion, fakeTA, fakeProf, fakeRole
	return spec
}

// kept is what `naructl logs name` prints for the Node API's reader.
func kept(c *node.Cluster, name string) string { return ctl(c.API.Reader, "logs", name).Stdout }

// TestNode_AnEcrLoginPullLogsWhereTheOperatorReadsItAndAWSCredentialsLogsItsOwnFailure gives the
// Node spec.aws with fake ARNs and the ecr-login helper for the registry service's host, and has
// mvmd pull from it. The host is no ECR registry's, and the helper logs that to its own file under
// mvmd's HOME, which init keeps as lines of mvmd. aws-credentials runs the helper's serve, and the
// line the operator reads is that program's own failure, not the exec error of a shell the image
// lacks.
func TestNode_AnEcrLoginPullLogsWhereTheOperatorReadsItAndAWSCredentialsLogsItsOwnFailure(t *testing.T) {
	c, nd := pullMachine(t, fakeAWS(node.ClusterSpec{
		Registries:   registryEntry("helper=ecr-login"),
		EditUserData: editUserData(authArgs),
	}))

	eventually(t, "naructl logs mvmd shows the line ecr-login wrote to its log file", func() bool {
		return strings.Contains(kept(c, "mvmd"), "Error parsing the serverURL")
	})
	eventually(t, "naructl logs aws-credentials shows the helper's own failure", func() bool {
		return strings.Contains(kept(c, "aws-credentials"), "failed to generate initial credentials")
	})
	out := kept(c, "aws-credentials")
	for _, execError := range []string{"executable file not found", "exec:", "sh:", "no such file or directory"} {
		assert.NotContains(t, out, execError)
	}
	notServed(t, nd)
}

// TestNode_AWSCredentialsRestartsWhenNaruHostdsCertificateChanges boots a host whose spec.aws makes
// aws-credentials read naru-hostd's certificate, and rewrites that file through the Node API.
// aws-credentials runs the image's stand-in, which stays up, so that what restarts it is the file it
// reads changing and not an exit: init logs that the watched file changed.
func TestNode_AWSCredentialsRestartsWhenNaruHostdsCertificateChanges(t *testing.T) {
	c := start(t, fakeAWS(node.ClusterSpec{
		N: 1, GenNode: true, NodeAPI: true, Host: true,
		EditUserData: editUserData(withService("aws-credentials", "/bin/naru-standin", "-timeout", "10m")),
	}))
	nd := c.Nodes[0]
	ctx := context.Background()
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=aws-credentials"))
	assert.NotContains(t, nd.Console(), "a watched file changed", "control: nothing changed the file yet")

	var doc struct {
		Spec struct {
			Files []struct{ Path, Content string }
		}
	}
	b, err := os.ReadFile(userDataOf(nd))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(b, &doc))
	var crt string
	for _, f := range doc.Spec.Files {
		if f.Path == hostCertFile {
			crt = f.Content
		}
	}
	require.NotEmpty(t, crt, "the user-data carries naru-hostd's certificate")
	patch, err := json.Marshal(map[string]any{"spec": map[string]any{"files": []any{map[string]any{"path": hostCertFile, "content": crt + "\n"}}}})
	require.NoError(t, err)
	applied := ctl(c.API.Admin, "apply", "-f", patchFile(t, string(patch)), "--mode", "no-reboot")
	require.NoError(t, applied.Err, applied.refusal())

	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "a watched file changed", "service=aws-credentials"))
}

// hostCertFile is naru-hostd's serving certificate, which aws-credentials watches.
const hostCertFile = "/run/naru-hostd/tls.crt"

// dev6Form turns the user-data into what a v0.1.0-dev.6 naructl wrote for a spec.aws Node: no
// aws-credentials service, no restartOnChange or logFiles, and mvmd's environment naming a
// credential_process profile that nothing writes any more.
func dev6Form(doc *yaml.Node) error {
	services := field(field(doc.Content[0], "spec"), "services")
	var keep []*yaml.Node
	for _, s := range services.Content {
		if field(s, "name").Value == "aws-credentials" {
			continue
		}
		for _, key := range []string{"restartOnChange", "logFiles"} {
			for i := 0; i+1 < len(s.Content); i += 2 {
				if s.Content[i].Value == key {
					s.Content = append(s.Content[:i], s.Content[i+2:]...)
					break
				}
			}
		}
		if field(s, "name").Value == "mvmd" {
			env := field(s, "env")
			env.Content = nil
			for _, kv := range [][2]string{{"PATH", "/bin:/run/bin"}, {"AWS_CONFIG_FILE", "/run/naru/aws/config"}, {"AWS_REGION", fakeRegion}} {
				env.Content = append(env.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[0]},
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: kv[1]})
			}
		}
		keep = append(keep, s)
	}
	services.Content = keep
	return nil
}

// TestNode_UserDataOfDev6FormWithSpecAWSBootsWithoutAWSCredentials checks that user-data written
// before aws-credentials existed still boots: init accepts it, starts its services, naru-hostd among
// them, and no aws-credentials runs.
func TestNode_UserDataOfDev6FormWithSpecAWSBootsWithoutAWSCredentials(t *testing.T) {
	c := start(t, fakeAWS(node.ClusterSpec{N: 1, GenNode: true, NodeAPI: true, Host: true, EditUserData: editUserData(dev6Form)}))
	nd := c.Nodes[0]

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "started", "service=naru-hostd"))
	assert.NotContains(t, pids(t, c.API.Reader), "aws-credentials")
	assert.NotContains(t, nd.Console(), "service=aws-credentials")
}
