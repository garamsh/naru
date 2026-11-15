package linux

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/dhcp4"
	"github.com/garamsh/naru/lib/init/netcfg"
)

var (
	dhcpMAC  = net.HardwareAddr{0x02, 0, 0, 0, 0, 0x09}
	leaseIP  = netip.MustParseAddr("10.0.2.15")
	routerIP = netip.MustParseAddr("10.0.2.2")
)

// dhcpServer is a DHCP server in the process: it answers what the client sends, as one on the wire
// would. A server with silent set never answers.
type dhcpServer struct {
	replies chan []byte
	silent  bool
}

func newDHCPServer() *dhcpServer { return &dhcpServer{replies: make(chan []byte, 16)} }

func (s *dhcpServer) Send(_, _ netip.Addr, b []byte) error {
	m, err := dhcp4.Decode(b)
	if err != nil {
		return err
	}
	if s.silent {
		return nil
	}
	reply := dhcp4.Message{Reply: true, XID: m.XID, CHAddr: m.CHAddr, YIAddr: leaseIP, Options: map[byte][]byte{
		dhcp4.OptSubnetMask: {255, 255, 255, 0},
		dhcp4.OptRouter:     routerIP.AsSlice(),
		dhcp4.OptDNS:        {10, 0, 2, 3},
		dhcp4.OptNTP:        {10, 0, 2, 4},
		dhcp4.OptServerID:   routerIP.AsSlice(),
		dhcp4.OptLeaseTime:  binary.BigEndian.AppendUint32(nil, 1000),
	}}
	switch m.Type() {
	case dhcp4.Discover:
		reply.Options[dhcp4.OptMessageType] = []byte{byte(dhcp4.Offer)}
	case dhcp4.Request:
		reply.Options[dhcp4.OptMessageType] = []byte{byte(dhcp4.Ack)}
	}
	s.replies <- reply.Encode()
	return nil
}

func (s *dhcpServer) Close() error { return nil }

func (s *dhcpServer) Recv(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.replies:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dhcpHost is a Host whose DHCP transport is s, over links.
func dhcpHost(t *testing.T, s *dhcpServer, l *fakeLinks) *Host {
	t.Helper()
	h, _ := testHost(t, nil)
	h.links = l
	h.leaseWait = 2 * time.Second
	h.dhcpOpen = func(got link) (dhcp4.Transport, error) {
		require.Equal(t, dhcpMAC, got.MAC, "the transport is opened on the link that was chosen")
		return s, nil
	}
	return h
}

// loopback is lo as the kernel reports it: a 6-byte zero MAC and the loopback flag.
func loopback() link {
	return link{Index: 1, Name: "lo", MAC: make(net.HardwareAddr, 6), Loopback: true}
}

func ethernet() []link {
	return []link{loopback(), {Index: 2, Name: "ens18", MAC: net.HardwareAddr{0x02, 0, 0, 0, 0, 0x08}}, {Index: 3, Name: "ens19", MAC: dhcpMAC}}
}

func TestHost_ApplyNetworkLeasesAnAddressOnTheFirstLinkWithACarrier(t *testing.T) {
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)

	applied, err := h.ApplyNetwork(netcfg.Automatic(), discard)

	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{leaseIP}, applied.Addresses, "the lease's address is the Node's")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.2.3")}, applied.Nameservers)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.2.4")}, applied.NTP)
	assert.Equal(t, []string{"up lo", "up ens18", "up ens19", "up ens19", "addr 10.0.2.15/24 ens19", "route 10.0.2.2 ens19"}, l.calls)
	assert.Equal(t, "nameserver 10.0.2.3\n", read(t, h.resolvConf))
}

func TestHost_ApplyNetworkSkipsLoopbackByItsFlagWhenItHasACarrier(t *testing.T) {
	// lo has a MAC of six bytes and a carrier, as in the kernel; only its flag tells it from ens19.
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"lo": true, "ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)

	applied, err := h.ApplyNetwork(netcfg.Automatic(), discard)

	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{leaseIP}, applied.Addresses)
	assert.Equal(t, []string{"up lo", "up ens18", "up ens19", "up ens19", "addr 10.0.2.15/24 ens19", "route 10.0.2.2 ens19"}, l.calls, "lo is brought up once, for the services, and is not leased on")
}

