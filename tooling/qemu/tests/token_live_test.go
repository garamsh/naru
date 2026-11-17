//go:build qemu_real

package tests_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// refusedWindow is how long a Node whose token was revoked is given to register before the test
// takes it as refused: far more than node-3 needs, which joins in the same boot.
const refusedWindow = 90 * time.Second

// TestNode_ABootstrapTokenAddedLiveJoinsANodeAndARevokedOneIsRefusedWithoutAReboot starts node-1 with
// the tokens of node-1 and node-2 only, adds node-3's token with `naructl token create`, and revokes
// node-4's with `naructl token revoke` before either boots (ADR 0081). The control-plane Node's boot
// ID is the same after both.
func TestNode_ABootstrapTokenAddedLiveJoinsANodeAndARevokedOneIsRefusedWithoutAReboot(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 4, Gaon: true, BootPKI: true, NodeAPI: true, Late: []int{3, 4}})
	api, err := hostapi.New(c.HostAddr, c.Client)
	require.NoError(t, err)
	ctx := context.Background()
	admin := c.API.Admin

	require.NoError(t, c.Nodes[0].WaitFor(ctx, bootTimeout, "started", "service=gaon-apiserver"))
	eventually(t, "node-1 Node API", func() bool { return ctl(admin, "version").Err == nil })
	eventually(t, "node-2 is registered and Ready", func() bool {
		n, err := api.Node(ctx, "node-2")
		return err == nil && n.Ready()
	})
	_, bootID := versionOf(t, admin)
	_, err = api.Node(ctx, "node-3")
	require.Error(t, err, "setup: node-3 is not registered before its token exists")

	require.NoError(t, tool.TokenCreate(c.CADir, "node-3", admin))
	require.NoError(t, tool.TokenCreate(c.CADir, "node-4", admin))
	// gen node would make node-4 a new token once the store has none, which no list holds either; the
	// revoked one is put back so node-4 presents the very token that was revoked.
	revoked, err := os.ReadFile(filepath.Join(c.CADir, "tokens", "node-4"))
	require.NoError(t, err)
	require.NoError(t, tool.TokenRevoke(c.CADir, "node-4", admin))
	require.NoError(t, os.WriteFile(filepath.Join(c.CADir, "tokens", "node-4"), revoked, 0o600))
	_, err = c.StartLate(3)
	require.NoError(t, err)
	late4, err := c.StartLate(4)
	require.NoError(t, err)

	eventually(t, "node-3 joined with the token added live", func() bool {
		n, err := api.Node(ctx, "node-3")
		return err == nil && n.Ready()
	})
	require.NoError(t, late4.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"))
	time.Sleep(refusedWindow)
	_, err = api.Node(ctx, "node-4")
	assert.Error(t, err, "node-4's token was revoked, so it is refused")
	_, after := versionOf(t, admin)
	assert.Equal(t, bootID, after, "the control-plane Node did not reboot")
}
