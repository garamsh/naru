package api

import (
	"context"
	"encoding/json"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

type routesEnv struct {
	*env
	routes *Routes
	now    *time.Time
}

// startRoutes starts a server whose host runs a proxy, with its routes file and the proxy's
// revision file in a directory of the test, and a clock the test sets.
func startRoutes(t *testing.T) routesEnv {
	t.Helper()
	dir := t.TempDir()
	rt := &Routes{File: filepath.Join(dir, "routes.json"), GID: -1, RevisionFile: filepath.Join(dir, "revision")}
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) {
		d.Routes = rt
		d.Now = func() time.Time { return now }
	})
	return routesEnv{e, rt, &now}
}

func table(rev int64, sentAt time.Time, routes ...hostapi.Route) hostapi.RoutesRequest {
	return hostapi.RoutesRequest{Revision: rev, SentAt: sentAt, Routes: routes}
}

func route(name string) hostapi.Route {
	return hostapi.Route{Hostname: name, Machine: "0123456789abcdef", Address: netip.MustParseAddr("10.0.0.7"), Port: 8080, Protocol: hostapi.RouteHTTP, Running: true}
}

func (e routesEnv) written(t *testing.T) hostapi.RoutesFile {
	t.Helper()
	b, err := os.ReadFile(e.routes.File)
	require.NoError(t, err)
	var f hostapi.RoutesFile
	require.NoError(t, json.Unmarshal(b, &f))
	return f
}

func (e routesEnv) put(t *testing.T, req hostapi.RoutesRequest) (int, hostapi.ErrorBody, hostapi.RoutesAnswer) {
	t.Helper()
	code, b := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/routes", req)
	var refusal hostapi.ErrorBody
	var answer hostapi.RoutesAnswer
	if code == 200 {
		require.NoError(t, json.Unmarshal(b, &answer), string(b))
	} else {
		require.NoError(t, json.Unmarshal(b, &refusal), string(b))
	}
	return code, refusal, answer
}

func TestRoutes_TheHostWritesTheTableForItsProxyAndAnswersTheRevisionTheProxyLoaded(t *testing.T) {
	e := startRoutes(t)
	require.NoError(t, e.nets.SetPeers(t.Context(), netip.MustParseAddr("10.99.0.2"), netip.Prefix{}, nil))

	code, _, answer := e.put(t, table(7, e.now.Add(-time.Second), route("a.m.example")))
	require.Equal(t, 200, code)
	assert.Equal(t, int64(0), answer.Loaded, "the proxy has loaded no table yet")
	f := e.written(t)
	assert.Equal(t, int64(7), f.Revision)
	assert.Equal(t, []hostapi.Route{route("a.m.example")}, f.Routes)
	assert.True(t, f.ReceivedAt.Equal(*e.now), "the host's own time of taking it, which the proxy counts the table's age from")
	assert.Equal(t, netip.MustParseAddr("10.99.0.2"), f.MeshAddress, "the address the proxy dials Machines from")
	st, err := os.Stat(e.routes.File)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), st.Mode().Perm())

	require.NoError(t, os.WriteFile(e.routes.RevisionFile, []byte("7\n"), 0o644))
	_, _, answer = e.put(t, table(7, *e.now, route("a.m.example")))
	assert.Equal(t, int64(7), answer.Loaded, "the revision the proxy wrote")
}

func TestRoutes_ALowerRevisionIsRefusedAndTheSameTaken(t *testing.T) {
	e := startRoutes(t)
	code, _, _ := e.put(t, table(7, *e.now, route("a.m.example")))
	require.Equal(t, 200, code)

	code, refusal, _ := e.put(t, table(6, *e.now, route("b.m.example")))
	assert.Equal(t, 409, code)
	assert.Equal(t, hostapi.CodeStaleTable, refusal.Code)
	assert.Equal(t, []hostapi.Route{route("a.m.example")}, e.written(t).Routes, "the table taken stays")

	code, _, _ = e.put(t, table(7, *e.now, route("b.m.example")))
	assert.Equal(t, 200, code, "the same revision is taken, so each push keeps the table young")
	code, _, _ = e.put(t, table(8, *e.now))
	assert.Equal(t, 200, code)
	assert.Empty(t, e.written(t).Routes)
}

