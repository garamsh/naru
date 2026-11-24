package node

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQemuArgs_ADetachedSeedLeavesTheNoCloudDriveOut(t *testing.T) {
	dir := t.TempDir()
	boot := dir + "/naru.img"
	require.NoError(t, os.WriteFile(boot, nil, 0o644))
	spec := Spec{Config: cfg(1, 2, false), Disk: boot, Dir: dir, Mcast: "230.78.0.1:5000"}

	with, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)
	spec.NoSeed = true
	without, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")
	require.NoError(t, err)

	assert.Contains(t, strings.Join(with, " "), "id=cidata", "the control: the drive is attached by default")
	assert.NotContains(t, strings.Join(without, " "), "cidata")
	assert.Contains(t, strings.Join(without, " "), "id=data", "the data disk stays")
}
