//go:build qemu_real

package tests_test

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The volume test (ADR 0083) boots the two-Node cluster with a blank extra disk on each Node. In the first
// cluster the Node's configuration makes the disk a storage disk, which init formats as XFS and mounts with
// project quota, and the default class naru-local serves directories of it: a claim binds, a Pod writes, the
// Pod is deleted and a new Pod finds the data, and a Pod that fills a small volume is stopped by its quota
// (EDQUOT). In the second the Node offers the disk whole, and the class naru-block serves it as a raw device.
// Neither cluster uses a registry beyond the test image: the add-on's image is imported from the extension.

// volumePod is a Pod that runs the test image's entry with args and the claim mounted at /data, or, with
// device, the claim as the raw device /dev/vol. It names no Node, so the scheduler binds a claim that
// waits for its first consumer.
func volumePod(name, claim string, device bool, args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = fmt.Sprintf("%q", a)
	}
	mount := "volumeMounts: [{name: vol, mountPath: /data}]"
	if device {
		mount = "volumeDevices: [{name: vol, devicePath: /dev/vol}]"
	}
	return fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: %s
spec:
  restartPolicy: Never
  containers:
  - name: entry
    image: %%s
    command: ["/bin/entry"]
    args: [%s]
    %s
  volumes:
  - name: vol
    persistentVolumeClaim: {claimName: %s}
`, name, strings.Join(quoted, ", "), mount, claim)
}

func claim(name, class, mode, size string) string {
	if class != "" {
		class = "\n  storageClassName: " + class
	}
	if mode != "" {
		mode = "\n  volumeMode: " + mode
	}
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
spec:
  accessModes: [ReadWriteOnce]%s%s
  resources:
    requests:
      storage: %s
`, name, class, mode, size)
}

// logEventually waits until the Pod's log has want, and shows the Pod and the claims when it does not.
func logEventually(t *testing.T, c *node.KubeCluster, pod, want string) string {
	t.Helper()
	var logs string
	ok := assert.Eventually(t, func() bool {
		logs, _ = kubectl(t, c, "", "logs", pod)
		return strings.Contains(logs, want)
	}, kubeTimeout, 3*time.Second, "the log of "+pod+" has "+want)
	if !ok {
		desc, _ := kubectl(t, c, "", "describe", "pod", pod)
		pvc, _ := kubectl(t, c, "", "get", "pvc,pv", "-o", "wide")
		ds, _ := kubectl(t, c, "", "-n", "kube-system", "get", "pods", "-o", "wide")
		t.Logf("%s's log:\n%s\n%s\n%s\n%s", pod, logs, desc, pvc, ds)
		t.FailNow()
	}
	return logs
}

func apply(t *testing.T, c *node.KubeCluster, manifest string) {
	t.Helper()
	out, err := kubectl(t, c, manifest, "apply", "-f", "-")
	require.NoError(t, err, out)
}

// startVolumeCluster boots a cluster whose Nodes have the extra disk as mode says, and waits for the driver to
// register on both: the CSINode of each lists naru-volumes.
func startVolumeCluster(t *testing.T, mode string) *node.KubeCluster {
	t.Helper()
	c, _ := startDNSCluster(t, node.KubeSpec{VolumeDisk: mode})
	for _, nd := range []string{"node-1", "node-2"} {
		kubectlEventually(t, c, nd+" registers the volume driver", "get", "csinode", nd, "-o", "jsonpath={.spec.drivers[*].name}")
	}
	return c
}

// pinned runs the Pod on the Node named nd.
func pinned(pod, nd string) string {
	return strings.Replace(pod, "spec:\n", "spec:\n  nodeSelector: {kubernetes.io/hostname: "+nd+"}\n", 1)
}

// otherNode is the one of the two Nodes that is not nd.
func otherNode(nd string) string {
	if nd == "node-1" {
		return "node-2"
	}
	return "node-1"
}