func TestRoutes_ARestartedHostStillRefusesARevisionBelowTheTableItWrote(t *testing.T) {
	e := startRoutes(t)
	code, _, _ := e.put(t, table(7, *e.now, route("a.m.example")))
	require.Equal(t, 200, code)

	again := &Routes{File: e.routes.File, GID: -1, RevisionFile: e.routes.RevisionFile}
	now := *e.now
	e2 := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) {
		d.Routes, d.Now = again, func() time.Time { return now }
	})
	r := routesEnv{e2, again, &now}
	code, refusal, _ := r.put(t, table(6, now))
	assert.Equal(t, 409, code)
	assert.Equal(t, hostapi.CodeStaleTable, refusal.Code)
	code, _, _ = r.put(t, table(7, now))
	assert.Equal(t, 200, code, "control: the revision it wrote is taken")
}

func TestRoutes_ATableSentLongerAgoThanThePushAndTheSkewAllowIsRefusedWithTheDifference(t *testing.T) {
	e := startRoutes(t)
	code, _, _ := e.put(t, table(1, e.now.Add(-hostapi.RouteAllowance)))
	assert.Equal(t, 200, code, "sent exactly the allowance before it arrived")
	code, _, _ = e.put(t, table(2, e.now.Add(time.Minute)))
	assert.Equal(t, 200, code, "orch's clock ahead of the host's makes a table no older")

	code, refusal, _ := e.put(t, table(3, e.now.Add(-hostapi.RouteAllowance-1500*time.Millisecond)))
	assert.Equal(t, 409, code)
	assert.Equal(t, hostapi.CodeLateTable, refusal.Code)
	assert.Contains(t, refusal.Error, "5.5s", "the difference seen, so a host whose clock runs off is seen")
	assert.Equal(t, int64(2), e.written(t).Revision, "the late table is not written")
	assert.Equal(t, 4*time.Second, hostapi.RouteAllowance, "a 2 s push and 2 s of skew")
}

func TestRoutes_ARouteTheProxyWouldDropIsRefused(t *testing.T) {
	e := startRoutes(t)
	bad := route("a.m.example")
	bad.Address = netip.MustParseAddr("127.0.0.1")
	code, refusal, _ := e.put(t, table(1, *e.now, route("b.m.example"), bad, route("b.m.example")))
	assert.Equal(t, 422, code)
	assert.Equal(t, []string{
		"routes[1]: the address 127.0.0.1 is not a Machine's",
		"routes[2].hostname: b.m.example is named twice",
	}, refusal.Fields)
	_, err := os.Stat(e.routes.File)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing is written")

	code, _, _ = e.put(t, table(1, *e.now, route("b.m.example"), route("a.m.example")))
	assert.Equal(t, 200, code, "control: the same table with good routes")
}

func TestRoutes_AHostThatRunsNoProxyAnswers501(t *testing.T) {
	e := start(t)
	code, _ := e.do(t, e.as(t, ProvisionerOrganization), "PUT", "/v1/routes", table(1, time.Now()))
	assert.Equal(t, 501, code)
	code, _ = e.do(t, e.as(t, ReaderOrganization), "PUT", "/v1/routes", table(1, time.Now()))
	assert.Equal(t, 403, code, "a reader may not push")
}

func TestRoutes_TheProxysRevisionFileIsNotFollowedAndIsBounded(t *testing.T) {
	e := startRoutes(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	require.NoError(t, os.WriteFile(other, []byte("9\n"), 0o644))
	require.NoError(t, os.Symlink(other, e.routes.RevisionFile))
	_, err := e.routes.proxyLoaded()
	assert.Error(t, err, "a link in the proxy's directory is not followed")

	require.NoError(t, os.Remove(e.routes.RevisionFile))
	require.NoError(t, os.WriteFile(e.routes.RevisionFile, []byte(strconv.Itoa(1)+strings.Repeat(" ", maxRevisionFile)), 0o644))
	_, err = e.routes.proxyLoaded()
	assert.Error(t, err, "longer than a revision, though it holds one among the spaces")

	require.NoError(t, os.WriteFile(e.routes.RevisionFile, []byte("12\n"), 0o644))
	got, err := e.routes.proxyLoaded()
	require.NoError(t, err)
	assert.Equal(t, int64(12), got, "control")
}

func TestHost_ReportsTheProxysMaxAgeRoundedUpToASecond(t *testing.T) {
	rt := &Routes{File: filepath.Join(t.TempDir(), "routes.json"), GID: -1, MaxAge: 90*time.Minute + time.Millisecond}
	e := startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, nil, func(d *Deps) {
		d.Routes = rt
		d.ProxyHealthy = func(context.Context) bool { return true }
	})
	code, b := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	require.Equal(t, 200, code, string(b))
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	require.NotNil(t, h.Proxy)
	assert.Equal(t, int64(90*60+1), h.Proxy.RouteMaxAgeSeconds, "a proxy may forward up to the millisecond past 90 minutes, so orch waits the whole second")
}
