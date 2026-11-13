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

func TestQemuArgs_AHostReachNICIsNotRestricted(t *testing.T) {
	c := cfg(1, 2, true, false)
	c.HostPort, c.HostReach = 41234, true

	line := qemuLine(t, c)

	assert.Contains(t, line, "-netdev user,id=net1,hostfwd=tcp:127.0.0.1:41234-10.0.2.15:6443")
	assert.NotContains(t, line, "restrict")
}

func TestQemuArgs_AMemoryOfTheConfigReplacesTheDefault(t *testing.T) {
	c := cfg(1, 2, true, false)

	assert.Contains(t, qemuLine(t, c), "-m 3072 ")
	c.MemoryMiB = 4096
	assert.Contains(t, qemuLine(t, c), "-m 4096 ")
}

func TestQemuArgs_ARTCBaseStartsTheNodesClockAtThatDate(t *testing.T) {
	spec := Spec{Config: cfg(1, 1, true, false), RTCBase: "2021-03-04T05:06:07"}
	spec.Dir, spec.Disk, spec.Mcast = t.TempDir(), filepath.Join(t.TempDir(), "naru.img"), "230.78.0.1:5000"
	require.NoError(t, os.WriteFile(spec.Disk, []byte("disk"), 0o644))

	args, err := qemuArgs(spec, "naru15-node-1", "cidata.img", "data.img", "console.log")

	require.NoError(t, err)
	assert.Contains(t, strings.Join(args, " "), "-rtc base=2021-03-04T05:06:07,clock=vm")
	assert.Contains(t, strings.Join(args, " "), "-cpu host,-kvmclock")
	assert.NotContains(t, qemuLine(t, cfg(1, 1, true, false)), "-rtc")
	assert.NotContains(t, qemuLine(t, cfg(1, 1, true, false)), "kvmclock")
}
