//go:build qemu_real

package tests_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/naructl"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The blank Node of the default volumes add-on (ADR 0096): its data disk has no extension store, its
// configuration lists `naru-volumes` with no digest as `gen kubernetes-node` writes it, and nothing
// is delivered by hand. The image is on the ESP of the boot disk, and init takes it from there into
// the store before it mounts it.

// espIndexMagic is how the ESP's extension index starts, which the disk builder writes.
var espIndexMagic = []byte(`{"extensions":[{"name":"naru-volumes"`)

// blankInputs is the inputs of the harness without the extension store the data disk is built with.
func blankInputs() (blank fixture.Inputs) {
	blank = inputs
	blank.Extension.StoreDir = ""
	return blank
}

// spoilESPIndex makes the ESP's extension index of the boot disk at path unreadable as JSON: the
// delivery removed and nothing else changed.
func spoilESPIndex(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	buf := make([]byte, 64<<20)
	keep := len(espIndexMagic) - 1
	var at int64
	found := 0
	for {
		n, err := io.ReadFull(f, buf[keep:])
		window := buf[:keep+n]
		if i := bytes.Index(window, espIndexMagic); i >= 0 {
			_, werr := f.WriteAt([]byte("X"), at-int64(keep)+int64(i))
			require.NoError(t, werr)
			found++
		}
		if err != nil {
			break
		}
		at += int64(n)
		copy(buf, window[len(window)-keep:])
	}
	require.Equal(t, 1, found, "the boot disk's ESP holds one extension index: the setup of this test is what it asked for")
}

func TestNode_ABlankNodeWithTheDefaultVolumesAddOnBootsTheFirstTimeWithTheImageFromItsESP(t *testing.T) {
	nd, admin := startUpgradeNodeOn(t, tool, blankInputs(), copyBootDisk(t), withVolumesExtension)

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "extension staged from the boot medium", "naru-volumes"), "init took the image from the ESP")
	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "extension mounted", "naru-volumes"), "and mounted it at the first boot")
	assert.False(t, nd.Has("boot refused"), "the first boot is not refused")
	list := ctlWith(tool, admin, "extension", "list")
	require.NoError(t, list.Err, list.refusal())
	assert.Contains(t, list.Stdout, "naru-volumes", "the Node stores the image it was not given by hand")
}

func TestNode_ABlankNodeWithTheDefaultVolumesAddOnRefusesItsFirstBootWhenTheESPCarriesNoImage(t *testing.T) {
	// The control of the test above: the same Node and the same disk with the index spoiled.
	disk := copyBootDisk(t)
	spoilESPIndex(t, disk)

	nd, _ := startUpgradeNodeOn(t, tool, blankInputs(), disk, withVolumesExtension)

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, `no image of "naru-volumes" is stored`), "the boot is refused as it was before the delivery")
	assert.False(t, nd.Has("extension staged from the boot medium"))
}

func withVolumesExtension(_ *node.Config, gen *naructl.Node) {
	gen.Extensions = []string{"naru-volumes"}
}
