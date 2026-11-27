package network_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/wire"
)

// toProxy is a proxy exposure's lease on 192.168.0.90 for 443 and 80.
func toProxy() network.Exposure {
	return network.Exposure{
		Address: addr("192.168.0.90"), ToProxy: true,
		Ports: []network.ExposurePort{{Protocol: wire.ProtocolTCP, Port: 443, BackendPort: 443}, {Protocol: wire.ProtocolTCP, Port: 80, BackendPort: 80}},
	}
}

func TestRuleset_AProxyExposureRedirectsItsPortsToTheHostsProxyAndForwardsNothing(t *testing.T) {
	c := twoNetworks()
	c.LAN = "lan0"
	c.Proxy = network.ProxyPorts{TLS: 8443, Redirect: 80}
	e := toProxy()
	e.Deadline = c.At.Add(15 * time.Second)
	c.Exposures = []network.Exposure{e}
	rules := network.Ruleset(c)

	assert.Contains(t, rules, `iifname "lan0" ip daddr 192.168.0.90 ip daddr @leased tcp dport 443 redirect to :8443`, "443 to the proxy's TLS listener")
	assert.Contains(t, rules, `iifname "lan0" ip daddr 192.168.0.90 ip daddr @leased tcp dport 80 redirect to :80`, "80 to its redirect listener")
	assert.Contains(t, rules, "192.168.0.90 timeout 15s", "the redirect ends with the lease even when hostd has stopped")
	assert.NotContains(t, rules, "exposure_snat", "a flow to the host's own proxy is not forwarded, so it is not source-NATed")
	assert.NotContains(t, rules, "dnat ip to", "and goes to no backend")

	web := web(false)
	web.Deadline = e.Deadline
	c.Exposures = append(c.Exposures, web)
	rules = network.Ruleset(c)
	assert.Contains(t, rules, "exposure_snat", "control: a network's exposure beside it is forwarded and source-NATed")
	assert.Contains(t, rules, "redirect to :8443")
}

func TestManager_TakesAProxyExposureOnlyOnAHostThatRunsAProxy(t *testing.T) {
	e := startExposer(t, "", newLink(), &clock{t: t0})
	x := toProxy()
	x.Deadline = t0.Add(time.Minute)
	assert.ErrorIs(t, e.m.PutExposure(context.Background(), x), network.ErrNoProxy)

	e.m.Base.Proxy = network.ProxyPorts{TLS: 443, Redirect: 80}
	require.NoError(t, e.m.PutExposure(context.Background(), x), "control: a host that runs a proxy")
	held := e.m.Exposures()
	require.Len(t, held, 1)
	assert.True(t, held[0].ToProxy)

	reserved := toProxy()
	reserved.Address, reserved.Deadline = addr("10.8.0.5"), x.Deadline
	assert.ErrorIs(t, e.m.PutExposure(context.Background(), reserved), network.ErrReserved, "a member's address")
}

func TestManager_RefusesAProxyExposureThatIsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*network.Exposure)
	}{
		{"a network", func(x *network.Exposure) { x.Network = "a" }},
		{"a backend", func(x *network.Exposure) { x.Backends = []netip.Addr{addr("10.8.0.5")} }},
		{"the PROXY protocol", func(x *network.Exposure) { x.Proxy = true }},
		{"another port", func(x *network.Exposure) { x.Ports[1].Port, x.Ports[1].BackendPort = 8080, 8080 }},
		{"a UDP port", func(x *network.Exposure) { x.Ports[1].Protocol = wire.ProtocolUDP }},
		{"a backend port of its own", func(x *network.Exposure) { x.Ports[0].BackendPort = 8443 }},
		{"a port twice", func(x *network.Exposure) { x.Ports[1] = x.Ports[0] }},
		{"no 443", func(x *network.Exposure) { x.Ports = x.Ports[1:] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := startExposer(t, "", newLink(), &clock{t: t0})
			e.m.Base.Proxy = network.ProxyPorts{TLS: 443, Redirect: 80}
			x := toProxy()
			tc.change(&x)
			x.Deadline = t0.Add(time.Minute)
			assert.ErrorIs(t, e.m.PutExposure(context.Background(), x), network.ErrInvalid)
		})
	}
}
