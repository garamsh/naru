//go:build qemu_real

package tests_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The tests of the host API (ADR 0028) boot one Node that runs naru-hostd, with the
// user-data naructl writes from the host authority of `naructl gen host-ca`, and run the naructl
// built from the checkout on the host against node-1's host API through the host forward. The
// Machines mvmd holds are read from the console of node-1, where naru-mvmdlist prints them.

// hctl runs the naructl of the checkout as a host API client file names: words is the command
// ("microvm", "create" is two), and the rest follow --config.
func hctl(file string, command []string, args ...string) result {
	cmd := append(append(append([]string{}, command...), "--config", file), args...)
	res := tool.Run(cmd...)
	return result{Stdout: res.Stdout, Stderr: res.Stderr, Err: res.Err}
}

var machinesLine = regexp.MustCompile(`naru88-machines: (\S+)`)

// machines is what mvmd of the Node held when naru-mvmdlist last said: "none", or id=STATE pairs.
func machines(nd *node.Node) string {
	all := machinesLine.FindAllStringSubmatch(nd.Console(), -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1][1]
}

func hostCluster(t *testing.T) *node.Cluster {
	t.Helper()
	c := start(t, node.ClusterSpec{N: 1, GenNode: true, NodeAPI: true, Host: true})
	require.NotNil(t, c.Host)
	eventually(t, "the host API answers a reader", func() bool { return hctl(c.Host.Reader, []string{"host"}).Err == nil })
	return c
}

type hostInfo struct {
	Region string            `json:"region"`
	Labels map[string]string `json:"labels"`
}

func hostOf(t *testing.T, file string) hostInfo {
	t.Helper()
	r := hctl(file, []string{"host"})
	require.NoError(t, r.Err, r.refusal())
	var h hostInfo
	require.NoError(t, json.Unmarshal([]byte(r.Stdout), &h), r.Stdout)
	return h
}

func TestNode_HostAPICreatesAMicroVMThatRunsShowsItAndDeletesItFromMvmdAndKeepsItsRolesAndItsRequestsIdempotent(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	prov, reader := c.Host.Provisioner, c.Host.Reader
	image := inputs.Image.Repository + "@" + inputs.Image.Digest
	create := func(file string, extra ...string) result {
		args := append([]string{"--name", "web", "--image", image, "--disk", "data=1048576"}, extra...)
		return hctl(file, []string{"microvm", "create"}, args...)
	}
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "started", "service=naru-hostd"))

	// /v1/host returns the region and the labels from the user-data.
	h := hostOf(t, reader)
	assert.Equal(t, node.HostRegion, h.Region)
	assert.Equal(t, map[string]string{"disk": "ssd"}, h.Labels)

	// A reader's POST is the host's 403, and a client from another CA fails the handshake; the
	// provisioner's file, with the same arguments, is accepted.
	denied := create(reader)
	require.Error(t, denied.Err)
	assert.Contains(t, denied.refusal(), "403")
	foreign := hctl(c.Host.Foreign, []string{"host"})
	require.Error(t, foreign.Err)
	assert.Contains(t, foreign.refusal(), "certificate", "the handshake refuses the foreign pair")
	assert.NoError(t, hctl(c.Host.Provisioner, []string{"host"}).Err)
	assert.NotContains(t, machines(nd), "web=", "the refused requests made nothing")

	// create makes a Machine that reaches Running; get and list show it with its address.
	made := create(prov)
	require.NoError(t, made.Err, made.refusal())
	assert.Contains(t, made.Stdout, "created web")
	eventually(t, "mvmd lists the Machine Running", func() bool { return strings.Contains(machines(nd), "web=RUNNING") })
	var vm struct {
		Name      string   `json:"name"`
		State     string   `json:"state"`
		Addresses []string `json:"addresses"`
	}
	got := hctl(reader, []string{"microvm", "get"}, "web")
	require.NoError(t, got.Err, got.refusal())
	require.NoError(t, json.Unmarshal([]byte(got.Stdout), &vm), got.Stdout)
	assert.Equal(t, "web", vm.Name)
	assert.Equal(t, "running", vm.State)
	require.NotEmpty(t, vm.Addresses, "the Machine has an address")
	list := hctl(reader, []string{"microvm", "list"})
	require.NoError(t, list.Err, list.refusal())
	assert.Contains(t, list.Stdout, "web")
	assert.Contains(t, list.Stdout, vm.Addresses[0])

	// A repeated POST with the same body is the same microVM; a different body under the name is 409.
	again := create(prov)
	require.NoError(t, again.Err, again.refusal())
	assert.Contains(t, again.Stdout, "exists with this request")
	assert.Contains(t, again.Stdout, vm.Addresses[0], "the same microVM")
	other := create(prov, "--cpu", "2")
	require.Error(t, other.Err)
	assert.Contains(t, other.refusal(), "409")

	// A label changed by naructl apply is what /v1/host says next, with no reboot.
	_, bootID := versionOf(t, c.API.Admin)
	applied := ctl(c.API.Admin, "apply", "-f", patchFile(t, "spec:\n  host:\n    labels:\n      disk: nvme\n"), "--mode", "no-reboot")
	require.NoError(t, applied.Err, applied.refusal())
	eventually(t, "/v1/host returns the new label", func() bool { return hostOf(t, reader).Labels["disk"] == "nvme" })
	_, after := versionOf(t, c.API.Admin)
	assert.Equal(t, bootID, after, "the Node did not reboot")
	assert.Equal(t, node.HostRegion, hostOf(t, reader).Region)

	// A reader's DELETE is 403; the provisioner's removes the microVM, and mvmd no longer lists it.
	refused := hctl(reader, []string{"microvm", "delete"}, "web")
	require.Error(t, refused.Err)
	assert.Contains(t, refused.refusal(), "403")
	assert.Contains(t, machines(nd), "web=RUNNING")
	deleted := hctl(prov, []string{"microvm", "delete"}, "web")
	require.NoError(t, deleted.Err, deleted.refusal())
	eventually(t, "mvmd no longer lists the Machine", func() bool { return machines(nd) == "none" })
	gone := hctl(reader, []string{"microvm", "get"}, "web")
	require.Error(t, gone.Err)
	assert.Contains(t, gone.refusal(), "404")
}

