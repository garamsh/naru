package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// startProxyHost starts a server whose host has a LAN interface and, with proxy, a naru-proxy
// whose health is healthy.
func startProxyHost(t *testing.T, proxy bool, healthy *atomic.Bool) exposureServer {
	t.Helper()
	x := &network.Exposer{}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) {
		d.Exposures, d.LANInterface = x, "lan0"
		if proxy {
			d.ProxyHealthy = func(context.Context) bool { return healthy.Load() }
		}
	})
	link := &lanLink{addrs: map[netip.Addr]bool{}, refused: map[netip.Addr]bool{}, lanPrefixes: []netip.Prefix{netip.MustParsePrefix("192.168.0.0/24")}}
	e.nets.Base.LAN = "lan0"
	if proxy {
		e.nets.Base.Proxy = network.ProxyPorts{TLS: 8443, Redirect: 80}
	}
	x.Listen = func(string) (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }
	x.Manager, x.Link, x.Log = e.nets, link, slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, x.Restore(context.Background()))
	return exposureServer{e, link}
}

func proxyLease() hostapi.ExposureRequest {
	return hostapi.ExposureRequest{
		ToProxy:      true,
		Ports:        []hostapi.ExposurePort{{Protocol: wire.ProtocolTCP, Port: 443, BackendPort: 443}, {Protocol: wire.ProtocolTCP, Port: 80, BackendPort: 80}},
		LeaseSeconds: 15,
	}
}

func TestProxyExposure_AHostThatRunsAProxyRedirectsTheLeasedPortsToIt(t *testing.T) {
	var healthy atomic.Bool
	e := startProxyHost(t, true, &healthy)
	p := e.as(t, ProvisionerOrganization)

	code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.90", proxyLease())
	require.Equal(t, 200, code, string(b))
	var got hostapi.Exposure
	require.NoError(t, json.Unmarshal(b, &got))
	assert.True(t, got.ToProxy)
	assert.Empty(t, got.Network)
	assert.Empty(t, got.Backends)
	assert.True(t, e.link.has("192.168.0.90"), "the host holds the address")
	rules := e.nft.ruleset()
	assert.Contains(t, rules, "192.168.0.90 ip daddr @leased tcp dport 443 redirect to :8443")
	assert.Contains(t, rules, "192.168.0.90 ip daddr @leased tcp dport 80 redirect to :80")

	code, b = e.do(t, p, "GET", "/v1/exposures", nil)
	require.Equal(t, 200, code, string(b))
	var list struct{ Exposures []hostapi.Exposure }
	require.NoError(t, json.Unmarshal(b, &list))
	require.Len(t, list.Exposures, 1)
	assert.True(t, list.Exposures[0].ToProxy, "the listing tells a proxy exposure's lease")
}

func TestProxyExposure_AHostWithoutAProxyRefusesTheLease409(t *testing.T) {
	e := startProxyHost(t, false, nil)
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/exposures/192.168.0.90", proxyLease())
	assert.Equal(t, 409, code, string(b))
	assert.False(t, e.link.has("192.168.0.90"))
}

func TestProxyExposure_Refusals(t *testing.T) {
	var healthy atomic.Bool
	e := startProxyHost(t, true, &healthy)
	p := e.as(t, ProvisionerOrganization)
	for name, tc := range map[string]struct {
		change func(*hostapi.ExposureRequest)
		fields []string
	}{
		"a network":          {func(r *hostapi.ExposureRequest) { r.Network = "a" }, []string{"network"}},
		"a backend":          {func(r *hostapi.ExposureRequest) { r.Backends = []string{"10.8.0.5"} }, []string{"backends"}},
		"the PROXY protocol": {func(r *hostapi.ExposureRequest) { r.ProxyProtocol = "v2" }, []string{"proxyProtocol"}},
		"another port":       {func(r *hostapi.ExposureRequest) { r.Ports[1].Port, r.Ports[1].BackendPort = 8080, 8080 }, []string{"ports[1]"}},
		"a UDP port":         {func(r *hostapi.ExposureRequest) { r.Ports[1].Protocol = wire.ProtocolUDP }, []string{"ports[1]"}},
		"a port elsewhere":   {func(r *hostapi.ExposureRequest) { r.Ports[0].BackendPort = 30443 }, []string{"ports[0]", "ports"}},
		"a port twice":       {func(r *hostapi.ExposureRequest) { r.Ports[1] = r.Ports[0] }, []string{"ports[1]"}},
		"no 443":             {func(r *hostapi.ExposureRequest) { r.Ports = r.Ports[1:] }, []string{"ports"}},
		"a long lease":       {func(r *hostapi.ExposureRequest) { r.LeaseSeconds = 121 }, []string{"leaseSeconds"}},
	} {
		req := proxyLease()
		tc.change(&req)
		code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.90", req)
		require.Equal(t, 422, code, "%s: %s", name, b)
		var body hostapi.ErrorBody
		require.NoError(t, json.Unmarshal(b, &body))
		assert.Equal(t, tc.fields, body.Fields, "%s: the fields named are the ones that are wrong", name)
	}
	assert.False(t, e.link.has("192.168.0.90"))
	code, b := e.do(t, p, "PUT", "/v1/exposures/192.168.0.90", proxyLease())
	require.Equal(t, 200, code, "control: the lease the cases vary is taken: %s", b)
}

func TestProxyExposure_HostReportsItsProxysHealth(t *testing.T) {
	var healthy atomic.Bool
	e := startProxyHost(t, true, &healthy)
	r := e.as(t, ReaderOrganization)
	host := func() hostapi.Host {
		code, b := e.do(t, r, "GET", "/v1/host", nil)
		require.Equal(t, 200, code, string(b))
		var h hostapi.Host
		require.NoError(t, json.Unmarshal(b, &h))
		return h
	}
	require.NotNil(t, host().Proxy)
	assert.False(t, host().Proxy.Healthy, "a proxy that does not answer")
	healthy.Store(true)
	assert.True(t, host().Proxy.Healthy)

	assert.Nil(t, startProxyHost(t, false, nil).hostInfo(t).Proxy, "control: a host that runs no proxy reports none")
}

func (e exposureServer) hostInfo(t *testing.T) hostapi.Host {
	t.Helper()
	code, b := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	require.Equal(t, 200, code, string(b))
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	return h
}
