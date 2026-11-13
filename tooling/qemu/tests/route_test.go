//go:build qemu_real

package tests_test

import (
	"context"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// netecho is the service of the data disk that sends, receives and reports routes (naru64).
var netecho = path.Join(fixture.GuestRoot, "bin/naru-netecho")

// routedPair is two Nodes whose Machine prefixes, 10.77.1.0/24 and 10.77.2.0/24, are each routed
// to from the other. node-1 sends datagrams from 10.77.1.2, an address of its own prefix, as a
// Machine does, to three destinations that node-2 receives on:
//
//	10.77.2.2  node-2's prefix: routed, and in node-1's no_masquerade
//	10.99.0.2  routed, and not in no_masquerade: the control for no_masquerade
//	10.98.0.2  in no_masquerade, and not routed: the control for the route
func routedPair(c *node.Config) {
	sock := []string{"-alias", "-after", node.MvmdSocket}
	switch c.Index {
	case 1:
		c.Routes = []node.Route{{To: "10.77.2.0/24", Via: "10.78.0.2"}, {To: "10.99.0.0/24", Via: "10.78.0.2"}}
		c.NoMasquerade = []string{"10.77.2.0/24", "10.98.0.0/24"}
		c.NetEcho = append(sock, "-send", "10.77.1.2=10.77.2.2:9000", "-send", "10.77.1.2=10.99.0.2:9000", "-send", "10.77.1.2=10.98.0.2:9000")
	case 2:
		c.Routes = []node.Route{{To: "10.77.1.0/24", Via: "10.78.0.1"}}
		c.NetEcho = append(sock, "-listen", "10.77.2.2:9000", "-listen", "10.99.0.2:9000", "-listen", "10.98.0.2:9000")
	}
}

// TestNode_ATrafficFromAMachinePrefixKeepsItsSourceAddressToAnotherNodesPrefixAndIsMasqueradedElsewhere
// boots two Nodes with spec.network.routes and mvmd's network.no_masquerade, and reads, on the
// receiving Node, the source address each datagram arrives with.
func TestNode_ATrafficFromAMachinePrefixKeepsItsSourceAddressToAnotherNodesPrefixAndIsMasqueradedElsewhere(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 2, Configure: routedPair})
	receiver := c.Nodes[1]
	ctx := context.Background()

	// The route and no_masquerade: the datagram arrives, and its source is the Machine's.
	require.NoError(t, receiver.WaitFor(ctx, bootTimeout, "received", "from=10.77.1.2", "to=10.77.2.2"), "routed and left unmasqueraded")
	// The control for no_masquerade: the same sender, routed too, but masqueraded to the Node's address.
	require.NoError(t, receiver.WaitFor(ctx, bootTimeout, "received", "from=10.78.0.1", "to=10.99.0.2"), "routed and masqueraded")
	assert.False(t, hasReceived(receiver, "from=10.77.1.2", "to=10.99.0.2"), "a destination outside no_masquerade never sees the Machine's address")
	// The control for the route: left unmasqueraded, but with no route nothing arrives. The sends
	// repeat every 300 ms, so the window holds about ten rounds of each.
	time.Sleep(3 * time.Second)
	assert.False(t, hasReceived(receiver, "to=10.98.0.2"), "a destination with no route is not delivered")
}

func hasReceived(n *node.Node, fragments ...string) bool {
	return n.Has(append([]string{"received"}, fragments...)...)
}

// netechoRoutes is a patch that runs netecho as a route watcher.
var netechoRoutes = "spec:\n  services:\n    - name: naru64-routes\n      binary: " + netecho + "\n      args: [\"-routes\"]\n"

func routePatch(via string) string {
	return "spec:\n  network:\n    routes:\n      - to: 10.77.2.0/24\n        via: " + via + "\n"
}

const routeDelete = "spec:\n  network:\n    routes:\n      - to: 10.77.2.0/24\n        $patch: delete\n"

func TestNode_ARouteAppliedWithoutARebootIsInTheKernelsTableChangesItsNextHopAndGoesWhenRemoved(t *testing.T) {
	c := nodeAPI(t)
	admin, n := c.API.Admin, c.Nodes[0]
	ctx := context.Background()
	_, bootID := versionOf(t, admin)
	watcher := ctl(admin, "apply", "-f", patchFile(t, netechoRoutes), "--mode", "no-reboot")
	require.NoError(t, watcher.Err, watcher.refusal())
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "running", "service=naru-netecho"), "the route watcher runs")
	assert.False(t, n.Has("route added", "10.77.2.0/24"), "the control: before the apply, the kernel has no such route")

	added := ctl(admin, "apply", "-f", patchFile(t, routePatch("10.78.0.2")), "--mode", "no-reboot")

	require.NoError(t, added.Err, added.refusal())
	assert.Contains(t, added.Stdout, "add-route")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "route added", "10.77.2.0/24 via 10.78.0.2"), "the route is in the kernel's table")
	_, after := versionOf(t, admin)
	assert.Equal(t, bootID, after, "no reboot")

	changed := ctl(admin, "apply", "-f", patchFile(t, routePatch("10.78.0.3")), "--mode", "no-reboot")

	require.NoError(t, changed.Err, changed.refusal())
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "route added", "10.77.2.0/24 via 10.78.0.3"), "the next hop is the new one")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "route removed", "10.77.2.0/24 via 10.78.0.2"), "and the old one is gone")

	removed := ctl(admin, "apply", "-f", patchFile(t, routeDelete), "--mode", "no-reboot")

	require.NoError(t, removed.Err, removed.refusal())
	assert.Contains(t, removed.Stdout, "delete-route")
	require.NoError(t, n.WaitFor(ctx, bootTimeout, "route removed", "10.77.2.0/24 via 10.78.0.3"), "the removed route leaves the table")
	_, final := versionOf(t, admin)
	assert.Equal(t, bootID, final, "still no reboot")
}
