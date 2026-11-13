package naructl_test

import (
	"os"
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
	}))

	line := argsOf(t, out)
	assert.True(t, strings.HasPrefix(line, "gen kubernetes-node --ca /ca --name node-2 --role worker --address 10.78.0.2/24"), line)
	for _, want := range []string{
		"--peer 10.78.0.1=10.64.1.0/24", "--local-registry", "--registry-ca /ca.pem", "--sandbox-image 127.0.0.1:5000/x@sha256:ab",
		"--network-config /nc", "--mac 02:00:00:00:00:02", "--binary containerd=/c --binary kubelet=/k", "--disable kube-proxy",
		"--pod-network 10.64.0.0/12 --pod-prefix 10.64.2.0/24",
	} {
		assert.Contains(t, line, want)
	}
	assert.NotContains(t, line, "--extra-nic")
}

func TestGenKubernetes_NamesTheAdminAddressOnlyWhenThereIsOne(t *testing.T) {
	tool, out := recorder(t)

	require.NoError(t, tool.GenKubernetes("/ca", "10.78.0.1", "127.0.0.1:41000"))
	assert.Equal(t, "gen kubernetes --ca /ca --endpoint 10.78.0.1 --admin-address 127.0.0.1:41000", argsOf(t, out))

	require.NoError(t, tool.GenKubernetes("/ca", "10.78.0.1", ""))
	assert.Equal(t, "gen kubernetes --ca /ca --endpoint 10.78.0.1", argsOf(t, out))
}
