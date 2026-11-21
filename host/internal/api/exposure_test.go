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
	mu    sync.Mutex
	addrs map[netip.Addr]bool
}

func (l *lanLink) Add(a netip.Addr) error {
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
	link := &lanLink{addrs: map[netip.Addr]bool{}}
	e.nets.Base.LAN = "lan0"
	x.Listen = func(string) (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }
	x.Manager, x.Link, x.Log = e.nets, link, slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, x.Restore(context.Background()))
	p := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.8.0.0/16", RemoteMembers: []string{"10.0.2.7/32"}})
	require.Equal(t, 200, code, string(b))
	return exposureServer{e, link}
}

func goodExposure() hostapi.ExposureRequest {
	return hostapi.ExposureRequest{
		Network:      "a",
		Ports:        []hostapi.ExposurePort{{Protocol: "tcp", Port: 443, NodePort: 30443}, {Protocol: "udp", Port: 53, NodePort: 30053}},
		Backends:     []string{"10.8.0.5", "10.0.2.7"},
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
	assert.Contains(t, e.nft.ruleset(), "192.168.0.80 tcp dport 443 dnat ip to numgen random mod 2 map { 0 : 10.8.0.5 . 30443, 1 : 10.0.2.7 . 30443 }")

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
	e := startExposures(t)
	p := e.as(t, ProvisionerOrganization)
	with := func(f func(*hostapi.ExposureRequest)) hostapi.ExposureRequest {
		r := goodExposure()
		f(&r)
		return r
	}
	for name, tc := range map[string]struct {
		path string
		body hostapi.ExposureRequest
		want int
	}{
		"address is not an address":       {"/v1/exposures/lan", goodExposure(), 422},
		"address is IPv6":                 {"/v1/exposures/fd00::80", goodExposure(), 422},
		"address is the loopback":         {"/v1/exposures/127.0.0.1", goodExposure(), 422},
		"address is in the network":       {"/v1/exposures/10.8.1.1", goodExposure(), 422},
		"address is the management":       {"/v1/exposures/172.16.0.5", goodExposure(), 422},
		"unknown network":                 {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Network = "nope" }), 422},
		"bad network name":                {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Network = "A_" }), 422},
		"no ports":                        {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Ports = nil }), 422},
		"port 0":                          {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Ports[0].Port = 0 }), 422},
		"node port too big":               {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Ports[0].NodePort = 70000 }), 422},
		"protocol sctp":                   {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Ports[0].Protocol = "sctp" }), 422},
		"the same port twice":             {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Ports[1] = r.Ports[0] }), 422},
		"no backends":                     {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Backends = nil }), 422},
		"backend outside the network":     {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.9.0.5"} }), 422},
		"backend is not an address":       {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Backends = []string{"node"} }), 422},
		"backend twice":                   {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.8.0.5", "10.8.0.5"} }), 422},
		"lease of zero":                   {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.LeaseSeconds = 0 }), 422},
		"lease of an hour":                {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.LeaseSeconds = 3600 }), 422},
		"proxy protocol v1":               {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.ProxyProtocol = "v1" }), 422},
		"proxy protocol v2 with UDP port": {"/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) { r.ProxyProtocol = "v2" }), 422},
	} {
		code, b := e.do(t, p, "PUT", tc.path, tc.body)
		assert.Equal(t, tc.want, code, "%s: %s", name, b)
	}
	code, _ := e.do(t, p, "GET", "/v1/exposures", nil)
	assert.Equal(t, 200, code)
	assert.Empty(t, e.nets.Exposures(), "a refused lease holds nothing")
	assert.False(t, e.link.has("192.168.0.80"))
	assert.False(t, e.link.has("10.8.1.1"))

	code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.80", with(func(r *hostapi.ExposureRequest) {
		r.ProxyProtocol = "v2"
		r.Ports = r.Ports[:1]
	}))
	assert.Equal(t, 200, code, "control: the same with only TCP is taken: %s", b)
}

func TestExposure_AMemberAddressIsRefused(t *testing.T) {
	e := startExposures(t)
	p := e.as(t, ProvisionerOrganization)
	require.NoError(t, e.nets.Attach(context.Background(), network.Member{Network: "a", Address: netip.MustParseAddr("10.0.0.3")}))
	code, b := e.do(t, p, "PUT", "/v1/exposures/10.0.0.3", goodExposure())
	assert.Equal(t, 422, code, "a microVM's address is not announced on the LAN: %s", b)
	assert.False(t, e.link.has("10.0.0.3"))
	code, b = e.do(t, p, "PUT", "/v1/exposures/10.0.0.4", goodExposure())
	assert.Equal(t, 200, code, "control: another address is: %s", b)
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
