package command_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		"gen", "kubernetes-node", "--ca", dep.ca, "--name", "node-" + index, "--role", role, "--address", "192.168.0." + index + "0/24",
		"--endpoint", "192.168.0.10", "--pod-network", "10.64.0.0/12", "--pod-prefix", "10.64." + index + ".0/24",
		"--peer", peer, "--out", out,
	}, extra...)...)
	return out
}

func (dep deployment) kubernetes(t *testing.T) {
	t.Helper()
	mustRun(t, "gen", "kubernetes", "--ca", dep.ca, "--endpoint", "192.168.0.10")
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

	out, err := run(t, "gen", "kubernetes-node", "--ca", dep.ca, "--name", "node-1", "--role", "control-plane", "--address", "192.168.0.10/24",
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
