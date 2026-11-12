//go:build qemu_real

package tests_test

import (
	"testing"
	"time"

	"github.com/garamsh/naru/tooling/qemu/internal/node"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The tests of the reboot (ADR 0022) run the naructl of the checkout against one Node. QEMU runs
// with -no-reboot, so a guest that reboots ends its QEMU process: that is what each test waits on,
// and the Node is then started again on its disks, as a hypervisor does.

// awaitRebootAndRestart waits for the Node's QEMU process to end by itself, starts it again and
// waits for its API.
func awaitRebootAndRestart(t *testing.T, nd *node.Node, admin string) {
	t.Helper()
	select {
	case <-nd.Done():
	case <-time.After(bootTimeout):
		t.Fatal("the Node did not reboot")
	}
	require.NoError(t, nd.Reboot())
	eventually(t, "the Node serves its API after the reboot", func() bool { return ctl(admin, "version").Err == nil })
}

func TestNode_NaructlRebootAnswersThenTheNodeStopsItsServicesAndBootsAgainAtTheSameRevision(t *testing.T) {
	c := nodeAPI(t)
	admin := c.API.Admin
	require.NoError(t, ctl(admin, "apply", "-f", patchFile(t, idle("1m"))).Err)
	_, bootID := versionOf(t, admin)

	r := ctl(admin, "reboot")

	require.NoError(t, r.Err, r.refusal())
	assert.Contains(t, r.Stdout, "the Node accepted the reboot", "the client has its answer before the Node is gone")
	awaitRebootAndRestart(t, c.Nodes[0], admin)
	_, after := versionOf(t, admin)
	assert.NotEqual(t, bootID, after, "a reboot is a new boot")
	eventually(t, "the service runs again", func() bool { return pids(t, admin)["naru49-idle"] != "" })
	assert.Equal(t, 1, revisionOf(t, admin), "a reboot changes no revision")
}

func TestNode_ARebootModeApplyStoresAFieldOnlyARebootCanChangeThenTheNodeBootsAtTheNewRevision(t *testing.T) {
	c := nodeAPI(t)
	admin := c.API.Admin
	_, bootID := versionOf(t, admin)
	forwarding := patchFile(t, "spec:\n  network:\n    forwarding: [ipv4, ipv6]\n")

	refused := ctl(admin, "apply", "-f", forwarding, "--mode", "no-reboot")
	require.Error(t, refused.Err, "the control: the same patch is refused without a reboot")
	assert.Contains(t, refused.refusal(), "spec.network")
	require.Equal(t, 0, revisionOf(t, admin))

	applied := ctl(admin, "apply", "-f", forwarding, "--mode", "reboot")

	require.NoError(t, applied.Err, applied.refusal())
	assert.Contains(t, applied.Stdout, "applied revision 1, mode reboot")
	assert.Contains(t, applied.Stdout, "reboot node")
	awaitRebootAndRestart(t, c.Nodes[0], admin)
	_, after := versionOf(t, admin)
	assert.NotEqual(t, bootID, after, "a reboot is a new boot")
	assert.Equal(t, 1, revisionOf(t, admin), "the Node came back on the new revision")
	got := ctl(admin, "get", "config")
	require.NoError(t, got.Err, got.refusal())
	assert.Contains(t, got.Stdout, "ipv6", "the reboot-only field is in the configuration")
}
