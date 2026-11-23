package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
)

// lanLink is the LAN interface of the tests.
type lanLink struct {
	mu          sync.Mutex
	addrs       map[netip.Addr]bool
	refused     map[netip.Addr]bool
	lanPrefixes []netip.Prefix
}

func (l *lanLink) Check(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refused[a] {
		return network.ErrHostAddress
	}
	if len(l.lanPrefixes) > 0 {
		for _, p := range l.lanPrefixes {
			if p.Contains(a) {
				return nil
			}
		}
		return network.ErrOutsideLAN
	}
	return nil
}

func (l *lanLink) Add(a netip.Addr, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addrs[a] = true
	return nil
}

func (l *lanLink) Remove(a netip.Addr) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.addrs, a)
	return nil
}

func (l *lanLink) Probe(netip.Addr) error      { return nil }
func (l *lanLink) Held() ([]netip.Addr, error) { return nil, nil }
func (l *lanLink) Announce(netip.Addr) error   { return nil }

func (l *lanLink) has(a string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.addrs[netip.MustParseAddr(a)]
}

type exposureServer struct {
	*env
	link *lanLink
}

// startExposures starts a server whose host has a LAN interface and the network "a".
func startExposures(t *testing.T) exposureServer {
	t.Helper()
	x := &network.Exposer{}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) { d.Exposures, d.LANInterface = x, "lan0" })
	link := &lanLink{addrs: map[netip.Addr]bool{}, refused: map[netip.Addr]bool{}, lanPrefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/24")}}
	e.nets.Base.LAN = "lan0"
	x.Listen = func(string) (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }
	x.Manager, x.Link, x.Log = e.nets, link, slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, x.Restore(context.Background()))
	p := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.8.0.0/16", RemoteMembers: []string{"10.0.2.7/32"}})
	require.Equal(t, 200, code, string(b))
	for _, a := range []string{"10.8.0.5", "10.8.0.6"} {
		require.NoError(t, e.nets.Attach(context.Background(), network.Member{Network: "a", Address: netip.MustParseAddr(a)}))
	}
	return exposureServer{e, link}
}

func goodExposure() hostapi.ExposureRequest {
	return hostapi.ExposureRequest{
		Network:      "a",
		Ports:        []hostapi.ExposurePort{{Protocol: "tcp", Port: 443, NodePort: 30443}, {Protocol: "udp", Port: 53, NodePort: 30053}},
		Backends:     []string{"10.8.0.5", "10.8.0.6"},
		LeaseSeconds: 15,
	}
}

func TestExposure_PutListDelete(t *testing.T) {
	e := startExposures(t)
	p := e.as(t, ProvisionerOrganization)
	r := e.as(t, ReaderOrganization)

	before := time.Now()
	code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.80", goodExposure())
	require.Equal(t, 200, code, string(b))
	var got hostapi.Exposure
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, "192.168.0.80", got.Address)
	want := goodExposure()
	want.LeaseSeconds = got.LeaseSeconds
	assert.Equal(t, want, got.ExposureRequest)
	assert.InDelta(t, 15, got.LeaseSeconds, 2, "leaseSeconds is what is left of the lease")
	at, err := time.Parse(time.RFC3339, got.ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(15*time.Second), at, 3*time.Second, "the lease ends leaseSeconds after the request")
	assert.True(t, e.link.has("192.168.0.80"))
	assert.Contains(t, e.nft.ruleset(), "192.168.0.80 ip daddr @leased tcp dport 443 dnat ip to numgen random mod 2 map { 0 : 10.8.0.5 . 30443, 1 : 10.8.0.6 . 30443 }")

	code, b = e.do(t, r, "GET", "/v1/exposures", nil)
	require.Equal(t, 200, code, string(b))
	var list struct{ Exposures []hostapi.Exposure }
	require.NoError(t, json.Unmarshal(b, &list))
	require.Len(t, list.Exposures, 1)
	assert.Equal(t, "192.168.0.80", list.Exposures[0].Address)

	code, _ = e.do(t, p, "DELETE", "/v1/exposures/192.168.0.80", nil)
	assert.Equal(t, 204, code)
	assert.False(t, e.link.has("192.168.0.80"))
	assert.NotContains(t, e.nft.ruleset(), "dnat ip to")
	code, _ = e.do(t, p, "DELETE", "/v1/exposures/192.168.0.80", nil)
	assert.Equal(t, 204, code, "an absent lease is 204")
}

func TestExposure_HostReportsItsLANInterface(t *testing.T) {
	e := startExposures(t)
	code, b := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	require.Equal(t, 200, code, string(b))
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, "lan0", h.LANInterface)
}

