package proxy_test

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/proxy/internal/proxy"
)

const nameC = "00000000000000cc.m.test"

func TestRoutes_ARouteThatFailsTheCheckOrANameGivenTwiceIsDroppedAloneAndLogged(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	badAlias := httpRoute(alias, machineA, b.port)
	badAlias.Certificate, badAlias.Key = f.pki.aliasCert, "not a key"
	local := httpRoute(nameB, machineB, b.port)
	local.Address = netip.MustParseAddr("127.0.0.1")

	f.write(2, "", httpRoute(nameA, machineA, b.port), badAlias, local, httpRoute(nameC, machineB, b.port), httpRoute(nameC, machineA, b.port))
	require.NoError(t, f.routes.Load(), "the file is taken without the routes it drops")
	assert.Equal(t, int64(2), health(t, f).Revision)
	assert.Equal(t, "2", revisionFile(t, f), "the revision is answered, so orch is not held at the last one")

	resp, _ := f.get(f.client(true), nameA)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "control: the good route of the file routes")
	for _, name := range []string{alias, nameB, nameC} {
		_, err := f.dialTLS(&tls.Config{ServerName: name})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "unrecognized name", "%s is refused as a name the proxy does not serve", name)
		assert.Contains(t, f.logs.String(), "host="+name, "the drop of %s is logged", name)
	}
	assert.Contains(t, f.logs.String(), "dropped a route")
}

func TestRoutes_AReceivedAtAfterNowCountsTheTablesAgeFromNow(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	now := *f.now.Load()
	f.received = now.Add(24 * time.Hour)
	f.write(2, "", httpRoute(nameA, machineA, b.port))
	require.NoError(t, f.routes.Load())
	assert.True(t, health(t, f).ReceivedAt.Equal(now), "the age counts from the load")
	assert.Contains(t, f.logs.String(), "taken after now")

	later := now.Add(time.Hour + time.Second)
	f.now.Store(&later)
	resp, _ := f.get(f.client(true), nameA)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "the table expires the max age after its load")
}

func TestProxy_DropsTheClientsXRealIP(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	req, err := http.NewRequest(http.MethodGet, "https://"+nameA+"/", nil)
	require.NoError(t, err)
	req.Header.Set("X-Real-IP", "203.0.113.9")
	resp, err := f.client(true).Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var s seen
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&s))
	assert.Empty(t, s.XRealIP)
	assert.Equal(t, "127.0.0.1", s.XFF, "control: the address the proxy saw is forwarded")
}

func TestProxy_ClosesATCPServiceOnATableOlderThanTheMaxAgeWithoutDialling(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	var dialled atomic.Int64
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			dialled.Add(1)
			_, _ = c.Write([]byte("pong"))
			_ = c.Close()
		}
	}()
	r := httpRoute(nameA, machineA, l.Addr().(*net.TCPAddr).Port)
	r.Protocol = hostapi.RouteTCP
	f := newFixture(t, time.Hour, "", r)

	read := func() string {
		c, err := f.dialTLS(&tls.Config{ServerName: nameA})
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, _ := io.ReadAll(c)
		return string(got)
	}
	assert.Equal(t, "pong", read(), "control: within the max age")
	at := f.received.Add(time.Hour + time.Second)
	f.now.Store(&at)
	assert.Empty(t, read())
	assert.Equal(t, int64(1), dialled.Load(), "the Machine is not dialled on an expired table")
}

// pusher is a Machine's service that writes tick every 50 ms for push and is then silent, without
// reading. With upgrade it first answers a WebSocket upgrade.
func pusher(t *testing.T, push time.Duration, upgrade bool) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		if upgrade {
			br := bufio.NewReader(c)
			for {
				line, err := br.ReadString('\n')
				if err != nil || line == "\r\n" {
					break
				}
			}
			_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		}
		for end := time.Now().Add(push); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
			if _, err := io.WriteString(c, "tick\n"); err != nil {
				return
			}
		}
		_, _ = io.Copy(io.Discard, c)
	}()
	return l.Addr().(*net.TCPAddr).Port
}

// receives reads ticks from c, and reports how long the connection lived and how many it carried.
func receives(t *testing.T, c net.Conn) (time.Duration, int) {
	t.Helper()
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(10 * time.Second))
	all, _ := io.ReadAll(c)
	return time.Since(start), strings.Count(string(all), "tick")
}

const idle = 300 * time.Millisecond

func TestProxy_KeepsATCPServiceThatOnlyPushesAndClosesItOnceBothDirectionsAreSilent(t *testing.T) {
	r := httpRoute(nameA, machineA, pusher(t, 4*idle, false))
	r.Protocol = hostapi.RouteTCP
	f := newFixtureWith(t, func(c *proxy.Config) { c.Idle = idle }, time.Hour, "", r)

	c, err := f.dialTLS(&tls.Config{ServerName: nameA})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	lived, ticks := receives(t, c)
	assert.GreaterOrEqual(t, ticks, 15, "the Machine's pushes reach a client that writes nothing, past the idle time")
	assert.Less(t, lived, 4*idle+3*idle, "the connection closes once both directions are silent for the idle time")
}

