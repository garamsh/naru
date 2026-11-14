package api

import (
	"encoding/json"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/hostapi"
)

const keyOne = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func TestNetwork_DeclareListGetDelete(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	r := e.as(t, ReaderOrganization)

	code, b := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16", APIEndpoint: "10.1.0.2"})
	require.Equal(t, 200, code, string(b))
	assert.Contains(t, e.nft.ruleset(), "ip saddr @net_a ip daddr @net_a accept")
	assert.Contains(t, e.nft.ruleset(), "ip saddr 172.16.0.0/24 ip daddr 10.1.0.2 accept", "management reaches the API endpoint")

	code, b = e.do(t, r, "GET", "/v1/networks", nil)
	require.Equal(t, 200, code, string(b))
	var list struct{ Networks []hostapi.Network }
	require.NoError(t, json.Unmarshal(b, &list))
	assert.Equal(t, []hostapi.Network{{Name: "a", Prefix: "10.1.0.0/16", APIEndpoint: "10.1.0.2", RemoteMembers: []string{}}}, list.Networks)

	code, b = e.do(t, r, "GET", "/v1/networks/a", nil)
	require.Equal(t, 200, code, string(b))

	code, _ = e.do(t, p, "PUT", "/v1/networks/b", hostapi.NetworkRequest{Prefix: "10.2.0.0/16"})
	require.Equal(t, 200, code)
	assert.Contains(t, e.nft.ruleset(), "ip saddr @net_a ip daddr @networks counter drop", "a network does not reach another")

	code, _ = e.do(t, p, "DELETE", "/v1/networks/b", nil)
	assert.Equal(t, 204, code)
	assert.NotContains(t, e.nft.ruleset(), "10.2.0.0/16")
	code, _ = e.do(t, p, "DELETE", "/v1/networks/b", nil)
	assert.Equal(t, 204, code, "an absent network is 204")
	code, _ = e.do(t, r, "GET", "/v1/networks/b", nil)
	assert.Equal(t, 404, code)
}

func TestNetwork_Refusals(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)

	for name, tc := range map[string]struct {
		path string
		body hostapi.NetworkRequest
		want int
	}{
		"overlap":          {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "10.1.4.0/24"}, 409},
		"not a prefix":     {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "ten"}, 422},
		"not masked":       {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "10.2.0.1/16"}, 422},
		"too small":        {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "10.2.0.0/30"}, 422},
		"ipv6":             {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "fd00::/16"}, 422},
		"endpoint outside": {"/v1/networks/b", hostapi.NetworkRequest{Prefix: "10.2.0.0/16", APIEndpoint: "10.9.0.1"}, 422},
		"bad name":         {"/v1/networks/B_", hostapi.NetworkRequest{Prefix: "10.2.0.0/16"}, 422},
	} {
		code, b := e.do(t, p, "PUT", tc.path, tc.body)
		assert.Equal(t, tc.want, code, "%s: %s", name, b)
	}
	assert.Len(t, e.nets.Networks(), 1, "a refused network is not kept")
}

