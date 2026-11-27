package proxy_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/proxy/internal/proxy"
)

// redirects serves f's proxy's redirect on a loopback listener and returns its address.
func redirects(t *testing.T, f *fixture) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.srv.ServeRedirects(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return l.Addr().String()
}

// ask sends raw on a new connection to addr and reads the response.
func ask(t *testing.T, addr, raw string) (*http.Response, net.Conn) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	_, err = io.WriteString(c, raw)
	require.NoError(t, err)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	require.NoError(t, err)
	_ = resp.Body.Close()
	return resp, c
}

func TestRedirect_AnswersPlainHTTPWithAPermanentRedirectToHTTPSOnTheSameNameAndPath(t *testing.T) {
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, newBackend(t).port))
	addr := redirects(t, f)

	resp, _ := ask(t, addr, "POST /a/b?c=d HTTP/1.1\r\nHost: "+nameA+":80\r\nContent-Length: 0\r\n\r\n")
	assert.Equal(t, http.StatusPermanentRedirect, resp.StatusCode, "308, so the client repeats the method")
	assert.Equal(t, "https://"+nameA+"/a/b?c=d", resp.Header.Get("Location"), "the port is dropped and the path and query kept")

	resp, _ = ask(t, addr, "GET /a HTTP/1.1\r\nHost: [fd00::1]:80\r\n\r\n")
	assert.Equal(t, "https://[fd00::1]/a", resp.Header.Get("Location"), "an IPv6 literal keeps its brackets without its port")
	resp, _ = ask(t, addr, "GET /a HTTP/1.1\r\nHost: [fd00::1]\r\n\r\n")
	assert.Equal(t, "https://[fd00::1]/a", resp.Header.Get("Location"), "and with no port")

	resp, _ = ask(t, addr, "GET / HTTP/1.0\r\n\r\n")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "a request with no name has nothing to redirect to")
}

func TestRedirect_CountsAgainstTheProxysConnectionLimits(t *testing.T) {
	f := newFixtureWith(t, func(c *proxy.Config) { c.MaxConns, c.MaxPerSource = 8, 1 }, time.Hour, "", httpRoute(nameA, machineA, newBackend(t).port))
	addr := redirects(t, f)

	held, _ := ask(t, addr, "GET / HTTP/1.1\r\nHost: "+nameA+"\r\n\r\n")
	require.Equal(t, http.StatusPermanentRedirect, held.StatusCode, "control: the first connection, kept alive")
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "GET / HTTP/1.1\r\nHost: "+nameA+"\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, _ := io.ReadAll(c)
	assert.False(t, strings.HasPrefix(string(got), "HTTP/1.1 308"), "a second connection from the address is over its limit and closed")
}