func TestProxy_KeepsAWebSocketThatOnlyPushesAndClosesItOnceBothDirectionsAreSilent(t *testing.T) {
	f := newFixtureWith(t, func(c *proxy.Config) { c.Idle = idle }, time.Hour, "", httpRoute(nameA, machineA, pusher(t, 4*idle, true)))

	c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", nameA)
	require.NoError(t, err)
	lived, ticks := receives(t, c)
	assert.GreaterOrEqual(t, ticks, 15, "the Machine's pushes reach a client that writes nothing, past the idle time")
	assert.Less(t, lived, 4*idle+3*idle, "the upgraded connection closes once both directions are silent for the idle time")
}

func TestProxy_ClosesAnHTTPConnectionThatSendsNothing(t *testing.T) {
	f := newFixtureWith(t, func(c *proxy.Config) { c.Idle = idle }, time.Hour, "", httpRoute(nameA, machineA, newBackend(t).port))
	c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+nameA+"\r\n")
	require.NoError(t, err)
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(10 * time.Second))
	_, _ = io.ReadAll(c)
	assert.Less(t, time.Since(start), 3*idle, "a request whose headers stop is closed at the idle time")
}

// holds opens a connection to the proxy from the local address from, completes its handshake and
// reports whether the proxy kept it.
func holds(t *testing.T, f *fixture, from string) (net.Conn, bool) {
	t.Helper()
	d := &net.Dialer{Timeout: 5 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(from)}}
	c, err := tls.DialWithDialer(d, "tcp", f.addr, &tls.Config{ServerName: nameA, RootCAs: f.pki.pool, NextProtos: []string{"http/1.1"}})
	if err != nil {
		return nil, false
	}
	t.Cleanup(func() { _ = c.Close() })
	_, err = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", nameA)
	if err != nil {
		return c, false
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		return c, false
	}
	_ = resp.Body.Close()
	return c, resp.StatusCode == http.StatusOK
}

func TestProxy_RefusesAConnectionOverTheLimitOfItsAddressOrOfTheProxy(t *testing.T) {
	b := newBackend(t)
	f := newFixtureWith(t, func(c *proxy.Config) { c.MaxConns, c.MaxPerSource = 3, 2 }, time.Hour, "", httpRoute(nameA, machineA, b.port))

	first, ok := holds(t, f, "127.0.0.1")
	require.True(t, ok)
	_, ok = holds(t, f, "127.0.0.1")
	require.True(t, ok, "control: a second from the address")
	_, ok = holds(t, f, "127.0.0.1")
	assert.False(t, ok, "a third from the address is over its limit")
	_, ok = holds(t, f, "127.0.0.2")
	require.True(t, ok, "control: another address")
	_, ok = holds(t, f, "127.0.0.3")
	assert.False(t, ok, "a fourth in all is over the proxy's limit")

	require.NoError(t, first.Close())
	assert.Eventually(t, func() bool { _, ok := holds(t, f, "127.0.0.3"); return ok }, 5*time.Second, 50*time.Millisecond,
		"a closed connection gives back its slot")
}

// silent is a Machine's tcp service that reads its connection and writes nothing, leaving it
// open; ended receives when its reads end, at the proxy's close or the end of the client's stream.
func silent(t *testing.T) (int, <-chan time.Time) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	ended := make(chan time.Time, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		_, _ = io.Copy(io.Discard, c)
		ended <- time.Now()
		<-time.After(time.Minute)
	}()
	return l.Addr().(*net.TCPAddr).Port, ended
}

func TestProxy_ClosesATCPServiceTheClientHalfClosedOnceTheMachineIsSilentForTheIdleTime(t *testing.T) {
	port, _ := silent(t)
	r := httpRoute(nameA, machineA, port)
	r.Protocol = hostapi.RouteTCP
	f := newFixtureWith(t, func(c *proxy.Config) { c.Idle = idle }, time.Hour, "", r)

	c, err := f.dialTLS(&tls.Config{ServerName: nameA})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = io.WriteString(c, "ping")
	require.NoError(t, err)
	require.NoError(t, c.CloseWrite())
	start := time.Now()
	_ = c.SetReadDeadline(start.Add(10 * time.Second))
	_, _ = io.ReadAll(c)
	assert.Less(t, time.Since(start), 3*idle, "the Machine's silence after the client's end of stream closes it")
}

func TestProxy_ClosesTheMachinesSideOfATCPServiceWhenTheClientsConnectionFails(t *testing.T) {
	port, ended := silent(t)
	r := httpRoute(nameA, machineA, port)
	r.Protocol = hostapi.RouteTCP
	f := newFixtureWith(t, func(c *proxy.Config) { c.Idle = time.Minute }, time.Hour, "", r)

	raw, err := net.Dial("tcp", f.addr)
	require.NoError(t, err)
	c := tls.Client(raw, &tls.Config{ServerName: nameA, RootCAs: f.pki.pool})
	require.NoError(t, c.Handshake())
	_, err = io.WriteString(c, "ping")
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	require.NoError(t, raw.(*net.TCPConn).SetLinger(0))
	start := time.Now()
	require.NoError(t, raw.Close())
	select {
	case at := <-ended:
		assert.Less(t, at.Sub(start), 5*time.Second, "a reset closes the Machine's side long before the idle time")
	case <-time.After(10 * time.Second):
		t.Fatal("the Machine's side stayed open after the client's connection failed")
	}
}
