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

// The DNS test (ADR 0083) boots the two-Node cluster of the Kubernetes test with its default add-ons.
// A Pod resolves a Service of its own namespace and one of another by name, and an external name through
// the Node's resolver, which CoreDNS forwards to. The Nodes' resolver is a Pod on node-1's network that
// answers the one name; no machine answers it otherwise. A second cluster disables DNS: its control-plane
// Node has no service to apply anything, and what the administrator put in kube-system stays as it was.

// startDNSCluster boots a cluster and waits for the default ServiceAccount, which a Pod needs.
func startDNSCluster(t *testing.T, spec node.KubeSpec) (*node.KubeCluster, string) {
	t.Helper()
	_, err := exec.LookPath("kubectl")
	require.NoError(t, err, "kubectl is a precondition of make test-qemu")
	spec.Disk, spec.Dir, spec.Inputs, spec.Naructl = filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), t.TempDir(), inputs, tool
	c, err := node.StartKubernetes(spec)
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		for _, nd := range c.Nodes {
			t.Logf("console of %s:\n%s", nd.Name, nd.Console())
		}
	})
	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "started", "service=kubelet"), nd.Name+" kubelet")
	}
	kubectlEventually(t, c, "the default ServiceAccount exists", "get", "serviceaccount", "default", "-o", "name")
	return c, spec.Dir
}

// nodeOneUserData is the user-data naructl wrote for node-1, the control-plane Node.
func nodeOneUserData(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "node-1", "naru.yaml"))
	require.NoError(t, err)
	return string(b)
}

func service(namespace, name string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Service
metadata:
  name: %s
  namespace: %s
spec:
  ports:
  - port: 80
`, name, namespace)
}

func TestNode_KubernetesDNSResolvesAServiceOfItsOwnNamespaceAndOfAnotherAndAnExternalNameThroughTheNodesResolver(t *testing.T) {
	c, dir := startDNSCluster(t, node.KubeSpec{Resolver: true})
	assert.Contains(t, nodeOneUserData(t, dir), "name: addons\n", "the control-plane Node has the service that applies the add-ons: the control of the disabled case")
	const external = "external.naru.test"

	// The Nodes' resolver: a Pod on node-1's network, which answers external.naru.test with 192.0.2.53.
	resolver := strings.Replace(pod("resolver", "node-1", "resolver", "dns", node.QEMUNetwork(1, len(c.Nodes)).IP+":53", external+"=192.0.2.53"),
		"  restartPolicy: Never", "  hostNetwork: true\n  restartPolicy: Never", 1)
	out, err := kubectl(t, c, fmt.Sprintf(resolver, c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)

	kubectlEventually(t, c, "the DNS add-on's Deployment is applied", "-n", "kube-system", "get", "deployment", "coredns", "-o", "name")
	out, err = kubectl(t, c, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other\n", "apply", "-f", "-")
	require.NoError(t, err, out)
	for _, s := range []string{service("default", "web"), service("other", "web")} {
		out, err = kubectl(t, c, s, "apply", "-f", "-")
		require.NoError(t, err, out)
	}
	own := kubectlEventually(t, c, "the Service of the default namespace has a ClusterIP", "get", "service", "web", "-o", "jsonpath={.spec.clusterIP}")
	elsewhere := kubectlEventually(t, c, "the Service of the other namespace has a ClusterIP", "-n", "other", "get", "service", "web", "-o", "jsonpath={.spec.clusterIP}")
	require.NotEqual(t, own, elsewhere)

	// The Pod is on node-2, so its queries cross the Nodes: kube-proxy to CoreDNS, CoreDNS to node-1's resolver.
	out, err = kubectl(t, c, fmt.Sprintf(pod("lookup", "node-2", "lookup", "resolve", "web", "web.other", external+"."), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	want := []string{
		"naru91-entry: resolved web " + own,
		"naru91-entry: resolved web.other " + elsewhere,
		"naru91-entry: resolved " + external + ". 192.0.2.53",
	}
	var logs string
	ok := assert.Eventually(t, func() bool {
		logs, _ = kubectl(t, c, "", "logs", "lookup")
		for _, w := range want {
			if !strings.Contains(logs, w) {
				return false
			}
		}
		return true
	}, kubeTimeout, 3*time.Second, "the Pod resolves a Service of its namespace, a Service of another and an external name")
	if !ok {
		coredns, _ := kubectl(t, c, "", "-n", "kube-system", "get", "pods", "-o", "wide")
		t.Logf("kube-system:\n%s\nthe lookup Pod's log:\n%s", coredns, logs)
		t.FailNow()
	}
}

func TestNode_KubernetesDisabledDNSGivesTheControlPlaneNodeNothingToApplyAndWhatTheAdministratorKeptInKubeSystemIsLeftAlone(t *testing.T) {
	c, dir := startDNSCluster(t, node.KubeSpec{DisableAddons: []string{"dns"}})

	// The setup asked for the cluster without DNS: node-1 has no service that applies add-ons, and
	// the kubelet is still told the cluster's DNS address, so a Pod would use the administrator's.
	userData := nodeOneUserData(t, dir)
	require.NotContains(t, userData, "name: addons\n")
	require.Contains(t, userData, "clusterDNS")

	// node-1 runs kube-proxy last when it has no add-ons to apply, so once it has started and the API
	// server is ready, nothing is left that could still apply an add-on.
	require.NoError(t, c.Nodes[0].WaitFor(context.Background(), bootTimeout, "started", "service=kube-proxy"))
	kubectlEventually(t, c, "the API server is ready", "get", "--raw=/readyz")

	// The administrator's own cluster DNS, under the name the add-on would use.
	const mine = `apiVersion: v1
kind: Service
metadata:
  name: kube-dns
  namespace: kube-system
  annotations:
    example.test/owner: administrator
spec:
  clusterIP: 10.96.0.10
  ports:
  - name: dns
    port: 53
    protocol: UDP
`
	out, err := kubectl(t, c, mine, "apply", "-f", "-")
	require.NoError(t, err, out)

	deployments, err := kubectl(t, c, "", "-n", "kube-system", "get", "deployments", "-o", "name")
	require.NoError(t, err, deployments)
	assert.NotContains(t, deployments, "coredns", "a cluster that disables DNS gets no CoreDNS")
	owner, err := kubectl(t, c, "", "-n", "kube-system", "get", "service", "kube-dns", "-o", `jsonpath={.metadata.annotations.example\.test/owner}`)
	require.NoError(t, err, owner)
	assert.Equal(t, "administrator", owner, "the Service the administrator kept is neither replaced nor pruned")
	managers, err := kubectl(t, c, "", "-n", "kube-system", "get", "service", "kube-dns", "-o", "jsonpath={.metadata.managedFields[*].manager}")
	require.NoError(t, err, managers)
	assert.NotContains(t, managers, "naru-addons")
}