func TestExposure_Refusals(t *testing.T) {
	const ok = "/v1/exposures/192.168.0.80"
	change := func(f func(*hostapi.ExposureRequest)) func() hostapi.ExposureRequest {
		return func() hostapi.ExposureRequest {
			r := goodExposure()
			f(&r)
			return r
		}
	}
	for _, tc := range []struct {
		name string
		path string
		body func() hostapi.ExposureRequest
	}{
		{"address is not an address", "/v1/exposures/lan", goodExposure},
		{"address is IPv6", "/v1/exposures/fd00::80", goodExposure},
		{"address is the loopback", "/v1/exposures/127.0.0.1", goodExposure},
		{"address is in the network", "/v1/exposures/10.8.1.1", goodExposure},
		{"address is outside the LAN prefix", "/v1/exposures/172.16.0.5", goodExposure},
		{"unknown network", ok, change(func(r *hostapi.ExposureRequest) { r.Network = "nope" })},
		{"bad network name", ok, change(func(r *hostapi.ExposureRequest) { r.Network = "A_" })},
		{"no ports", ok, change(func(r *hostapi.ExposureRequest) { r.Ports = nil })},
		{"port 0", ok, change(func(r *hostapi.ExposureRequest) { r.Ports[0].Port = 0 })},
		{"node port above the range", ok, change(func(r *hostapi.ExposureRequest) { r.Ports[0].NodePort = 32768 })},
		{"node port below the range", ok, change(func(r *hostapi.ExposureRequest) { r.Ports[0].NodePort = 29999 })},
		{"protocol sctp", ok, change(func(r *hostapi.ExposureRequest) { r.Ports[0].Protocol = "sctp" })},
		{"the same port twice", ok, change(func(r *hostapi.ExposureRequest) { r.Ports[1] = r.Ports[0] })},
		{"no backends", ok, change(func(r *hostapi.ExposureRequest) { r.Backends = nil })},
		{"backend on another host", ok, change(func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.0.2.7"} })},
		{"backend outside the network", ok, change(func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.9.0.5"} })},
		{"backend is not an address", ok, change(func(r *hostapi.ExposureRequest) { r.Backends = []string{"node"} })},
		{"backend twice", ok, change(func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.8.0.5", "10.8.0.5"} })},
		{"negative lease", ok, change(func(r *hostapi.ExposureRequest) { r.LeaseSeconds = -1 })},
		{"lease of an hour", ok, change(func(r *hostapi.ExposureRequest) { r.LeaseSeconds = 3600 })},
		{"proxy protocol v1", ok, change(func(r *hostapi.ExposureRequest) { r.ProxyProtocol = "v1" })},
		{"proxy protocol v2 with a UDP port", ok, change(func(r *hostapi.ExposureRequest) { r.ProxyProtocol = "v2" })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := startExposures(t)
			p := e.as(t, ProvisionerOrganization)
			code, b := e.do(t, p, "PUT", tc.path, tc.body())
			assert.Equal(t, 422, code, string(b))
			assert.Empty(t, e.nets.Exposures(), "a refused lease holds nothing")
			assert.False(t, e.link.has("192.168.0.80"))
			assert.False(t, e.link.has("10.8.1.1"))
		})
	}

	t.Run("controls", func(t *testing.T) {
		for name, body := range map[string]func() hostapi.ExposureRequest{
			"the base request": goodExposure,
			"proxy v2 with only TCP": change(func(r *hostapi.ExposureRequest) {
				r.ProxyProtocol = "v2"
				r.Ports = r.Ports[:1]
			}),
			"the lowest node port":  change(func(r *hostapi.ExposureRequest) { r.Ports[0].NodePort = 30000 }),
			"the highest node port": change(func(r *hostapi.ExposureRequest) { r.Ports[0].NodePort = 32767 }),
		} {
			e := startExposures(t)
			code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", ok, body())
			assert.Equal(t, 200, code, "%s: %s", name, b)
		}
	})
}

func TestExposure_ALeaseNamingNoSecondsIsFifteen(t *testing.T) {
	e := startExposures(t)
	req := goodExposure()
	req.LeaseSeconds = 0
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/exposures/192.168.0.80", req)
	require.Equal(t, 200, code, string(b))
	var got hostapi.Exposure
	require.NoError(t, json.Unmarshal(b, &got))
	assert.InDelta(t, 15, got.LeaseSeconds, 2)
}

func TestExposure_AnAddressTheHostHoldsIsRefused(t *testing.T) {
	e := startExposures(t)
	p := e.as(t, ProvisionerOrganization)
	e.link.refused[netip.MustParseAddr("192.168.0.80")] = true
	code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.80", goodExposure())
	assert.Equal(t, 422, code, string(b))
	assert.Empty(t, e.nets.Exposures())
	code, b = e.do(t, p, "PUT", "/v1/exposures/192.168.0.81", goodExposure())
	assert.Equal(t, 200, code, "control: another address is taken: %s", b)
}

func TestExposure_AMemberAddressIsRefused(t *testing.T) {
	e := startExposures(t)
	p := e.as(t, ProvisionerOrganization)
	require.NoError(t, e.nets.Attach(context.Background(), network.Member{Network: "a", Address: netip.MustParseAddr("10.0.0.3")}))
	code, b := e.do(t, p, "PUT", "/v1/exposures/10.0.0.3", goodExposure())
	assert.Equal(t, 422, code, "a microVM's address is not announced on the LAN: %s", b)
	assert.False(t, e.link.has("10.0.0.3"))
	code, b = e.do(t, p, "PUT", "/v1/exposures/192.168.0.84", goodExposure())
	assert.Equal(t, 200, code, "control: an address in the LAN prefix that is not a member's is taken: %s", b)
}

func TestExposure_Roles(t *testing.T) {
	e := startExposures(t)
	r := e.as(t, ReaderOrganization)
	code, _ := e.do(t, r, "PUT", "/v1/exposures/192.168.0.80", goodExposure())
	assert.Equal(t, 403, code)
	code, _ = e.do(t, r, "DELETE", "/v1/exposures/192.168.0.80", nil)
	assert.Equal(t, 403, code)
	assert.False(t, e.link.has("192.168.0.80"), "a reader took nothing")
	code, _ = e.do(t, r, "GET", "/v1/exposures", nil)
	assert.Equal(t, 200, code, "control: a reader lists")
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/exposures/192.168.0.80", goodExposure())
	assert.Equal(t, 200, code, "control: a provisioner takes: %s", b)
}

func TestExposure_AHostWithoutALANInterface(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	for _, c := range [][2]string{{"PUT", "/v1/exposures/192.168.0.80"}, {"GET", "/v1/exposures"}, {"DELETE", "/v1/exposures/192.168.0.80"}} {
		code, _ := e.do(t, p, c[0], c[1], goodExposure())
		assert.Equal(t, 501, code, c[0]+" "+c[1])
	}
}