func TestNode_KubernetesALocalVolumeBindsAPodWritesItAndANewPodOnTheSameNodeFindsTheDataWhileOneOnTheOtherNodeCannotStart(t *testing.T) {
	c := startVolumeCluster(t, node.VolumeDiskStorage)
	class := kubectlEventually(t, c, "naru-local is the default class", "get", "storageclass", "naru-local", "-o", `jsonpath={.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}`)
	require.Equal(t, "true", class)

	// The claim names no class: the default serves it.
	apply(t, c, claim("data", "", "", "64Mi"))
	apply(t, c, fmt.Sprintf(volumePod("writer", "data", false, "volume", "/data", "naru-was-here"), c.Image))
	logEventually(t, c, "writer", "naru-entry: volume wrote naru-was-here")
	assert.Equal(t, "naru-local", kubectlEventually(t, c, "the claim is served by the default class", "get", "pvc", "data", "-o", "jsonpath={.spec.storageClassName}"))
	phase, _ := kubectl(t, c, "", "get", "pvc", "data", "-o", "jsonpath={.status.phase}")
	require.Equal(t, "Bound", phase)
	home := kubectlEventually(t, c, "the Node the writer ran on", "get", "pod", "writer", "-o", "jsonpath={.spec.nodeName}")

	out, err := kubectl(t, c, "", "delete", "pod", "writer", "--wait")
	require.NoError(t, err, out)
	// The volume is a directory of one Node's disk: a Pod pinned to the other Node never starts.
	apply(t, c, fmt.Sprintf(pinned(volumePod("elsewhere", "data", false, "volume", "/data", "not-written"), otherNode(home)), c.Image))
	apply(t, c, fmt.Sprintf(pinned(volumePod("reader", "data", false, "volume", "/data", "not-written"), home), c.Image))
	logEventually(t, c, "reader", "naru-entry: volume read naru-was-here")
	phase, _ = kubectl(t, c, "", "get", "pod", "elsewhere", "-o", "jsonpath={.status.phase}")
	assert.Equal(t, "Pending", phase, "the control reader on %s runs; the one on %s does not", home, otherNode(home))
}

func TestNode_KubernetesALocalVolumeStopsAPodThatFillsItWithTheQuotaError(t *testing.T) {
	c := startVolumeCluster(t, node.VolumeDiskStorage)
	apply(t, c, claim("small", "naru-local", "", "8Mi"))
	apply(t, c, fmt.Sprintf(volumePod("filler", "small", false, "fill", "/data"), c.Image))
	logs := logEventually(t, c, "filler", "naru-entry: fill ended after ")
	assert.Contains(t, logs, "disk quota exceeded", "the volume's project quota answers EDQUOT")
	m := regexp.MustCompile(`fill ended after (\d+) MiB`).FindStringSubmatch(logs)
	require.NotNil(t, m, logs)
	mib, _ := strconv.Atoi(m[1])
	assert.LessOrEqual(t, mib, 8, "no more than the 8 MiB the claim asked for was written")
}

func TestNode_KubernetesABlockVolumeIsADiskThePodWritesAndKeepsAfterItsClaimIsReleasedAndFreedByAnOperator(t *testing.T) {
	c := startVolumeCluster(t, node.VolumeDiskBlock)
	apply(t, c, claim("disk", "naru-block", "Block", "512Mi"))
	apply(t, c, fmt.Sprintf(volumePod("blocker", "disk", true, "block", "/dev/vol"), c.Image))
	logEventually(t, c, "blocker", "naru-entry: block verified 1048576")
	pv := kubectlEventually(t, c, "the claim is bound to a volume", "get", "pvc", "disk", "-o", "jsonpath={.spec.volumeName}")
	home := kubectlEventually(t, c, "the Node the blocker ran on", "get", "pod", "blocker", "-o", "jsonpath={.spec.nodeName}")

	// naru-block retains: deleting the claim leaves the volume, and the disk is never wiped.
	out, err := kubectl(t, c, "", "delete", "pod", "blocker", "--wait")
	require.NoError(t, err, out)
	out, err = kubectl(t, c, "", "delete", "pvc", "disk", "--wait")
	require.NoError(t, err, out)
	require.Eventually(t, func() bool {
		phase, _ := kubectl(t, c, "", "get", "pv", pv, "-o", "jsonpath={.status.phase}")
		return phase == "Released"
	}, kubeTimeout, 3*time.Second, "the volume is Released, not deleted")

	// The operator frees the disk by the standard release: set the volume's reclaim policy to Delete.
	// The driver forgets the claim and wipes nothing (ADR 0090).
	out, err = kubectl(t, c, "", "patch", "pv", pv, "-p", `{"spec":{"persistentVolumeReclaimPolicy":"Delete"}}`)
	require.NoError(t, err, out)
	require.Eventually(t, func() bool {
		_, err := kubectl(t, c, "", "get", "pv", pv)
		return err != nil
	}, kubeTimeout, 3*time.Second, "the released volume is deleted")

	// A new claim gets the same disk, and what the first Pod wrote is still on it.
	apply(t, c, claim("disk2", "naru-block", "Block", "512Mi"))
	apply(t, c, fmt.Sprintf(pinned(volumePod("again", "disk2", true, "block", "/dev/vol"), home), c.Image))
	logEventually(t, c, "again", "naru-entry: block kept 1048576")
}
