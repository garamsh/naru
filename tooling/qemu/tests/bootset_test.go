//go:build qemu_real

package tests_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/release"
)

// The test of the Node boot set (ADR 0046) boots one Node that runs naru-hostd, takes a release's
// boot set from the files make image wrote, adds it to the host through `naructl host boot-set add`
// with no reboot, creates a Node microVM from it with the user-data osFormSeed writes, and removes
// both. The harness route reaches the microVM's Node API in bootset_ready_test.go.

// osFormSeed writes into dir the user-data of a Node that is a microVM of the OS form and runs no
// service: its data disk, which hostd gives it blank, and its Node API, whose serving pair names
// address and is issued from the Node API authority in ca (gen ca and gen api-ca). naructl writes no
// such Node by itself, so spec.api is taken from what gen node writes for that address.
func osFormSeed(t *testing.T, ca, name, address, dir string) string {
	t.Helper()
	gen := filepath.Join(dir, name+"-gen.yaml")
	require.NoError(t, tool.GenNode(naructl.Node{
		CA: ca, Name: name, Address: address + "/24", APIServer: address, PairsAsFiles: true,
		MachinePrefix: "10.201.0.0/24", Firecracker: strings.Repeat("0", 64), Out: gen,
	}))
	raw, err := os.ReadFile(gen)
	require.NoError(t, err)
	var written config.NodeConfig
	require.NoError(t, yaml.Unmarshal(raw, &written))
	require.NotNil(t, written.Spec.API, "gen node issued the Node API's serving pair")
	cfg := config.NodeConfig{APIVersion: written.APIVersion, Kind: written.Kind, Metadata: config.Metadata{Name: name}}
	cfg.Spec.DataDisk = config.DataDisk{Label: wire.DataLabel, MountPoint: wire.DataMount, FormatBlank: true}
	cfg.Spec.API = written.Spec.API
	out, err := yaml.Marshal(cfg)
	require.NoError(t, err)
	_, err = config.Parse(out)
	require.NoError(t, err, "init takes the seed")
	seed := filepath.Join(dir, name+"-user-data")
	require.NoError(t, os.WriteFile(seed, out, 0o600))
	return seed
}

func TestNode_HostAPITakesAReleasesNodeBootSetWithNoRebootAndCreatesANodeMicroVMFromIt(t *testing.T) {
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

	// The user-data of a Node that is a microVM.
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	require.NoError(t, tool.GenCA(ca))
	require.NoError(t, tool.GenAPICA(ca))
	seed := osFormSeed(t, ca, "vm", "10.200.0.2", dir)

	// The microVM is created from the boot set, with its seed and a data disk.
	made := hctl(prov, []string{"microvm", "create"}, "--name", "vm", "--boot-set", set, "--seed", seed,
		"--data-disk", "1073741824")
	require.NoError(t, made.Err, made.refusal())
	eventually(t, "mvmd lists the Machine Running", func() bool { return strings.Contains(machines(nd), "=RUNNING") })

	// The set a microVM names stays; once the microVM is gone, the set is removed.
	inUse := hctl(prov, []string{"host", "boot-set", "remove"}, set)
	require.Error(t, inUse.Err)
	assert.Contains(t, inUse.refusal(), "409")
	deleted := hctl(prov, []string{"microvm", "delete"}, "vm")
	require.NoError(t, deleted.Err, deleted.refusal())
	eventually(t, "mvmd no longer lists the Machine", func() bool { return machines(nd) == "none" })
	removed := hctl(prov, []string{"host", "boot-set", "remove"}, set)
	require.NoError(t, removed.Err, removed.refusal())
}
