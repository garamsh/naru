package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/layout/pki"
)

// kubeNode runs gen kubernetes-node for a Node of a two-Node cluster and returns the user-data file.
func (dep deployment) kubeNode(t *testing.T, role string, extra ...string) string {
	t.Helper()
	index, peer := "1", "192.168.0.20=10.64.2.0/24"
	if role == "worker" {
		index, peer = "2", "192.168.0.10=10.64.1.0/24"
	}
	out := filepath.Join(t.TempDir(), "node-"+index+".yaml")
	mustRun(t, append([]string{
		"gen", "kubernetes-node", "--ca-dir", dep.ca, "--name", "node-" + index, "--role", role, "--address", "192.168.0." + index + "0/24",
		"--endpoint", "192.168.0.10", "--pod-network", "10.64.0.0/12", "--pod-prefix", "10.64." + index + ".0/24",
		"--peer", peer, "--out", out,
	}, extra...)...)
	return out
}

func (dep deployment) kubernetes(t *testing.T) {
	t.Helper()
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10")
}

func TestGenKubernetesNode_BothRolesAreAcceptedByInit(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)

	for _, role := range []string{"control-plane", "worker"} {
		validate(t, dep.kubeNode(t, role, "--local-registry", "--sandbox-image", "127.0.0.1:5000/pause", "--registry-ca", "/var/lib/naru/inputs/registry/ca.pem"))
	}
}

func TestGenKubernetesNode_WorkerRoutesToTheControlPlanesPods(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)

	got := routes(t, dep.kubeNode(t, "worker"))

	assert.Equal(t, []struct{ To, Via string }{{"10.64.1.0/24", "192.168.0.10"}}, got)
}

func TestGenKubernetesNode_ControlPlaneNeedsWhatGenKubernetesWrites(t *testing.T) {
	dep := newDeployment(t)

	out, err := run(t, "gen", "kubernetes-node", "--ca-dir", dep.ca, "--name", "node-1", "--role", "control-plane", "--address", "192.168.0.10/24",
		"--endpoint", "192.168.0.10", "--pod-network", "10.64.0.0/12", "--pod-prefix", "10.64.1.0/24", "--out", filepath.Join(t.TempDir(), "x.yaml"))

	require.Error(t, err)
	assert.Contains(t, out, "run gen kubernetes")
}

func TestGenKubernetes_WritesTheAdminKubeconfigAndKeepsTheKeysOnASecondRun(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)
	before, err := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	require.NoError(t, err)

	dep.kubernetes(t)

	after, err := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	require.NoError(t, err)
	assert.Equal(t, before, after, "the ServiceAccount key is not replaced")
	for file, mode := range map[string]os.FileMode{"admin.conf": 0o600, "sa.key": 0o600, "sa.pub": 0o644, "front-proxy/ca.key": 0o600} {
		info, err := os.Stat(filepath.Join(dep.ca, file))
		require.NoError(t, err, file)
		assert.Equal(t, mode, info.Mode().Perm(), file)
	}
}

func TestGenKubernetes_DisableAddonIsWhatTheControlPlaneNodeReadsAndAnUnknownNameIsRefused(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--disable-addon", "dns", "--disable-addon", "volumes", "--disable-network-policy")

	data, err := os.ReadFile(dep.kubeNode(t, "control-plane"))
	require.NoError(t, err)

	assert.NotContains(t, string(data), "name: addons\n", "a cluster without DNS, volumes and the enforcer has nothing for the add-ons service to apply")
	_, err = run(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--disable-addon", "nope")
	require.Error(t, err)
	assert.EqualError(t, err, `--disable-addon "nope" is not one of dns, volumes`)
}

func TestGenKubernetes_DisableNetworkPolicyIsWhatEveryNodeReadsAndAGenWithoutItTurnsTheEnforcerOnAgain(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--disable-network-policy")
	_, err := os.Stat(filepath.Join(dep.ca, pki.DisableNetworkPolicyFile))
	require.NoError(t, err, "the marker is in the CA directory beside disabled-addons")

	for _, role := range []string{"control-plane", "worker"} {
		data, err := os.ReadFile(dep.kubeNode(t, role))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "kube-network-policies", role)
		assert.NotContains(t, string(data), "bridgeNetfilter", role)
	}

	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10")
	_, err = os.Stat(filepath.Join(dep.ca, pki.DisableNetworkPolicyFile))
	assert.ErrorIs(t, err, os.ErrNotExist, "a run without the flag removes the marker")
	for _, role := range []string{"control-plane", "worker"} {
		data, err := os.ReadFile(dep.kubeNode(t, role))
		require.NoError(t, err)
		assert.Contains(t, string(data), "name: kube-network-policies\n", role)
		assert.Contains(t, string(data), "bridgeNetfilter: true", role)
	}
}

