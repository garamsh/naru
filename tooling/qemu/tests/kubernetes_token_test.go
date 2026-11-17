//go:build qemu_real

package tests_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// readyNodes are the names `kubectl get nodes` lists Ready.
func readyNodes(t *testing.T, c *node.KubeCluster) []string {
	t.Helper()
	out, err := kubectl(t, c, "", "get", "nodes", "--no-headers")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[1] == "Ready" {
			names = append(names, f[0])
		}
	}
	return names
}

// TestNode_AKubernetesWorkerJoinsWithABootstrapTokenAddedLiveAndOneWithARevokedTokenDoesNot starts a
// control-plane Node that accepts bootstrap tokens with an empty token file, and a worker with an
// issued pair. It adds node-3's token with `naructl token create --kubernetes` and revokes node-4's
// before either boots (ADR 0081). node-3 becomes Ready, node-4 never registers, and the
// control-plane Node's boot ID is the same at the end.
func TestNode_AKubernetesWorkerJoinsWithABootstrapTokenAddedLiveAndOneWithARevokedTokenDoesNot(t *testing.T) {
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	spec := node.KubeSpec{Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs, Naructl: tool, Late: []int{3, 4}}
	c, err := node.StartKubernetes(spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		for _, nd := range c.Nodes {
			t.Logf("console of %s:\n%s", nd.Name, nd.Console())
		}
	})
	ctx := context.Background()
	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=kubelet"), nd.Name)
	}
	require.Eventually(t, func() bool { return len(readyNodes(t, c)) == 2 }, kubeTimeout, 3*time.Second, "node-1 and node-2 are Ready")
	eventually(t, "node-1 Node API", func() bool { return ctl(c.APIAdmin, "version").Err == nil })
	_, bootID := versionOf(t, c.APIAdmin)

	rbac, err := tool.KubeBootstrapRBAC()
	require.NoError(t, err)
	out, err := kubectl(t, c, rbac, "apply", "-f", "-")
	require.NoError(t, err, out)

	require.NoError(t, tool.KubeTokenCreate(c.CADir, "node-3", c.APIAdmin))
	require.NoError(t, tool.KubeTokenCreate(c.CADir, "node-4", c.APIAdmin))
	// gen kubernetes-node --token reads the token the store holds, so the revoked one is put back
	// for node-4 to present the very token that was revoked.
	revoked, err := os.ReadFile(filepath.Join(c.CADir, "tokens", "node-4"))
	require.NoError(t, err)
	require.NoError(t, tool.KubeTokenRevoke(c.CADir, "node-4", c.APIAdmin))
	require.NoError(t, os.WriteFile(filepath.Join(c.CADir, "tokens", "node-4"), revoked, 0o600))
	assert.NotContains(t, readyNodes(t, c), "node-3", "setup: node-3 is not in the cluster before it boots")

	_, err = c.StartLate(3)
	require.NoError(t, err)
	late4, err := c.StartLate(4)
	require.NoError(t, err)

	require.Eventually(t, func() bool { return slices.Contains(readyNodes(t, c), "node-3") }, kubeTimeout, 3*time.Second, "node-3 joined with the token added live")
	require.NoError(t, late4.WaitFor(ctx, bootTimeout, "started", "service=kubelet"))
	time.Sleep(refusedWindow)
	nodes, err := kubectl(t, c, "", "get", "nodes", "--no-headers", "-o", "custom-columns=NAME:.metadata.name")
	require.NoError(t, err, nodes)
	assert.NotContains(t, nodes, "node-4", "node-4's token was revoked, so it is refused")
	_, after := versionOf(t, c.APIAdmin)
	assert.Equal(t, bootID, after, "the control-plane Node did not reboot")
}
