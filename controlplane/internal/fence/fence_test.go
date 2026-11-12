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

	"github.com/garamsh/naru/controlplane/internal/fence"
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
// one table and chain, a rule and a set of allowed addresses per closed port, and no more.
func TestApply_SendsOneTableOneChainAndARuleAndASetPerClosedPort(t *testing.T) {
	count := map[uint16]int{}
	conn, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		for _, m := range req {
			count[uint16(m.Header.Type)&0xff]++
		}
		return nltest.Multipart(nil)
	}))
	require.NoError(t, err)
	require.NoError(t, fence.Apply(conn, []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("127.0.0.1")}))
	assert.Equal(t, 1, count[unix.NFT_MSG_NEWTABLE])
	assert.Equal(t, 1, count[unix.NFT_MSG_NEWCHAIN])
	assert.Equal(t, len(fence.Ports), count[unix.NFT_MSG_NEWRULE], "a rule per closed port")
	assert.Equal(t, len(fence.Ports), count[unix.NFT_MSG_NEWSET], "an allowed-address set per rule")
}