func TestGenKubernetes_AGenWithoutDisableAddonTurnsEveryAddonOnAgain(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--disable-addon", "dns", "--disable-addon", "volumes")
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10")

	data, err := os.ReadFile(dep.kubeNode(t, "control-plane"))
	require.NoError(t, err)

	assert.Contains(t, string(data), "name: addons\n")
}

func TestGenKubernetes_EnableAddonIsWhatEveryNodeReadsAndOnlyAnOptInNameIsAccepted(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)
	for _, role := range []string{"control-plane", "worker"} {
		data, err := os.ReadFile(dep.kubeNode(t, role))
		require.NoError(t, err)
		assert.NotContains(t, string(data), "--cloud-provider=external", "%s: off by default", role)
	}

	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--enable-addon", "cloud-controller")
	for _, role := range []string{"control-plane", "worker"} {
		data, err := os.ReadFile(dep.kubeNode(t, role))
		require.NoError(t, err)
		assert.Contains(t, string(data), "--cloud-provider=external", "%s: every kubelet", role)
	}
	cp, err := os.ReadFile(dep.kubeNode(t, "control-plane"))
	require.NoError(t, err)
	assert.Contains(t, string(cp), "cloud-controller.yaml")

	_, err = run(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--enable-addon", "dns")
	assert.EqualError(t, err, `--enable-addon "dns" is not one of cloud-controller`)

	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10")
	data, err := os.ReadFile(dep.kubeNode(t, "worker"))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "--cloud-provider=external", "a gen without the flag turns it off again")
}

func TestGenKubernetes_AControlPlaneNodeAppliesTheAddonsByDefault(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)

	data, err := os.ReadFile(dep.kubeNode(t, "control-plane"))
	require.NoError(t, err)

	assert.Contains(t, string(data), "name: addons\n")
}

func TestTopology_KubernetesIsTheOneBesidesGaons(t *testing.T) {
	out, err := run(t, "topology", "kubernetes")
	require.NoError(t, err)

	var got struct {
		Services []struct {
			Name string
		}
		Agent string
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	var names []string
	for _, s := range got.Services {
		names = append(names, s.Name)
	}
	assert.Contains(t, names, "kubelet")
	assert.Equal(t, "kubelet", got.Agent)
	_, err = run(t, "topology", "other")
	assert.Error(t, err)
}

func TestGenKubernetesNode_ANodeAPIAuthorityGivesTheNodeSpecAPISoATokenCanBePatchedLive(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)
	plain := dep.kubeNode(t, "control-plane", "--bootstrap-tokens")
	assert.Nil(t, apiOf(t, plain), "control: without the authority there is no API")
	apiAuthority(t, dep)

	out := dep.kubeNode(t, "control-plane", "--bootstrap-tokens")
	validate(t, out)
	got := apiOf(t, out)

	require.NotNil(t, got)
	assert.NotEmpty(t, got.Certificate)
}

func TestGenKubernetesBootstrapRBAC_PrintsTheBindingsASingleApplyNeeds(t *testing.T) {
	out, err := run(t, "gen", "kubernetes-bootstrap-rbac")

	require.NoError(t, err)
	for _, want := range []string{"system:node-bootstrapper", "system:bootstrappers"} {
		assert.Contains(t, out, want)
	}
}

func TestGenKubernetes_DisablingVolumesLeavesTheVolumeExtensionOutOfTheNodes(t *testing.T) {
	dep := newDeployment(t)
	mustRun(t, "gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10", "--disable-addon", "volumes")

	data, err := os.ReadFile(dep.kubeNode(t, "worker", "--block-serial", "b1"))
	require.NoError(t, err)

	assert.NotContains(t, string(data), "naru-volumes")
	cp, err := os.ReadFile(dep.kubeNode(t, "control-plane"))
	require.NoError(t, err)
	assert.Contains(t, string(cp), "/addons/manifests/dns.yaml", "dns is still applied")
	assert.NotContains(t, string(cp), "volumes.yaml")
}

