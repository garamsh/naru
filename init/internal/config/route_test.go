package config_test

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/config"
)

// routeDoc is the valid document with spec.network.routes, each entry a "to via" pair written
// as a flow mapping.
func routeDoc(entries ...string) string {
	doc := strings.Replace(valid, "    forwarding: [ipv4, ipv6]\n", "    forwarding: [ipv4, ipv6]\n    routes:\n", 1)
	var list strings.Builder
	for _, e := range entries {
		list.WriteString("      - " + e + "\n")
	}
	return strings.Replace(doc, "    routes:\n", "    routes:\n"+list.String(), 1)
}

func TestParse_AcceptsSpecNetworkRoutes(t *testing.T) {
	cfg, err := config.Parse([]byte(routeDoc("{to: 10.200.2.0/24, via: 10.78.0.2}", "{to: 10.200.3.0/24, via: 10.78.0.3}")))

	require.NoError(t, err)
	assert.Equal(t, []config.Route{
		{To: netip.MustParsePrefix("10.200.2.0/24"), Via: netip.MustParseAddr("10.78.0.2")},
		{To: netip.MustParsePrefix("10.200.3.0/24"), Via: netip.MustParseAddr("10.78.0.3")},
	}, cfg.Spec.Network.Routes)
}

func TestParse_RefusesASpecNetworkRouteAndNamesTheField(t *testing.T) {
	tests := []struct {
		name  string
		doc   string
		field string
	}{
		{"no destination", routeDoc("{via: 10.78.0.2}"), "spec.network.routes[0].to"},
		{"no next hop", routeDoc("{to: 10.200.2.0/24}"), "spec.network.routes[0].via"},
		{"a destination that is an address", routeDoc("{to: 10.200.2.0, via: 10.78.0.2}"), "spec.network.routes[0].to"},
		{"an IPv6 destination", routeDoc("{to: 'fd00::/64', via: 10.78.0.2}"), "spec.network.routes[0].to"},
		{"the default route", routeDoc("{to: 0.0.0.0/0, via: 10.78.0.2}"), "spec.network.routes[0].to"},
		{"host bits in the destination", routeDoc("{to: 10.200.2.5/24, via: 10.78.0.2}"), "spec.network.routes[0].to"},
		{"an IPv6 next hop", routeDoc("{to: 10.200.2.0/24, via: 'fd00::2'}"), "spec.network.routes[0].via"},
		{"an unspecified next hop", routeDoc("{to: 10.200.2.0/24, via: 0.0.0.0}"), "spec.network.routes[0].via"},
		{"a loopback next hop", routeDoc("{to: 10.200.2.0/24, via: 127.0.0.1}"), "spec.network.routes[0].via"},
		{"a multicast next hop", routeDoc("{to: 10.200.2.0/24, via: 224.0.0.1}"), "spec.network.routes[0].via"},
		{"a next hop inside the destination", routeDoc("{to: 10.78.0.0/24, via: 10.78.0.2}"), "spec.network.routes[0].via"},
		{"a destination twice", routeDoc("{to: 10.200.2.0/24, via: 10.78.0.2}", "{to: 10.200.2.0/24, via: 10.78.0.3}"), "spec.network.routes[1].to"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.Parse([]byte(tt.doc))

			require.Error(t, err)
			assert.Equal(t, []string{tt.field}, fields(t, err))
		})
	}
}

func TestParse_RefusesASpecNetworkRouteThatIsNotAnAddress(t *testing.T) {
	_, err := config.Parse([]byte(routeDoc("to: 10.200.2.0/24\n        via: next-door")))

	require.Error(t, err)
	assert.Equal(t, []string{"spec.network.routes[0].via"}, fields(t, err))
}
