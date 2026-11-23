//go:build qemu_real

package tests_test

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// The control of the positive case in bootset_ready_test.go (ADR 0096): a microVM Node of the default
// volumes add-on whose initramfs carries a description of the image that names another extension,
// so the delivery is removed and nothing else is changed. Its first boot is refused as it was before
// the delivery existed, and its Node API stays up.

// newc is one file of a cpio archive in the kernel's "newc" format.
func newc(name, content string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X", 1, 0o100644, 0, 0, 1, 0, len(content), 0, 0, 0, 0, len(name)+1, 0)
	b.WriteString(name + "\x00")
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	b.WriteString(content)
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

// spoilNodeInitramfs writes a copy of the Node initramfs followed by one more archive that replaces the
// description of the volumes image with one that names another extension. The kernel unpacks the
// archives of a concatenation in order, so the later file wins.
func spoilNodeInitramfs(t *testing.T, initrd string) string {
	t.Helper()
	orig, err := os.ReadFile(initrd)
	require.NoError(t, err)
	var tail bytes.Buffer
	zw := gzip.NewWriter(&tail)
	_, err = zw.Write(newc("usr/lib/naru/boot-extensions/naru-volumes.json", `{"name":"naru-other","version":"v0","sha256":"`+strings.Repeat("0", 64)+`","services":[]}`))
	require.NoError(t, err)
	_, err = zw.Write(newc("TRAILER!!!", ""))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	out := filepath.Join(t.TempDir(), "node-initrd.cpio.gz")
	require.NoError(t, os.WriteFile(out, append(orig, tail.Bytes()...), 0o600))
	return out
}

func TestNode_ABlankMicroVMNodeWithTheDefaultVolumesAddOnRefusesItsFirstBootWhenItsInitramfsCarriesNoImage(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 1, Gaon: true, NodeAPI: true, Host: true, MemoryMiB: 2*node.KubeNodeMemoryMiB + 1024})
	require.NotNil(t, c.Host)
	eventually(t, "the host API answers a reader", func() bool { return hctl(c.Host.Reader, []string{"host"}).Err == nil })
	nd, prov := c.Nodes[0], c.Host.Provisioner
	require.NoError(t, nd.WaitFor(t.Context(), bootTimeout, "started", "service=naru-hostd"))
	image, guestDir := os.Getenv("NARU_IMAGE_DIR"), os.Getenv("NARU_GUEST_DIR")
	assets := t.TempDir()
	require.NoError(t, release.WriteBootSet(assets, "qemu-spoiled",
		filepath.Join(guestDir, "boot-sets", release.NodeBootSet, "vmlinux"), spoilNodeInitramfs(t, filepath.Join(image, "node-initrd.cpio.gz"))))
	const set = "naru-node-qemu-spoiled"
	added := hctl(prov, []string{"host", "boot-set", "add"}, filepath.Join(assets, set+".json"))
	require.NoError(t, added.Err, added.refusal())

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenKubernetes(ca, "10.200.0.2", ""))
	seed := filepath.Join(dir, "user-data")
	require.NoError(t, tool.GenKubernetesNode(naructl.KubernetesNode{
		CA: ca, Name: "kcp", Role: "control-plane", Address: "10.200.0.2/24", Endpoint: "10.200.0.2",
		PodNetwork: node.KubePodNetwork, PodPrefix: node.KubePodPrefix(1), Out: seed,
	}))
	raw, err := os.ReadFile(seed)
	require.NoError(t, err)
	require.Contains(t, string(raw), "naru-volumes", "the seed lists the extension with no digest, as the default add-on does")
	made := hctl(prov, []string{"microvm", "create"}, "--name", "kcp", "--boot-set", set, "--seed", seed,
		"--data-disk", "2147483648", "--pod-prefix", node.KubePodPrefix(1))
	require.NoError(t, made.Err, made.refusal())

	// A microVM's console is its vmm.log in mvmd's data, which the host reads from a copy of the Node's data disk.
	var log string
	require.Eventually(t, func() bool {
		sn, err := nd.SnapshotData(filepath.Join(dir, fmt.Sprintf("data-%d.img", time.Now().UnixNano())))
		if err != nil {
			return false
		}
		ids, _ := sn.List(wire.DataMount + "/mvmd/machines")
		for _, id := range ids {
			b, err := sn.Read(wire.DataMount + "/mvmd/machines/" + id + "/run/vmm.log")
			if err == nil {
				log = string(b)
			}
		}
		return strings.Contains(log, `no image of "naru-volumes" is stored`)
	}, 4*time.Minute, 15*time.Second, "the microVM's first boot is refused for want of the image")
	assert.NotContains(t, log, "extension staged from the boot medium")
}