func TestGenKubernetesNode_StorageAndBlockSerialsMakeTheNodeOfferVolumes(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)

	out := dep.kubeNode(t, "worker", "--storage", "data=serial:s1", "--block-serial", "b1", "--block-serial", "b2")
	validate(t, out)
	data, err := os.ReadFile(out)
	require.NoError(t, err)

	got := string(data)
	assert.Contains(t, got, "name: naru-volumes\n", "the volume extension is in the Node's configuration")
	assert.Contains(t, got, "--node-labels=naru.garam.sh/volumes=true")
	assert.Contains(t, got, "storageDir")
	assert.Contains(t, got, "serial: s1")
	_, err = run(t, "gen", "kubernetes-node", "--ca-dir", dep.ca, "--name", "node-2", "--role", "worker", "--address", "192.168.0.20/24",
		"--endpoint", "192.168.0.10", "--pod-network", "10.64.0.0/12", "--pod-prefix", "10.64.2.0/24", "--peer", "192.168.0.10=10.64.1.0/24",
		"--storage", "data=serial:s1", "--block-serial", "s1", "--out", filepath.Join(t.TempDir(), "x.yaml"))
	require.Error(t, err, "a disk is either a storage disk or a block disk")
}

func saAlgorithm(t *testing.T, ca string) pki.Algorithm {
	t.Helper()
	pub, err := os.ReadFile(filepath.Join(ca, "sa.pub"))
	require.NoError(t, err)
	alg, err := pki.KeyAlgorithm(pub)
	require.NoError(t, err)
	return alg
}

func genKubernetesWith(t *testing.T, dep deployment, extra ...string) (string, error) {
	t.Helper()
	return run(t, append([]string{"gen", "kubernetes", "--ca-dir", dep.ca, "--endpoint", "192.168.0.10"}, extra...)...)
}

func TestGenKubernetes_TheServiceAccountKeyIsRS256UnlessAnAlgorithmIsNamed(t *testing.T) {
	plain, named := newDeployment(t), newDeployment(t)
	plain.kubernetes(t)
	_, err := genKubernetesWith(t, named, "--service-account-algorithm", "ES256")
	require.NoError(t, err)

	assert.Equal(t, pki.RS256, saAlgorithm(t, plain.ca))
	assert.Equal(t, pki.ES256, saAlgorithm(t, named.ca))
	_, err = genKubernetesWith(t, newDeployment(t), "--service-account-algorithm", "HS256")
	assert.ErrorContains(t, err, "RS256 or ES256")
}

func TestGenKubernetes_AKeyThereIsKeptAndANamedAlgorithmThatDiffersIsRefusedUnlessTheKeyIsRotated(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-algorithm", "ES256")
	require.NoError(t, err)
	before, err := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	require.NoError(t, err)

	_, err = genKubernetesWith(t, dep)
	require.NoError(t, err, "control: no algorithm named keeps the ES256 key")
	_, err = genKubernetesWith(t, dep, "--service-account-algorithm", "ES256")
	require.NoError(t, err, "control: the same algorithm named keeps it")
	kept, _ := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	assert.Equal(t, before, kept)
	_, err = genKubernetesWith(t, dep, "--service-account-algorithm", "RS256")

	assert.ErrorContains(t, err, "--rotate-service-account-key")
	still, _ := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	assert.Equal(t, before, still, "the refusal changed nothing")
}

// signingKid is the key id of the key that signs, from sa.key.
func signingKid(t *testing.T, dep deployment) string {
	t.Helper()
	pub, err := pki.SigningPublicKey(readFile(t, filepath.Join(dep.ca, "sa.key")))
	require.NoError(t, err)
	kid, err := pki.SigningKeyID(pub)
	require.NoError(t, err)
	return kid
}

