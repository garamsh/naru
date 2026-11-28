package hostclient_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/routes"
	"github.com/garamsh/naru/orch/internal/trust"
)

// hostServer answers GET /v1/host with the proxy given, or with none when it is empty, and PUT
// /v1/routes with loaded, keeping the table it was put; it returns its address and a client of it.
func hostServer(t *testing.T, proxy string, loaded int64, got *hostapi.RoutesRequest) (string, hostclient.RouteAPI) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/host":
			_, _ = w.Write([]byte(`{"region":"eu-1"` + proxy + `}`))
		case "PUT /v1/routes":
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, got))
			_ = json.NewEncoder(w).Encode(hostapi.RoutesAnswer{Loaded: loaded})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "https://"), hostapi.NewClient(srv.URL, srv.Client())
}

func TestRouteHosts_PushesToTheHostsThatReportAProxyAndAnswersWhatTheyLoaded(t *testing.T) {
	var gotP1, gotM1 hostapi.RoutesRequest
	p1, c1 := hostServer(t, `,"proxy":{"healthy":false,"routeMaxAgeSeconds":5400}`, 41, &gotP1)
	m1, c2 := hostServer(t, "", 0, &gotM1)
	var gotU1 hostapi.RoutesRequest
	u1, c3 := hostServer(t, `,"proxy":{"healthy":true,"routeMaxAgeSeconds":3600}`, 0, &gotU1)
	clients := map[string]hostclient.RouteAPI{p1: c1, m1: c2, u1: c3}
	inv := &invFake{hosts: []orchapi.Host{{ID: "p1", Address: p1, Proxy: true}, {ID: "m1", Address: m1, Proxy: true}, {ID: "u1", Address: u1}}}
	h := hostclient.NewRouteHosts(inv, func(_, addr string) hostclient.RouteAPI { return clients[addr] })

	proxies, err := h.Proxies(t.Context())
	require.NoError(t, err)
	assert.Equal(t, routes.Probe{Proxies: []routes.Proxy{{ID: "p1", MaxAgeSeconds: 5400}}, Ungranted: []string{"u1"}, NoProxy: []string{"m1"}}, proxies,
		"a granted proxy that is not healthy still takes the table that stops it; a proxy without the grant is not one; a grant without a proxy is no proxy")
	admitted, err := h.Admitted(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"p1", "m1", "u1"}, admitted)

	sent := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	loaded, err := h.Push(t.Context(), "p1", hostapi.RoutesRequest{Revision: 42, SentAt: sent})
	require.NoError(t, err)
	assert.Equal(t, int64(41), loaded)
	assert.Equal(t, int64(42), gotP1.Revision)
	assert.True(t, gotP1.SentAt.Equal(sent))
	_, err = h.Push(t.Context(), "gone", hostapi.RoutesRequest{})
	assert.Error(t, err, "a host that is not admitted")
}

func TestRouteHosts_TrustIsWhenEachAnsweringHostsPoolEnds(t *testing.T) {
	var got hostapi.RoutesRequest
	a, ca := hostServer(t, `,"trustExpiresAt":"2036-11-28T00:00:00Z","trustExpiresAuthority":"naru-host-ca"`, 0, &got)
	b, cb := hostServer(t, "", 0, &got)
	clients := map[string]hostclient.RouteAPI{a: ca, b: cb}
	inv := &invFake{hosts: []orchapi.Host{{ID: "a", Address: a}, {ID: "b", Address: b}, {ID: "away", Address: "127.0.0.1:1"}}}
	h := hostclient.NewRouteHosts(inv, func(_, addr string) hostclient.RouteAPI {
		if c, ok := clients[addr]; ok {
			return c
		}
		return hostapi.NewClient("https://"+addr, http.DefaultClient)
	})
	hosts, err := h.Trust(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []trust.Host{{ID: "a", ExpiresAt: time.Date(2036, 11, 28, 0, 0, 0, 0, time.UTC), Authority: "naru-host-ca"}}, hosts,
		"a host that reports no expiry, or does not answer, is left out")
}
