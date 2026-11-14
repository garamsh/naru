package fence_test

import (
	"net/netip"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/naru/node/internal/fence"
)

func TestAllowed_DropsDuplicatesAndRefusesWhatEtcdDoesNotListenOn(t *testing.T) {
	a := netip.MustParseAddr("10.0.0.1")
	got, err := fence.Allowed([]netip.Addr{a, a, netip.MustParseAddr("10.0.0.2")})
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{a, netip.MustParseAddr("10.0.0.2")}, got)

	_, err = fence.Allowed([]netip.Addr{netip.MustParseAddr("fe80::1")})
	assert.ErrorContains(t, err, "not an IPv4 address")
	_, err = fence.Allowed(nil)
	assert.ErrorContains(t, err, "no address")
}

// The kernel decides what a rule does; the QEMU test shows that. This checks what Apply sends it:
// one table, its input, output and forward chains, a rule and a set of allowed addresses per closed
// port, and no more.
func TestApply_SendsOneTableThreeChainsAndARuleAndASetPerClosedPort(t *testing.T) {
	count := map[uint16]int{}
	conn, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		for _, m := range req {
			count[uint16(m.Header.Type)&0xff]++
		}
		return nltest.Multipart(nil)
	}))
	require.NoError(t, err)
	require.NoError(t, fence.Apply(conn, []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("127.0.0.1")}, nil))
	assert.Equal(t, 1, count[unix.NFT_MSG_NEWTABLE])
	assert.Equal(t, 3, count[unix.NFT_MSG_NEWCHAIN])
	assert.Equal(t, len(fence.Ports), count[unix.NFT_MSG_NEWRULE], "a rule per closed port")
	assert.Equal(t, len(fence.Ports), count[unix.NFT_MSG_NEWSET], "an allowed-address set per rule")
}

// A MicroVM of this Node reaches another member masqueraded as the Node's address, which is allowed,
// and reaches this Node's own member as local delivery with its own source: the prefix is admitted
// on the client port by a rule of its own, ahead of the input rules that reject.
func TestApply_AdmitsTheMachinePrefixToTheClientPortAheadOfTheRejectRules(t *testing.T) {
	rules := applyRules(t, []netip.Prefix{netip.MustParsePrefix("10.77.3.0/24")})
	require.Len(t, rules, len(fence.Ports)+1+2, "a rule for the prefix, a rule per closed port, and the two sending-side rules")
	clientPort := string([]byte{byte(fence.ClientPort >> 8), byte(fence.ClientPort & 0xff)})
	assert.Contains(t, rules[0], string([]byte{10, 77, 3, 0}), "the first rule matches the prefix")
	assert.Contains(t, rules[0], string([]byte{255, 255, 255, 0}), "under its mask")
	assert.Contains(t, rules[0], clientPort, "on the client port")
	for _, r := range rules[3:] {
		assert.NotContains(t, r, string([]byte{10, 77, 3, 0}), "the reject rules name no prefix: a worker's address is refused")
	}
}

// A MicroVM's packet to another member leaves the Node masqueraded, so the receiver cannot tell it
// from the Node's own: the sending Node rejects a packet from the prefix to the peer port, in the
// output hook (local sources) and the forward hook (a MicroVM's packets).
func TestApply_RejectsThePrefixToThePeerPortWhereItIsSentBeforeTheMasquerade(t *testing.T) {
	rules := applyRules(t, []netip.Prefix{netip.MustParsePrefix("10.77.3.0/24")})
	require.Len(t, rules, len(fence.Ports)+1+2)
	peerPort := string([]byte{byte(fence.PeerPort >> 8), byte(fence.PeerPort & 0xff)})
	for _, r := range rules[1:3] {
		assert.Contains(t, r, string([]byte{10, 77, 3, 0}), "the sending-side rule matches the prefix")
		assert.Contains(t, r, peerPort, "on the peer port")
	}
	assert.Contains(t, rules[1], "output")
	assert.Contains(t, rules[2], "forward")
}

func applyRules(t *testing.T, machines []netip.Prefix) []string {
	t.Helper()
	var rules []string
	conn, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		for _, m := range req {
			if uint16(m.Header.Type)&0xff == unix.NFT_MSG_NEWRULE {
				rules = append(rules, string(m.Data))
			}
		}
		return nltest.Multipart(nil)
	}))
	require.NoError(t, err)
	require.NoError(t, fence.Apply(conn, []netip.Addr{netip.MustParseAddr("10.0.0.1")}, machines))
	return rules
}
