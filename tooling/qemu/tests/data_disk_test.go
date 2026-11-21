//go:build qemu_real

package tests_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/judge"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// dataDiskGrownSize is the size the data disk is enlarged to, from the 8 GiB the harness gives it.
const dataDiskGrownSize = 10 << 30

// TestNode_ADataDiskGrownBetweenBootsHasItsFileSystemGrownAtTheNextBoot enlarges a Node's data disk
// while it is off, as an operator enlarges the volume of a control host, and checks that init grows
// the data file system at the next boot (ADR 0088).
func TestNode_ADataDiskGrownBetweenBootsHasItsFileSystemGrownAtTheNextBoot(t *testing.T) {
	dir := t.TempDir()
	c, err := node.StartCluster(node.ClusterSpec{
		N: 1, Disk: filepath.Join(os.Getenv("NARU_IMAGE_DIR"), "naru.img"), Dir: dir, Inputs: inputs, Naructl: tool,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		c.Stop()
		t.Logf("console:\n%s", c.Nodes[0].Console())
	})
	nd := c.Nodes[0]
	expectAll(t, nd, judge.Boot(nd.Name))
	assert.False(t, nd.Has("data disk file system grown"), "the control: nothing to grow at the first boot")
	nd.Stop()
	disks, err := filepath.Glob(filepath.Join(dir, "*", "data.img"))
	require.NoError(t, err)
	require.Len(t, disks, 1, "the Node's data disk")
	require.NoError(t, os.Truncate(disks[0], dataDiskGrownSize))

	require.NoError(t, nd.Reboot())

	require.NoError(t, nd.WaitFor(context.Background(), bootTimeout, "data disk file system grown"), "init grew the data file system")
	assert.False(t, nd.Has("data disk not grown"), "and did not refuse to")
}
