//go:build qemu_real

package tests_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The NetworkPolicy test (ADR 0086) boots the two-Node cluster with kube-network-policies as the
// enforcer. It waits for kube-network-policies to start on every Node before applying policies:
// an enforcer that has not started yet does not hold traffic. It then verifies that a default-deny
// ingress policy blocks cross-Node Pod traffic and traffic between two Pods of one Node, that an allow rule for port 8080 opens exactly
// that port, and that a default-deny egress policy holds outbound connections. A second cluster
// started with --disable-network-policy carries no kube-network-policies service and leaves
// NetworkPolicy objects unenforced, so a default-deny does not block traffic.

// startNetworkPolicyCluster boots a cluster and waits for kube-network-policies to be running on
// every Node before returning.
func startNetworkPolicyCluster(t *testing.T) *node.KubeCluster {
	t.Helper()
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	spec := node.KubeSpec{
		Disk:    filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"),
		Dir:     t.TempDir(),
		Inputs:  inputs,
		Naructl: tool,
	}
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
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=kube-network-policies"), nd.Name+" kube-network-policies")
	}
	kubectlEventually(t, c, "the default ServiceAccount exists", "get", "serviceaccount", "default", "-o", "name")
	return c
}

func TestNode_KubernetesNetworkPolicyDefaultDenyBlocksCrossNodePodTrafficAndAllowRuleOpensPort(t *testing.T) {
	c := startNetworkPolicyCluster(t)

	// The serve Pod on node-1 serves HTTP on port 8080.
	out, err := kubectl(t, c, fmt.Sprintf(pod("serve", "node-1", "serve", "serve", ":8080"), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP := kubectlEventually(t, c, "the serve Pod has an address", "get", "pod", "serve", "-o", "jsonpath={.status.podIP}")
	byPod := "http://" + podIP + ":8080"

	// The dial Pod on node-2 starts before the default-deny lands, so that it is already running and
	// retrying when the policy takes effect. The dial command retries every two seconds with a
	// three-second HTTP timeout.
	out, err = kubectl(t, c, fmt.Sprintf(pod("dial", "node-2", "dial", "dial", byPod), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)

	// Ingress: default-deny all ingress to Pods in the default namespace.
	const defaultDenyIngress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`
	out, err = kubectl(t, c, defaultDenyIngress, "apply", "-f", "-")
	require.NoError(t, err, out)

	// The enforcer drops all ingress packets to the serve Pod: the dial Pod must not reach it.
	blocked := assert.Never(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod)
	}, 20*time.Second, 2*time.Second, "default-deny ingress NetworkPolicy blocks cross-Node traffic")
	if !blocked {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("dial Pod logs while blocked:\n%s", last)
		t.FailNow()
	}

	// Allow ingress on port 8080 from Pods in the default namespace.
	const allowIngress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: allow-ingress-8080
spec:
  podSelector:
    matchLabels:
      app: serve
  policyTypes:
  - Ingress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: default
    ports:
    - protocol: TCP
      port: 8080
`
	out, err = kubectl(t, c, allowIngress, "apply", "-f", "-")
	require.NoError(t, err, out)

	// The dial Pod is still running and retrying: the allow rule opens the port.
	opened := assert.Eventually(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod)
	}, kubeTimeout, 2*time.Second, "allow ingress NetworkPolicy for port 8080 opens the port")
	if !opened {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("dial Pod logs after allow rule:\n%s", last)
		t.FailNow()
	}
}

func TestNode_KubernetesNetworkPolicyDefaultDenyEgressBlocksOutboundTrafficFromPod(t *testing.T) {
	c := startNetworkPolicyCluster(t)

	// The serve Pod on node-1 serves HTTP on port 8080.
	out, err := kubectl(t, c, fmt.Sprintf(pod("serve", "node-1", "serve", "serve", ":8080"), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP := kubectlEventually(t, c, "the serve Pod has an address", "get", "pod", "serve", "-o", "jsonpath={.status.podIP}")
	byPod := "http://" + podIP + ":8080"

	// Egress: default-deny all egress from Pods in the default namespace, then start the dial Pod.
	// The dial Pod's outbound connections to node-1 are blocked by the egress rule.
	const defaultDenyEgress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-egress
spec:
  podSelector: {}
  policyTypes:
  - Egress
`
	out, err = kubectl(t, c, defaultDenyEgress, "apply", "-f", "-")
	require.NoError(t, err, out)

	out, err = kubectl(t, c, fmt.Sprintf(pod("dial", "node-2", "dial", "dial", byPod), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)

	blocked := assert.Never(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod)
	}, 20*time.Second, 2*time.Second, "default-deny egress NetworkPolicy blocks outbound traffic from the dial Pod")
	if !blocked {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("dial Pod logs while egress blocked:\n%s", last)
		t.FailNow()
	}
}

// The same-Node case of ADR 0086 item 5: two Pods on one Node cross the bridge, not the routed path,
// so the policy is enforced only when bridge-nf-call-iptables passes the bridged packets to the
// netfilter hooks. It fails when init does not set the sysctl.
func TestNode_KubernetesNetworkPolicyDefaultDenyBlocksTrafficBetweenPodsOfOneNode(t *testing.T) {
	c := startNetworkPolicyCluster(t)

	const defaultDenyIngress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`
	out, err := kubectl(t, c, defaultDenyIngress, "apply", "-f", "-")
	require.NoError(t, err, out)

	out, err = kubectl(t, c, fmt.Sprintf(pod("serve", "node-1", "serve", "serve", ":8080"), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP := kubectlEventually(t, c, "the serve Pod has an address", "get", "pod", "serve", "-o", "jsonpath={.status.podIP}")
	byPod := "http://" + podIP + ":8080"

	// The dial Pod is on the serve Pod's Node.
	out, err = kubectl(t, c, fmt.Sprintf(pod("dial", "node-1", "dial", "dial", byPod), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)

	blocked := assert.Never(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod)
	}, 20*time.Second, 2*time.Second, "default-deny ingress NetworkPolicy blocks traffic between two Pods of one Node")
	if !blocked {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("dial Pod logs while blocked:\n%s", last)
		t.FailNow()
	}
}

func TestNode_KubernetesDisabledNetworkPolicyLeavesServiceOutOfTheTopologyAndNetworkPoliciesAreUnenforced(t *testing.T) {
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	dir := t.TempDir()
	spec := node.KubeSpec{
		Disk:                 filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"),
		Dir:                  dir,
		Inputs:               inputs,
		Naructl:              tool,
		DisableNetworkPolicy: true,
	}
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
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=kubelet"), nd.Name+" kubelet")
	}

	// The user-data must not carry a kube-network-policies entry.
	b, err := os.ReadFile(filepath.Join(dir, "node-1", "naru.yaml"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "kube-network-policies", "node-1's user-data must not carry kube-network-policies when network policy is disabled")

	kubectlEventually(t, c, "the default ServiceAccount exists", "get", "serviceaccount", "default", "-o", "name")

	// Apply a default-deny ingress policy: without an enforcer, it must not block traffic.
	const defaultDenyIngress = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: default-deny-ingress
spec:
  podSelector: {}
  policyTypes:
  - Ingress
`
	out, err := kubectl(t, c, defaultDenyIngress, "apply", "-f", "-")
	require.NoError(t, err, out)

	out, err = kubectl(t, c, fmt.Sprintf(pod("serve", "node-1", "serve", "serve", ":8080"), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP := kubectlEventually(t, c, "the serve Pod has an address", "get", "pod", "serve", "-o", "jsonpath={.status.podIP}")
	byPod := "http://" + podIP + ":8080"

	out, err = kubectl(t, c, fmt.Sprintf(pod("dial", "node-2", "dial", "dial", byPod), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)

	reached := assert.Eventually(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod)
	}, kubeTimeout, 2*time.Second, "without an enforcer a default-deny NetworkPolicy does not block cross-Node traffic")
	if !reached {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("dial Pod logs:\n%s", last)
		t.FailNow()
	}
}
