//go:build qemu_real

package tests_test

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/hostapi"
	"github.com/garamsh/naru/tooling/qemu/internal/node"
)

// The test of the control plane's HA (ADR 0024) boots three Nodes that are all control-plane Nodes:
// each runs etcd as a member of one static cluster, the API server, and the holder of the virtual
// address. node-3 also runs naru-etcdprobe, which prints on its console what a client sees of the
// control plane from a member's address and from an address no member has.

const (
	holding = "naru-vip: holding the address"
	vipLine = `naru59-probe: vip=` + node.VirtualAddress + ` apiserver=(connected|answered-\d+)`
)

// probeLine is a line naru-etcdprobe printed to the console of the Node it runs on.
func probeLine(etcd, source, cert, result string) string {
	return "naru59-probe: etcd=" + etcd + " source=" + source + " client-cert=" + cert + " result=" + result
}

func TestControlPlane_ThreeMembersListTheirClusterAndAfterTheHolderStopsTheVirtualAddressMovesAndTheOthersKeepServing(t *testing.T) {
	c := start(t, node.ClusterSpec{N: 3, Gaon: true, BootPKI: true, ControlPlanes: 3, EtcdProbeOn: 3})
	ctx := context.Background()
	first, second, third := c.Nodes[0], c.Nodes[1], c.Nodes[2]
	apis := map[string]*hostapi.Client{}
	for _, nd := range c.Nodes {
		api, err := hostapi.New(c.HostAddrs[nd.Name], c.Client)
		require.NoError(t, err)
		apis[nd.Name] = api
	}

	for _, nd := range c.Nodes {
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=vip"), nd.Name)
		require.NoError(t, nd.WaitFor(ctx, bootTimeout, "started", "service=gaon-node-agent"), nd.Name)
	}
	for _, nd := range c.Nodes {
		eventually(t, nd.Name+" is registered and Ready", func() bool {
			n, err := apis["node-1"].Node(ctx, nd.Name)
			return err == nil && n.Ready()
		})
	}

	// The first member in priority order, node-1, holds the address; the others do not.
	require.NoError(t, first.WaitFor(ctx, bootTimeout, holding))
	assert.NotContains(t, second.Console(), holding)
	assert.NotContains(t, third.Console(), holding)

	// etcd of each member lists three members, from each member's address.
	members := regexp.MustCompile(`naru59-probe: members from=(\S+) names=(\S+)`)
	eventually(t, "every member's etcd lists the three members", func() bool {
		seen := map[string]string{}
		for _, m := range members.FindAllStringSubmatch(third.Console(), -1) {
			names := strings.Split(m[2], ",")
			sort.Strings(names)
			seen[m[1]] = strings.Join(names, ",")
		}
		return len(seen) == 3 && seen["10.78.0.1"] == "node-1,node-2,node-3" && seen["10.78.0.2"] == "node-1,node-2,node-3" && seen["10.78.0.3"] == "node-1,node-2,node-3"
	})

	// etcd does not dial its own client port with its serving pair, whose certificate has no clientAuth.
	for _, nd := range c.Nodes {
		assert.Contains(t, nd.Console(), "serving client traffic securely", nd.Name+" logs etcd")
		assert.NotContains(t, nd.Console(), "incompatible key usage", nd.Name)
	}

	// etcd's client port: a member with a certificate is served; a client without one is refused at the
	// TLS handshake; an address that is no member's is refused, by a reset, though it has the certificate.
	for _, addr := range []string{"10.78.0.1", "10.78.0.2", "10.78.0.3"} {
		require.NoError(t, third.WaitFor(ctx, bootTimeout, probeLine(addr, "member", "yes", "connected")), "a member with a certificate is served by "+addr)
		require.NoError(t, third.WaitFor(ctx, bootTimeout, probeLine(addr, "member", "no", "refused-at-tls-handshake")), "a client without a certificate is refused by "+addr)
		require.NoError(t, third.WaitFor(ctx, bootTimeout, probeLine(addr, "alias", "yes", "refused-by-tcp-reset")), "an address that is no member's is refused by "+addr)
	}
	// The pair `naructl gen etcd-client` wrote is accepted by every member on 2379, from node-3's own
	// address and from an address in node-3's MicroVM prefix. The peer port is refused by a reset to
	// that prefix at every member: node-3's masquerade would hide the prefix from the receiving
	// member's fence, so node-3 refuses it before the masquerade.
	for _, addr := range []string{"10.78.0.1", "10.78.0.2", "10.78.0.3"} {
		require.NoError(t, third.WaitFor(ctx, bootTimeout, probeLine(addr, "member", "snapshot", "connected")), "the etcd client pair is accepted by "+addr)
		require.NoError(t, third.WaitFor(ctx, bootTimeout, probeLine(addr, "machine", "snapshot", "connected")), "a MicroVM's address is served by "+addr)
		require.NoError(t, third.WaitFor(ctx, bootTimeout, "naru59-probe: etcd-peer="+addr+" source=machine client-cert=snapshot result=refused-by-tcp-reset"), "the peer port of "+addr)
	}
	assert.NotContains(t, third.Console(), "source=alias client-cert=yes result=connected", "no non-member connected")
	assert.NotContains(t, third.Console(), "source=member client-cert=no result=connected", "no client without a certificate connected")

	// The API server answers at the virtual address, with a certificate that names it.
	require.NoError(t, third.WaitFor(ctx, bootTimeout, "naru59-probe: vip="+node.VirtualAddress+" apiserver="))
	eventually(t, "the probe reaches the API server at the virtual address", func() bool { return regexp.MustCompile(vipLine).MatchString(third.Console()) })

	require.NoError(t, apis["node-1"].CreateNode(ctx, "marker"))
	_, err := apis["node-3"].Node(ctx, "marker")
	require.NoError(t, err, "a write through node-1's API server is read through node-3's: one etcd")
	// The holder stops, as a Node that loses power does.
	mark := len(third.Console())
	first.Stop()
	require.NoError(t, second.WaitFor(ctx, bootTimeout, holding), "node-2, the next live member, takes the address")
	assert.NotContains(t, third.Console(), holding, "node-3 does not")
	eventually(t, "the API server answers at the virtual address again", func() bool {
		return regexp.MustCompile(vipLine).MatchString(third.Console()[mark:])
	})

	// The other two keep serving: etcd has two of its three members, so it has its quorum.
	for _, name := range []string{"node-2", "node-3"} {
		eventually(t, name+"'s API server still serves the marker", func() bool {
			_, err := apis[name].Node(ctx, "marker")
			return err == nil
		})
	}
	eventually(t, "node-2's API server accepts a write: etcd has elected a leader among its two", func() bool {
		_ = apis["node-2"].CreateNode(ctx, "after") // a write that timed out may have been applied: read it back
		_, err := apis["node-2"].Node(ctx, "after")
		return err == nil
	})
	eventually(t, "a write through node-2 is read through node-3", func() bool {
		_, err := apis["node-3"].Node(ctx, "after")
		return err == nil
	})
	// The agents dial the virtual address only, and node-1 is gone: a Lease renewed now went through
	// the API server of the Node that holds it.
	for _, name := range []string{"node-2", "node-3"} {
		var now time.Time
		eventually(t, name+" has a Lease", func() bool {
			l, err := apis["node-2"].Lease(ctx, name)
			now = l.Spec.RenewTime
			return err == nil && !now.IsZero()
		})
		eventually(t, name+"'s Lease is renewed through the virtual address", func() bool {
			l, err := apis["node-2"].Lease(ctx, name)
			return err == nil && l.Spec.RenewTime.After(now)
		})
	}
}