func TestHost_ApplyNetworkLeasesOnANamedLink(t *testing.T) {
	// A DHCP interface that names a link uses it whether or not it sees a carrier.
	l := &fakeLinks{all: ethernet()}
	h := dhcpHost(t, newDHCPServer(), l)

	applied, err := h.ApplyNetwork(netcfg.Config{Interfaces: []netcfg.Interface{{Name: "ens19", DHCP: true}}}, discard)

	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{leaseIP}, applied.Addresses)
}

func TestHost_ApplyNetworkKeepsTheConfiguredResolversOverTheLeases(t *testing.T) {
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)
	cfg := netcfg.Automatic()
	cfg.Nameservers = []netip.Addr{netip.MustParseAddr("1.1.1.1")}

	applied, err := h.ApplyNetwork(cfg, discard)

	require.NoError(t, err)
	assert.Equal(t, cfg.Nameservers, applied.Nameservers)
	assert.Equal(t, "nameserver 1.1.1.1\n", read(t, h.resolvConf))
}

func TestHost_ApplyNetworkRefusesWhenNoEthernetLinkHasACarrier(t *testing.T) {
	// The control is the first test: with a carrier the same links lease.
	l := &fakeLinks{all: ethernet()}
	h := dhcpHost(t, newDHCPServer(), l)
	h.carrierWait = 50 * time.Millisecond

	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)

	assert.ErrorContains(t, err, "no Ethernet link has a carrier")
	assert.NoFileExists(t, h.resolvConf)
}

func TestHost_ApplyNetworkRefusesWhenNoServerAnswers(t *testing.T) {
	s := newDHCPServer()
	s.silent = true
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, s, l)
	h.leaseWait = 100 * time.Millisecond

	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)

	assert.ErrorContains(t, err, "no DHCPv4 lease on ens19")
	assert.NotContains(t, l.calls, "addr 10.0.2.15/24 ens19", "nothing is configured without a lease")
}

func TestHost_RunNetworkReturnsAtOnceWhenTheNetworkIsStatic(t *testing.T) {
	h, _ := testHost(t, nil)

	assert.NoError(t, h.RunNetwork(t.Context()))
}

func TestHost_RunNetworkKeepsTheLeaseUntilTheContextEnds(t *testing.T) {
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)
	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() { done <- h.RunNetwork(ctx) }()

	select {
	case <-done:
		t.Fatal("RunNetwork returned while the lease was held")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("RunNetwork did not return after the context ended")
	}
}

func TestLease_ANewLeaseReplacesTheAddressTheRouteAndTheResolvers(t *testing.T) {
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)
	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)
	require.NoError(t, err)
	l.calls = nil
	moved := dhcp4.Lease{
		Address: netip.MustParsePrefix("10.0.2.99/24"), Router: netip.MustParseAddr("10.0.2.1"),
		DNS: []netip.Addr{netip.MustParseAddr("9.9.9.9")}, Duration: time.Hour,
	}

	h.leases.Bound(moved)

	assert.Equal(t, []string{"del-route 10.0.2.2 ens19", "del-addr 10.0.2.15/24 ens19", "addr 10.0.2.99/24 ens19", "route 10.0.2.1 ens19"}, l.calls)
	assert.Equal(t, "nameserver 9.9.9.9\n", read(t, h.resolvConf))
	entries, err := os.ReadDir(filepath.Dir(h.resolvConf))
	require.NoError(t, err)
	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".naru-file-", "the replacement leaves no temporary file")
	}
}

func TestLease_ARenewedLeaseKeepsTheAddress(t *testing.T) {
	// The control: the same address is not removed and added again.
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)
	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)
	require.NoError(t, err)
	l.calls = nil

	h.leases.Bound(h.leases.lease)

	assert.NotContains(t, l.calls, "del-addr 10.0.2.15/24 ens19")
}

func TestLease_AnExpiredLeaseRemovesTheAddressAndTheRoute(t *testing.T) {
	l := &fakeLinks{all: ethernet(), carrier: map[string]bool{"ens19": true}}
	h := dhcpHost(t, newDHCPServer(), l)
	_, err := h.ApplyNetwork(netcfg.Automatic(), discard)
	require.NoError(t, err)
	l.calls = nil

	h.leases.Expired(h.leases.lease)

	assert.Equal(t, []string{"del-route 10.0.2.2 ens19", "del-addr 10.0.2.15/24 ens19"}, l.calls)
}
