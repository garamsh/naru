//go:build qemu_real

package tests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// The test of the Node boot set (ADR 0046) boots one Node that runs naru-hostd, takes a release's
// boot set from the files make image wrote, adds it to the host through `naructl host boot-set add`
// with no reboot, creates a Node microVM from it with the user-data `naructl gen kubernetes-node`
// writes for the control-plane role, and removes both. It does not reach the microVM's API server:
// the harness has no route from the test host to a microVM's address.

func TestNode_HostAPITakesAReleasesNodeBootSetWithNoRebootAndCreatesAControlPlaneNodeMicroVMFromIt(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	prov, reader := c.Host.Provisioner, c.Host.Reader
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")

	// The release's files: the kernel of the guest form and the Node initramfs, which make release
	// copies beside their manifest.
	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-test",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), filepath.Join(image, "node-initrd.cpio.gz")))
	manifest := filepath.Join(assets, "naru-node-qemu-test.json")
	const set = "naru-node-qemu-test"

	// A reader cannot add a boot set; the provisioner adds it with no reboot, and again is accepted.
	denied := hctl(reader, []string{"host", "boot-set", "add"}, manifest)
	require.Error(t, denied.Err)
	assert.Contains(t, denied.refusal(), "403")
	added := hctl(prov, []string{"host", "boot-set", "add"}, manifest)
	require.NoError(t, added.Err, added.refusal())
	assert.Contains(t, added.Stdout, "added boot set "+set)
	again := hctl(prov, []string{"host", "boot-set", "add"}, manifest)
	require.NoError(t, again.Err, again.refusal())

	// The user-data of a control-plane Node that is a microVM.
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenKubernetes(ca, "10.200.0.2", ""))
	seed := filepath.Join(dir, "user-data")
	require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{
		CA: ca, Name: "kcp", Role: "control-plane", Address: "10.200.0.2/24", Endpoint: "10.200.0.2",
		PodNetwork: node.KubePodNetwork, PodPrefix: node.KubePodPrefix(1), Out: seed,
	}))

	// The microVM is created from the boot set, with its seed, a data disk and its Pod prefix.
	made := hctl(prov, []string{"microvm", "create"}, "--name", "kcp", "--boot-set", set, "--seed", seed,
		"--data-disk", "1073741824", "--pod-prefix", node.KubePodPrefix(1))
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists the Machine Running", func() bool { return strings.Contains(machines(nd), "=RUNNING") })

	// The set a microVM names stays; once the microVM is gone, the set is removed.
	inUse := hctl(prov, []string{"host", "boot-set", "remove"}, set)
	require.Error(t, inUse.Err)
	assert.Contains(t, inUse.refusal(), "409")
	deleted := hctl(prov, []string{"microvm", "delete"}, "kcp")
	require.NoError(t, deleted.Err, deleted.refusal())
	eventually(t, "mvmd no longer lists the Machine", func() bool { return machines(nd) == "none" })
	removed := hctl(prov, []string{"host", "boot-set", "remove"}, set)
	require.NoError(t, removed.Err, removed.refusal())
}
