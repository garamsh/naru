package layout_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/layout"
)

func proxyHost(t *testing.T) layout.Node {
	t.Helper()
	n, ca := hostNode(t)
	zone, err := ca.Server("*.m.example", net.IPv4(192, 168, 0, 40))
	require.NoError(t, err)
	n.Proxy = &layout.Proxy{Certificate: string(zone.CertPEM), Key: string(zone.KeyPEM), RouteMaxAge: "30m"}
	return n
}

func TestUserData_AProxyHostRunsNaruProxyWithTheZonesKeyForItsUserAlone(t *testing.T) {
	n := proxyHost(t)
	require.NoError(t, n.Check())
	data, err := n.UserData()
	require.NoError(t, err)
	cfg, err := config.Parse(data)
	require.NoError(t, err, "init takes the section layout writes")

	var proxy *config.Service
	for i, s := range cfg.Spec.Services {
		if s.Name == config.ProxyService {
			proxy = &cfg.Spec.Services[i]
		}
	}
	require.NotNil(t, proxy, "init runs naru-proxy")
	assert.Contains(t, proxy.Args, "--route-max-age=30m")
	var key *config.File
	for i, f := range cfg.Spec.Files {
		if f.Path == layout.ProxyKeyFile {
			key = &cfg.Spec.Files[i]
		}
	}
	require.NotNil(t, key)
	assert.Equal(t, n.Proxy.Key, key.Content)
	assert.Equal(t, "0600", key.Mode)
	assert.Equal(t, &config.Owner{UID: 6003, GID: 6003}, key.Owner)
}

func TestCheck_RefusesAProxyWithoutTheHostAPIOrWithoutItsPair(t *testing.T) {
	require.NoError(t, proxyHost(t).Check(), "control")
	tests := map[string]func(*layout.Node){
		"no host API": func(n *layout.Node) { n.Host = nil },
		"no key":      func(n *layout.Node) { n.Proxy.Key = "" },
		"no certificate": func(n *layout.Node) {
			n.Proxy.Certificate = ""
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			n := proxyHost(t)
			mutate(&n)
			assert.Error(t, n.Check())
		})
	}
}
