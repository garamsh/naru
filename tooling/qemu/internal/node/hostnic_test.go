package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func qemuLine(t *testing.T, c Config) string {
	t.Helper()
	spec := Spec{Config: c}
	spec.Dir, spec.Disk, spec.Mcast = t.TempDir(), filepath.Join(t.TempDir(), "naru.img"), "230.78.0.1:5000"
	require.NoError(t, os.WriteFile(spec.Disk, []byte("disk"), 0o644))
	args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	return strings.Join(args, " ")
}

func TestQemuArgs_TheHostNICForwardsLoopbackToTheAPIServer(t *testing.T) {
	c := cfg(1, 2, true, false)
	c.HostPort = 41234

	line := qemuLine(t, c)

	assert.Contains(t, line, "-netdev user,id=net1,restrict=on,hostfwd=tcp:127.0.0.1:41234-10.0.2.15:6443")
	assert.Contains(t, line, "virtio-net-pci,netdev=net1,mac=02:00:00:01:00:01")
}

func TestQemuArgs_ANodeWithoutAHostPortHasOneNIC(t *testing.T) {
	assert.NotContains(t, qemuLine(t, cfg(2, 2, false, false)), "net1")
}
