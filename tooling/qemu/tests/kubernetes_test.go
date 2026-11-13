//go:build qemu_real

package tests_test

import (
	"bytes"
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

// The test of Kubernetes on naru (ADR 0031) boots a control-plane Node and a worker, both naru, and
// uses the host's kubectl with the administrator's kubeconfig `naructl gen kubernetes` wrote. A Pod
// on node-1 serves, and a Pod on node-2 dials it by its Pod address and through a Service's
// ClusterIP, which kube-proxy on node-2 translates.

// kubeTimeout is how long a cluster takes to form and to run a Pod: the Nodes start etcd, the control
// plane and the kubelets, and containerd pulls the image from the registry service.
const kubeTimeout = 8 * time.Minute

// kubectl runs the host's kubectl against the cluster, with stdin when given.
func kubectl(t *testing.T, c *node.KubeCluster, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("kubectl", append([]string{"--kubeconfig", c.Kubeconfig, "--request-timeout=20s"}, args...)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// kubectlEventually runs kubectl until it succeeds and prints something, and returns what it printed.
func kubectlEventually(t *testing.T, c *node.KubeCluster, what string, args ...string) string {
	t.Helper()
	var got string
	require.Eventually(t, func() bool {
		out, err := kubectl(t, c, "", args...)
		got = out
		return err == nil && out != ""
	}, kubeTimeout, 3*time.Second, what)
	return got
}

// pod is a Pod on nodeName that runs the test image's entry with args.
func pod(name, nodeName, label string, args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
  labels:
    app: %s
spec:
  nodeName: %s
  restartPolicy: Never
  containers:
  - name: entry
    image: %%s
    command: ["/bin/entry"]
    args: [%s]
`, name, label, nodeName, strings.Join(quoted, ", "))
}

func TestNode_KubernetesTwoNodesAreReadyAndAPodOnTheWorkerReachesAPodOnTheControlPlaneAndAServiceClusterIP(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl is not installed on the test host")
	}
	spec := node.KubeSpec{Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: t.TempDir(), Inputs: inputs, Naructl: tool}
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
		for _, svc := range []string{"containerd", "kubelet", "kube-proxy"} {
			require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service="+svc), nd.Name+" "+svc)
		}
	}

	// Both Nodes are Ready, as the administrator's kubectl sees them.
	require.Eventually(t, func() bool {
		out, err := kubectl(t, c, "", "get", "nodes", "--no-headers")
		if err != nil {
			return false
		}
		ready := 0
		for _, line := range strings.Split(out, "\n") {
			f := strings.Fields(line)
			if len(f) >= 2 && f[1] == "Ready" {
				ready++
			}
		}
		return ready == 2
	}, kubeTimeout, 3*time.Second, "kubectl get nodes lists node-1 and node-2 Ready")
	nodes, err := kubectl(t, c, "", "get", "nodes")
	require.NoError(t, err)
	t.Logf("kubectl get nodes:\n%s", nodes)
	assert.Contains(t, nodes, "node-1")
	assert.Contains(t, nodes, "node-2")

	// A Pod on node-1 serves, and a Service selects it. The controller manager makes the namespace's
	// default ServiceAccount, which admission requires of a Pod, a little after the Nodes are Ready.
	kubectlEventually(t, c, "the default ServiceAccount exists", "get", "serviceaccount", "default", "-o", "name")
	out, err := kubectl(t, c, fmt.Sprintf(pod("serve", "node-1", "serve", "serve", ":8080"), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	out, err = kubectl(t, c, `apiVersion: v1
kind: Service
metadata:
  name: serve
spec:
  selector:
    app: serve
  ports:
  - port: 80
    targetPort: 8080
`, "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP := kubectlEventually(t, c, "the Pod on node-1 has an address", "get", "pod", "serve", "-o", "jsonpath={.status.podIP}")
	clusterIP := kubectlEventually(t, c, "the Service has a ClusterIP", "get", "service", "serve", "-o", "jsonpath={.spec.clusterIP}")
	assert.True(t, strings.HasPrefix(podIP, "10.64.1."), "a Pod of node-1 is in node-1's prefix: %s", podIP)
	assert.True(t, strings.HasPrefix(clusterIP, "10.96."), "the ClusterIP is in the Service range: %s", clusterIP)

	// A Pod on node-2 dials the first by its Pod address and the Service by its ClusterIP.
	byPod, byService := "http://"+podIP+":8080", "http://"+clusterIP+":80"
	out, err = kubectl(t, c, fmt.Sprintf(pod("dial", "node-2", "dial", "dial", byPod, byService), c.Image), "apply", "-f", "-")
	require.NoError(t, err, out)
	podIP2 := kubectlEventually(t, c, "the Pod on node-2 has an address", "get", "pod", "dial", "-o", "jsonpath={.status.podIP}")
	assert.True(t, strings.HasPrefix(podIP2, "10.64.2."), "a Pod of node-2 is in node-2's prefix: %s", podIP2)
	reached := assert.Eventually(t, func() bool {
		logs, _ := kubectl(t, c, "", "logs", "dial")
		return strings.Contains(logs, "naru91-entry: dialed "+byPod) && strings.Contains(logs, "naru91-entry: dialed "+byService)
	}, kubeTimeout, 3*time.Second, "the Pod on node-2 reaches the Pod on node-1 by its address and the Service by its ClusterIP")
	if !reached {
		last, _ := kubectl(t, c, "", "logs", "dial", "--tail=5")
		t.Logf("the dialing Pod's last lines:\n%s", last)
		t.FailNow()
	}
	logs, err := kubectl(t, c, "", "logs", "dial")
	require.NoError(t, err)
	t.Logf("kubectl logs dial:\n%s", logs)
	served, err := kubectl(t, c, "", "logs", "serve")
	require.NoError(t, err)
	assert.Contains(t, served, "naru91-entry: served "+podIP2+":", "the server saw the client's Pod address, not a Node's")
}