func TestNetwork_RolesAndAHostWithoutNetworks(t *testing.T) {
	e := start(t)
	r := e.as(t, ReaderOrganization)
	code, _ := e.do(t, r, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	assert.Equal(t, 403, code, "a reader declares nothing")
	code, _ = e.do(t, r, "PUT", "/v1/peers", hostapi.PeersRequest{})
	assert.Equal(t, 403, code)
	code, _ = e.do(t, r, "DELETE", "/v1/networks/a", nil)
	assert.Equal(t, 403, code)
	assert.Empty(t, e.nft.calls, "a refused request reached the host")

	none := startEnv(t, netip.MustParseAddr("10.1.0.1"), false)
	p := none.as(t, ProvisionerOrganization)
	code, _ = none.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	assert.Equal(t, 501, code)
	code, _ = none.do(t, p, "GET", "/v1/peers", nil)
	assert.Equal(t, 501, code)
	body := createBody()
	body["network"] = "a"
	code, _ = none.do(t, p, "POST", "/v1/microvms", body)
	assert.Equal(t, 501, code, "a microVM is not created on a network the host does not have")
	assert.NotContains(t, none.rt.calls, "create web")
}

func TestNetwork_AMicroVMIsAttachedAtCreateAndDetachedAtDelete(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	assert.NotContains(t, e.nft.ruleset(), "10.1.0.5", "no member yet")

	body := createBody()
	body["network"] = "a"
	code, b := e.do(t, p, "POST", "/v1/microvms", body)
	require.Equal(t, 201, code, string(b))
	assert.Contains(t, e.nft.ruleset(), "elements = { 10.1.0.0/16 }", "the address mvmd gave is in the prefix already")
	assert.Equal(t, []network.Member{{Network: "a", Address: netip.MustParseAddr("10.1.0.5")}}, e.nets.Members())

	code, _ = e.do(t, p, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	assert.NotContains(t, e.nft.ruleset(), "10.1.0.5", "a deleted microVM's address is not a known source")
}

func TestNetwork_ACreateThatCannotAttachIsRefusedAndRemoved(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	body := createBody()
	body["network"] = "nowhere"
	code, b := e.do(t, p, "POST", "/v1/microvms", body)
	assert.Equal(t, 422, code, string(b))
	assert.NotContains(t, e.rt.calls, "create web", "an undeclared network is refused before mvmd is called")

	// The address mvmd gives (10.1.0.5) need not be inside the prefix: only the pod prefix is.
	code, _ = e.do(t, p, "PUT", "/v1/networks/far", hostapi.NetworkRequest{Prefix: "10.9.0.0/16"})
	require.Equal(t, 200, code)
	body["network"] = "far"
	code, b = e.do(t, p, "POST", "/v1/microvms", body)
	assert.Equal(t, 201, code, string(b))
	assert.Equal(t, []network.Member{{Network: "far", Address: netip.MustParseAddr("10.1.0.5")}}, e.nets.Members())

	// A pod prefix outside it is refused, and no microVM is made.
	code, _ = e.do(t, p, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	pod := osBody()
	pod["network"] = "far"
	pod["podPrefix"] = "10.7.0.0/24"
	code, b = e.do(t, p, "POST", "/v1/microvms", pod)
	assert.Equal(t, 422, code, string(b))
	code, _ = e.do(t, p, "GET", "/v1/microvms/web", nil)
	assert.Equal(t, 404, code, "no microVM was made")
}

func TestNetwork_PodPrefixMustBeInsideTheNetwork(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, p, "PUT", "/v1/networks/a", hostapi.NetworkRequest{Prefix: "10.1.0.0/16"})
	require.Equal(t, 200, code)
	body := osBody()
	body["network"] = "a"
	body["podPrefix"] = "10.7.0.0/24"
	code, b := e.do(t, p, "POST", "/v1/microvms", body)
	assert.Equal(t, 422, code, string(b))
	assert.Contains(t, string(b), "podPrefix")
	assert.NotContains(t, e.rt.calls, "createMachine web", "refused before mvmd is called")

	body["podPrefix"] = "10.1.7.0/24"
	code, b = e.do(t, p, "POST", "/v1/microvms", body)
	assert.Equal(t, 201, code, string(b))
}

func TestPeers_SetGetAndRefusals(t *testing.T) {
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	r := e.as(t, ReaderOrganization)
	peers := []hostapi.Peer{
		{Name: "h2", PublicKey: keyOne, Endpoint: "192.0.2.2:51820", Prefixes: []string{"10.1.1.0/24", "10.2.0.0/24"}},
	}
	code, b := e.do(t, p, "PUT", "/v1/peers", hostapi.PeersRequest{Peers: peers})
	require.Equal(t, 200, code, string(b))
	var got hostapi.PeersRequest
	require.NoError(t, json.Unmarshal(b, &got))
	assert.Equal(t, peers, got.Peers)

	assert.Equal(t, []network.Peer{{Name: "h2", PublicKey: keyOne, Endpoint: netip.MustParseAddrPort("192.0.2.2:51820"), Prefixes: []netip.Prefix{netip.MustParsePrefix("10.1.1.0/24"), netip.MustParsePrefix("10.2.0.0/24")}}}, e.tunnel.peers,
		"the tunnel is given the peer with only the prefixes it holds")
	for _, c := range e.nft.calls {
		assert.NotEqual(t, "wg", c[0], "the host no longer runs wg")
	}

	code, b = e.do(t, r, "GET", "/v1/peers", nil)
	require.Equal(t, 200, code, string(b))

	for name, bad := range map[string][]hostapi.Peer{
		"short key":       {{PublicKey: "abc", Prefixes: []string{}}},
		"duplicate key":   {{PublicKey: keyOne}, {PublicKey: keyOne}},
		"bad endpoint":    {{PublicKey: keyOne, Endpoint: "nope"}},
		"unmasked":        {{PublicKey: keyOne, Prefixes: []string{"10.1.1.1/24"}}},
		"held by two":     {{PublicKey: keyOne, Prefixes: []string{"10.1.1.0/24"}}, {PublicKey: "BAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", Prefixes: []string{"10.1.1.0/24"}}},
		"not a prefix":    {{PublicKey: keyOne, Prefixes: []string{"x"}}},
		"ipv6 allowed ip": {{PublicKey: keyOne, Prefixes: []string{"fd00::/64"}}},
	} {
		code, b := e.do(t, p, "PUT", "/v1/peers", hostapi.PeersRequest{Peers: bad})
		assert.Equal(t, 422, code, "%s: %s", name, b)
	}
}