func TestGenKubernetes_RotatingMakesANewSigningKeyAndKeepsOnlyThePublicKeyOfTheOneBefore(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-algorithm", "ES256")
	require.NoError(t, err)
	oldKey, _ := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	oldPub, _ := os.ReadFile(filepath.Join(dep.ca, "sa.pub"))

	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", signingKid(t, dep), "--service-account-algorithm", "RS256")
	require.NoError(t, err)

	newKey, _ := os.ReadFile(filepath.Join(dep.ca, "sa.key"))
	newPub, _ := os.ReadFile(filepath.Join(dep.ca, "sa.pub"))
	assert.NotEqual(t, oldKey, newKey)
	assert.Equal(t, pki.RS256, saAlgorithm(t, dep.ca), "the new key signs")
	keys, err := pki.PublicKeys(newPub)
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.Contains(t, string(newPub), string(oldPub), "the old public key is still accepted")
	info, err := os.Stat(filepath.Join(dep.ca, "sa.key"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", signingKid(t, dep))
	require.NoError(t, err)
	keys, err = pki.PublicKeys(readFile(t, filepath.Join(dep.ca, "sa.pub")))
	require.NoError(t, err)
	assert.Len(t, keys, 2, "a second rotation drops the first key")
	assert.NotContains(t, string(readFile(t, filepath.Join(dep.ca, "sa.pub"))), string(oldPub))
	assert.Equal(t, pki.RS256, saAlgorithm(t, dep.ca), "a rotation that names no algorithm keeps the cluster's")
}

func TestGenKubernetes_ARotationRepeatedWithTheSameKeyIdIsRefusedAndChangesNothing(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)
	kid := signingKid(t, dep)
	_, err := genKubernetesWith(t, dep, "--rotate-service-account-key", kid)
	require.NoError(t, err, "control: the first run rotates")
	afterOne := readFile(t, filepath.Join(dep.ca, "sa.pub"))
	keyAfterOne := readFile(t, filepath.Join(dep.ca, "sa.key"))

	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", kid)

	assert.ErrorContains(t, err, "rotated already")
	assert.Equal(t, afterOne, readFile(t, filepath.Join(dep.ca, "sa.pub")))
	assert.Equal(t, keyAfterOne, readFile(t, filepath.Join(dep.ca, "sa.key")))
}

func TestGenKubernetes_ARotationNeedsAKeyIdAndAKeyToRotate(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--rotate-service-account-key", "x")
	assert.ErrorContains(t, err, "no ServiceAccount key to rotate", "a new cluster has none")
	dep.kubernetes(t)

	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", "")
	assert.ErrorContains(t, err, "names the key id")
	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", "x", "--drop-previous-service-account-key")
	assert.ErrorContains(t, err, "exclusive")
}

func TestGenKubernetes_DroppingThePreviousKeyKeepsTheSigningKeyOnly(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)
	_, err := genKubernetesWith(t, dep, "--rotate-service-account-key", signingKid(t, dep))
	require.NoError(t, err)
	key := readFile(t, filepath.Join(dep.ca, "sa.key"))
	keys, _ := pki.PublicKeys(readFile(t, filepath.Join(dep.ca, "sa.pub")))
	require.Len(t, keys, 2, "setup")

	_, err = genKubernetesWith(t, dep, "--drop-previous-service-account-key")
	require.NoError(t, err)

	keys, err = pki.PublicKeys(readFile(t, filepath.Join(dep.ca, "sa.pub")))
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, key, readFile(t, filepath.Join(dep.ca, "sa.key")), "no new key is made")
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return b
}

func TestGenKubernetes_IssuersReachTheAPIServerInOrderWithTheInClusterIssuerLast(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com/naru", "--service-account-issuer", "https://old.example.com")
	require.NoError(t, err)

	cp := string(readFile(t, dep.kubeNode(t, "control-plane")))

	first := strings.Index(cp, "--service-account-issuer=https://oidc.example.com/naru")
	second := strings.Index(cp, "--service-account-issuer=https://old.example.com")
	inCluster := strings.Index(cp, "--service-account-issuer=https://kubernetes.default.svc.cluster.local")
	assert.True(t, 0 < first && first < second && second < inCluster, "first signs, then the rest, then the in-cluster issuer: %d %d %d", first, second, inCluster)
	assert.Contains(t, cp, "--service-account-jwks-uri=https://oidc.example.com/naru/openid/v1/jwks")
}

func TestGenKubernetes_AGenWithoutIssuerFlagsKeepsTheIssuersAndOnlyAnExplicitClearRemovesThem(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com")
	require.NoError(t, err)
	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", signingKid(t, dep))
	require.NoError(t, err)

	cp := string(readFile(t, dep.kubeNode(t, "control-plane")))
	assert.Contains(t, cp, "--service-account-issuer=https://oidc.example.com", "a rotation without the flag keeps the issuer")

	_, err = genKubernetesWith(t, dep, "--clear-service-account-issuers")
	require.NoError(t, err)
	cp = string(readFile(t, dep.kubeNode(t, "control-plane")))
	assert.NotContains(t, cp, "oidc.example.com")
	assert.Contains(t, cp, "--service-account-issuer=https://kubernetes.default.svc.cluster.local")

	_, err = genKubernetesWith(t, dep, "--clear-service-account-issuers", "--service-account-issuer", "https://oidc.example.com")
	assert.ErrorContains(t, err, "exclusive")
}