// rootMounted is the line of the guest's console that says it mounted its root disk, vdb, as ext4.
const rootMounted = "EXT4-fs (vdb): mounted filesystem"

// TestNode_AMicroVMTheHostAPICreatesRunsTheGuestKernelFromTheNodesRegistryOnAMkfsRootDisk has the
// host API create a microVM of the registry image by digest. The Node's registry served the image's
// manifest, the microVM's console (the vmm.log mvmd keeps for it) shows the guest kernel's banner and
// the guest mounting its root disk as ext4, and that root disk is the ext4 file system, with that
// UUID, that mvmd made with /bin/mkfs.ext4 (its disk.mkfs).
func TestNode_AMicroVMTheHostAPICreatesRunsTheGuestKernelFromTheNodesRegistryOnAMkfsRootDisk(t *testing.T) {
	c := hostCluster(t)
	nd := c.Nodes[0]
	ctx := context.Background()
	work := t.TempDir()
	made := hctl(c.Host.Provisioner, []string{"microvm", "create"}, "--name", "hello", "--image", inputs.Image.Repository+"@"+inputs.Image.Digest, "--disk", "data=1048576")
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists the Machine Running", func() bool { return strings.Contains(machines(nd), "hello=RUNNING") })

	// The image comes from the Node's test registry: its console shows the manifest served.
	require.NoError(t, nd.WaitFor(ctx, bootTimeout, "served", inputs.Image.Digest), "the registry served the manifest")

	// The microVM's console: mvmd writes the guest's serial output to the Machine's vmm.log, which
	// the host reads from a copy of the Node's data disk, replaced at each poll.
	var vmm string
	var snap *node.DataSnapshot
	eventually(t, "the microVM's console shows the guest mounting its root disk", func() bool {
		img := filepath.Join(work, "data.img")
		_ = os.Remove(img)
		var err error
		if snap, err = nd.SnapshotData(img); err != nil {
			return false
		}
		ids, err := snap.List(wire.DataMount + "/mvmd/machines")
		if err != nil {
			return false
		}
		for _, id := range ids {
			b, err := snap.Read(wire.DataMount + "/mvmd/machines/" + id + "/run/vmm.log")
			if err == nil && strings.Contains(string(b), rootMounted) {
				vmm = string(b)
				return true
			}
		}
		return false
	})
	t.Logf("the microVM's console (vmm.log):\n%s", vmm)
	mounted := regexp.MustCompile(`EXT4-fs \(vdb\): mounted filesystem ([0-9a-f-]{36})`).FindStringSubmatch(vmm)
	require.NotNil(t, mounted, "the guest mounted its root disk, vdb, as ext4")
	vmlinux, err := os.ReadFile(filepath.Join(os.Getenv("NARU_GUEST_DIR"), "kernels", "naru", "vmlinux"))
	require.NoError(t, err)
	banner := regexp.MustCompile(`Linux version [^\n\x00]+`).Find(vmlinux)
	require.NotEmpty(t, banner, "the guest kernel carries its banner")
	assert.Contains(t, vmm, string(banner), "the microVM booted the image's guest kernel")

	// The root disk is ext4, made by the mkfs mvmd runs.
	disks, err := snap.List(wire.DataMount + "/mvmd/disks")
	require.NoError(t, err)
	var disk string
	for _, n := range disks {
		if strings.HasSuffix(n, "-root") {
			disk = n
		}
	}
	require.NotEmpty(t, disk, "mvmd made the microVM's root disk: %v", disks)
	img := filepath.Join(work, "root.img")
	require.NoError(t, snap.Extract(wire.DataMount+"/mvmd/disks/"+disk, img))
	sb, err := node.Superblock(img)
	require.NoError(t, err)
	assert.Contains(t, sb, "Filesystem magic number:  0xEF53", "the root disk is an ext4 file system")
	assert.Contains(t, sb, "Filesystem UUID:          "+mounted[1], "it is the file system the guest mounted, which mvmd made with -U")
}

func TestNode_ANodeWithoutSpecHostRunsNoNaruHostd(t *testing.T) {
	c := nodeAPI(t)
	nd := c.Nodes[0]
	eventually(t, "every service of node-1 is listed running", func() bool {
		running := pids(t, c.API.Reader)
		for _, s := range servicesOf(t, nd) {
			if running[s] == "" {
				return false
			}
		}
		return true
	})
	assert.NotContains(t, pids(t, c.API.Reader), "naru-hostd")
	assert.NotContains(t, nd.Console(), "service=naru-hostd")
	assert.NotContains(t, nd.Console(), "naru88-machines")
}
