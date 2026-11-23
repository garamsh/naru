package naructl_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
)

func argsOf(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	require.NoError(t, err)
	return strings.Join(strings.Split(strings.TrimSpace(string(b)), "\n"), " ")
}

func TestGenKubernetesNode_PassesEveryFieldAsItsFlag(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{
		CA: "/ca", Name: "node-2", Role: "worker", Address: "10.78.0.2/24", Endpoint: "10.78.0.1",
		PodNetwork: "10.64.0.0/12", PodPrefix: "10.64.2.0/24", Peers: []string{"10.78.0.1=10.64.1.0/24"},
		MAC: "02:00:00:00:00:02", Gateway: "10.78.0.254", Nameservers: []string{"10.78.0.254"}, Search: "naru.test",
		LocalRegistry: true, RegistryCA: "/ca.pem", SandboxImage: "127.0.0.1:5000/x@sha256:ab",
		Binaries: map[string]string{"kubelet": "/k", "containerd": "/c"}, Disable: []string{"kube-proxy"},
		Out: "/o.yaml", NetworkConfig: "/nc",
		Storage: []string{"data1=serial:S1"}, BlockSerials: []string{"S2", "S3"},
	}))

	line := argsOf(t, out)
	assert.True(t, strings.HasPrefix(line, "gen kubernetes-node --ca-dir /ca --name node-2 --role worker --address 10.78.0.2/24"), line)
	for _, want := range []string{
		"--peer 10.78.0.1=10.64.1.0/24", "--local-registry", "--registry-ca /ca.pem", "--sandbox-image 127.0.0.1:5000/x@sha256:ab",
		"--network-config /nc", "--mac 02:00:00:00:00:02", "--binary containerd=/c --binary kubelet=/k", "--disable kube-proxy",
		"--pod-network 10.64.0.0/12 --pod-prefix 10.64.2.0/24",
		"--storage data1=serial:S1", "--block-serial S2 --block-serial S3",
	} {
		assert.Contains(t, line, want)
	}
	assert.NotContains(t, line, "--extra-nic")
}

func TestGenKubernetes_NamesTheAdminAddressOnlyWhenThereIsOne(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenKubernetes("/ca", "10.78.0.1", "127.0.0.1:41000"))
	assert.Equal(t, "gen kubernetes --ca-dir /ca --endpoint 10.78.0.1 --admin-address 127.0.0.1:41000", argsOf(t, out))

	require.NoError(t, tool.GenKubernetes("/ca", "10.78.0.1", ""))
	assert.Equal(t, "gen kubernetes --ca-dir /ca --endpoint 10.78.0.1", argsOf(t, out))
}

func TestGenKubernetesCluster_PassesTheServiceAccountSettingsAsTheirFlagsAndNothingElseWhenUnset(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenKubernetesCluster(naructl.KubernetesCluster{
		CADir: "/ca", Endpoint: "10.78.0.1", ServiceAccountIssuers: []string{"https://a.example.com", "https://b.example.com"},
		ServiceAccountAlgorithm: "ES256", RotateServiceAccountKey: "kid1",
	}))

	assert.Equal(t, "gen kubernetes --ca-dir /ca --endpoint 10.78.0.1 --service-account-issuer https://a.example.com --service-account-issuer https://b.example.com --service-account-algorithm ES256 --rotate-service-account-key kid1", argsOf(t, out))
}

func TestSigningKeyID_IsWhatGenKubernetesJWKSPrints(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "naructl")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\necho 'an other key id is decoy'\necho 'naructl: the signing ServiceAccount key id is abc-DEF_1 (--rotate-service-account-key takes it)'\n"), 0o755))

	kid, err := naructl.Tool{Bin: bin}.SigningKeyID("/ca")

	require.NoError(t, err)
	assert.Equal(t, "abc-DEF_1", kid)
	_, err = naructl.Tool{Bin: "/bin/true"}.SigningKeyID("/ca")
	assert.ErrorContains(t, err, "no signing key id")
}

func TestGenKubernetesNode_APodPrefixFromTheSeedIsItsOwnFlagAndNamesNoPrefix(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{CA: "/ca", Name: "w1-1", PodNetwork: "10.64.0.0/16", PodPrefixFromSeed: true, Out: "/o.yaml"}))

	line := argsOf(t, out)
	assert.Contains(t, line, "--pod-network 10.64.0.0/16 --pod-prefix-from-seed --out /o.yaml")
	assert.NotContains(t, line, "--pod-prefix ", "the control: the flag stands in for the prefix")
}