func TestGenKubernetes_RefusesAnIssuerThatIsNotAnHTTPSURL(t *testing.T) {
	for _, bad := range []string{"http://oidc.example.com", "oidc.example.com", "https://oidc.example.com/", "https://oidc.example.com/../x", "https://oidc.example.com/%2e%2e/x", "https://u:p@oidc.example.com", "HTTPS://oidc.example.com", "https://oidc.example.com/a b"} {
		_, err := genKubernetesWith(t, newDeployment(t), "--service-account-issuer", bad)
		assert.ErrorContains(t, err, "--service-account-issuer", bad)
	}
	_, err := genKubernetesWith(t, newDeployment(t), "--service-account-issuer", "https://oidc.example.com")
	assert.NoError(t, err, "control: an https URL")
}

func TestGenKubernetesJWKS_WritesTheDiscoveryDocumentAndKeySetUnderTheIssuersPath(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com/naru")
	require.NoError(t, err)
	out := t.TempDir()

	_, err = run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", out)
	require.NoError(t, err)

	var disc struct {
		Issuer  string   `json:"issuer"`
		JWKSURI string   `json:"jwks_uri"`
		Algs    []string `json:"id_token_signing_alg_values_supported"`
	}
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(out, "naru/.well-known/openid-configuration")), &disc))
	assert.Equal(t, "https://oidc.example.com/naru", disc.Issuer)
	assert.Equal(t, "https://oidc.example.com/naru/openid/v1/jwks", disc.JWKSURI)
	assert.Equal(t, []string{"RS256"}, disc.Algs)
	want, err := pki.ServiceAccountJWKS(readFile(t, filepath.Join(dep.ca, "sa.pub")))
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(readFile(t, filepath.Join(out, "naru/openid/v1/jwks"))))
}

func TestGenKubernetesJWKS_APublishedKeySetCarriesTheKeyBeforeARotationToo(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com", "--service-account-algorithm", "ES256")
	require.NoError(t, err)
	_, err = genKubernetesWith(t, dep, "--rotate-service-account-key", signingKid(t, dep), "--service-account-algorithm", "RS256")
	require.NoError(t, err)
	out := t.TempDir()

	_, err = run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", out)
	require.NoError(t, err)

	var set struct{ Keys []struct{ Alg string } }
	require.NoError(t, json.Unmarshal(readFile(t, filepath.Join(out, "openid/v1/jwks")), &set))
	require.Len(t, set.Keys, 2)
	assert.Equal(t, "RS256", set.Keys[0].Alg, "the signing key is first")
	assert.Equal(t, "ES256", set.Keys[1].Alg)
}

func TestGenKubernetesJWKS_RefusesAClusterWithNoIssuerToPublishFor(t *testing.T) {
	dep := newDeployment(t)
	dep.kubernetes(t)

	_, err := run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", t.TempDir())

	assert.ErrorContains(t, err, "no ServiceAccount issuer")
}

func TestGenKubernetesJWKS_PrintsTheSigningKeyIDThatARotationNames(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com")
	require.NoError(t, err)

	out, err := run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", t.TempDir())

	require.NoError(t, err)
	assert.Contains(t, out, "the signing ServiceAccount key id is "+signingKid(t, dep))
}

func TestGenKubernetesJWKS_RefusesAnIssuersFileThatNamesAPathOutsideOutDir(t *testing.T) {
	dep := newDeployment(t)
	_, err := genKubernetesWith(t, dep, "--service-account-issuer", "https://oidc.example.com/ok")
	require.NoError(t, err)
	out := t.TempDir()
	issuers := filepath.Join(dep.ca, "service-account-issuers")

	require.NoError(t, os.WriteFile(issuers, []byte("https://oidc.example.com/../../escape\n"), 0o644))
	_, err = run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", filepath.Join(out, "site"))
	assert.ErrorContains(t, err, "not a plain https URL")
	require.NoError(t, os.WriteFile(issuers, []byte("https://oidc.example.com/a b\n"), 0o644))
	_, err = run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", filepath.Join(out, "site"))
	assert.ErrorContains(t, err, "not a plain https URL", "a space does not split one issuer into two")
	_, statErr := os.Stat(filepath.Join(out, "escape"))
	assert.True(t, os.IsNotExist(statErr), "nothing was written outside")

	require.NoError(t, os.WriteFile(issuers, []byte("https://oidc.example.com/ok\n"), 0o644))
	_, err = run(t, "gen", "kubernetes-jwks", "--ca-dir", dep.ca, "--out-dir", filepath.Join(out, "site"))
	assert.NoError(t, err, "control: a plain issuer")
}
