package proxy_test

import (
	"bufio"
	"context"
	"errors"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/proxy/internal/proxy"
)

const (
	machineA = "0123456789abcdef"
	machineB = "fedcba9876543210"
	nameA    = machineA + ".m.test"
	nameB    = machineB + ".m.test"
	alias    = "shop.example.org"
)

// pki is a test authority with the zone's wildcard pair and an alias's own pair.
type pki struct {
	pool        *x509.CertPool
	zone        tls.Certificate
	aliasCert   string
	aliasKey    string
	aliasSerial *big.Int
}

func newPKI(t *testing.T) pki {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)
	leaf := func(serial int64, name string) (string, string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		require.NoError(t, err)
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		require.NoError(t, err)
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	}
	zc, zk := leaf(2, "*.m.test")
	zone, err := tls.X509KeyPair([]byte(zc), []byte(zk))
	require.NoError(t, err)
	ac, ak := leaf(3, alias)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return pki{pool: pool, zone: zone, aliasCert: ac, aliasKey: ak, aliasSerial: big.NewInt(3)}
}

// backend is a Machine's http service: it answers what it saw of each request.
type backend struct {
	port int
	hits atomic.Int64
}

type seen struct {
	Host, Remote, XFF, XFP, Proto string
}

func newBackend(t *testing.T) *backend {
	t.Helper()
	b := &backend{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.hits.Add(1)
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		_ = json.NewEncoder(w).Encode(seen{Host: r.Host, Remote: host, XFF: r.Header.Get("X-Forwarded-For"), XFP: r.Header.Get("X-Forwarded-Proto"), Proto: r.Proto})
	}))
	t.Cleanup(srv.Close)
	b.port = srv.Listener.Addr().(*net.TCPAddr).Port
	return b
}

func httpRoute(name, machine string, port int) hostapi.Route {
	return hostapi.Route{Hostname: name, Machine: machine, Address: netip.MustParseAddr("127.0.0.1"), Port: port, Protocol: hostapi.RouteHTTP, Running: true}
}

// fixture is a proxy serving on a loopback listener, with its routes file and clock.
type fixture struct {
	t        *testing.T
	pki      pki
	addr     string
	dir      string
	routes   *proxy.Routes
	srv      *proxy.Server
	now      atomic.Pointer[time.Time]
	received time.Time
}

func (f *fixture) path() string     { return filepath.Join(f.dir, "routes.json") }
func (f *fixture) revision() string { return filepath.Join(f.dir, "revision") }

func (f *fixture) write(revision int64, mesh string, routes ...hostapi.Route) {
	f.t.Helper()
	file := hostapi.RoutesFile{RoutesRequest: hostapi.RoutesRequest{Revision: revision, SentAt: f.received, Routes: routes}, ReceivedAt: f.received}
	if mesh != "" {
		file.MeshAddress = netip.MustParseAddr(mesh)
	}
	raw, err := json.Marshal(file)
	require.NoError(f.t, err)
	f.writeRaw(raw)
}

func (f *fixture) writeRaw(raw []byte) {
	f.t.Helper()
	tmp := f.path() + ".tmp"
	require.NoError(f.t, os.WriteFile(tmp, raw, 0o640))
	require.NoError(f.t, os.Rename(tmp, f.path()))
}

func newFixture(t *testing.T, maxAge time.Duration, mesh string, routes ...hostapi.Route) *fixture {
	t.Helper()
	f := &fixture{t: t, pki: newPKI(t), dir: t.TempDir(), received: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}
	now := f.received.Add(time.Minute)
	f.now.Store(&now)
	f.write(1, mesh, routes...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	f.routes = proxy.NewRoutes(f.path(), f.revision(), log)
	require.NoError(t, f.routes.Load())
	f.srv = proxy.New(proxy.Config{Zone: f.pki.zone, Routes: f.routes, MaxAge: maxAge, Now: func() time.Time { return *f.now.Load() }, Log: log})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.addr = l.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = f.srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

// client returns an HTTP client that reaches the proxy for every name, over HTTP/2 when h2.
func (f *fixture) client(h2 bool) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: f.pki.pool},
		ForceAttemptHTTP2: h2,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, f.addr)
		},
	}
	if !h2 {
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	f.t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func (f *fixture) get(c *http.Client, name string) (*http.Response, seen) {
	f.t.Helper()
	resp, err := c.Get("https://" + name + "/")
	require.NoError(f.t, err)
	defer func() { _ = resp.Body.Close() }()
	var s seen
	if resp.StatusCode == http.StatusOK {
		require.NoError(f.t, json.NewDecoder(resp.Body).Decode(&s))
	}
	return resp, s
}

func (f *fixture) dialTLS(conf *tls.Config) (*tls.Conn, error) {
	if conf.RootCAs == nil {
		conf.RootCAs = f.pki.pool
	}
	return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", f.addr, conf)
}

func TestProxy_RoutesARequestByItsHostToTheMachineFromTheMeshAddressAddingTheForwardedHeaders(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "127.0.0.2", httpRoute(nameA, machineA, b.port))

	for _, h2 := range []bool{true, false} {
		resp, s := f.get(f.client(h2), nameA)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		if h2 {
			assert.Equal(t, 2, resp.ProtoMajor, "HTTP/2 to the client")
		} else {
			assert.Equal(t, 1, resp.ProtoMajor, "HTTP/1.1 to the client")
		}
		assert.Equal(t, nameA, s.Host, "the Machine sees the name it was reached by")
		assert.Equal(t, "127.0.0.2", s.Remote, "the connection leaves from the host's mesh address")
		assert.Equal(t, "127.0.0.1", s.XFF)
		assert.Equal(t, "https", s.XFP)
	}
}

func TestProxy_RefusesAtTheHandshakeANameWithoutARouteOrWithoutAName(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))

	c, err := f.dialTLS(&tls.Config{ServerName: nameA})
	require.NoError(t, err, "control: a name with a route completes its handshake")
	_ = c.Close()

	_, err = f.dialTLS(&tls.Config{ServerName: nameB})
	require.Error(t, err, "a name under the zone with no route")
	_, err = f.dialTLS(&tls.Config{InsecureSkipVerify: true})
	require.Error(t, err, "no SNI")
	assert.Zero(t, b.hits.Load())
}

func TestProxy_RefusesANameTheZoneCertificateDoesNotCoverAndHasNoCertificateOfItsOwn(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port), httpRoute("elsewhere.example.org", machineA, b.port))

	_, err := f.dialTLS(&tls.Config{ServerName: "elsewhere.example.org", InsecureSkipVerify: true})
	require.Error(t, err)
	c, err := f.dialTLS(&tls.Config{ServerName: nameA})
	require.NoError(t, err, "control")
	_ = c.Close()
}

func TestProxy_TakesTLS12And13AndNothingOlder(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))

	for _, v := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		c, err := f.dialTLS(&tls.Config{ServerName: nameA, MinVersion: v, MaxVersion: v})
		require.NoError(t, err, tls.VersionName(v))
		assert.Equal(t, v, c.ConnectionState().Version)
		_ = c.Close()
	}
	_, err := f.dialTLS(&tls.Config{ServerName: nameA, MinVersion: tls.VersionTLS11, MaxVersion: tls.VersionTLS11})
	require.Error(t, err)
}

// request sends one HTTP/1.1 request with host over c and returns the answer.
func request(t *testing.T, c net.Conn, br *bufio.Reader, host string) *http.Response {
	t.Helper()
	_, err := fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\n\r\n", host)
	require.NoError(t, err)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestProxy_AnswersARequestForANameTheConnectionDoesNotServe421(t *testing.T) {
	a, b := newBackend(t), newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, a.port), httpRoute(nameB, machineB, b.port))
	shop := httpRoute(alias, machineB, b.port)
	shop.Certificate, shop.Key = f.pki.aliasCert, f.pki.aliasKey
	stream := httpRoute("0000000000000001.m.test", machineB, b.port)
	stream.Protocol = hostapi.RouteTCP
	f.write(2, "", httpRoute(nameA, machineA, a.port), httpRoute(nameB, machineB, b.port), shop, stream)
	require.NoError(t, f.routes.Load())

	c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	br := bufio.NewReader(c)

	assert.Equal(t, http.StatusOK, request(t, c, br, nameB).StatusCode, "control: another name the zone's certificate covers reaches its Machine on the same connection")
	assert.Equal(t, int64(1), b.hits.Load())
	assert.Equal(t, http.StatusMisdirectedRequest, request(t, c, br, alias).StatusCode, "an alias with its own certificate")
	assert.Equal(t, http.StatusMisdirectedRequest, request(t, c, br, "0000000000000000.m.test").StatusCode, "a name the certificate covers with no route")
	assert.Equal(t, http.StatusMisdirectedRequest, request(t, c, br, "0000000000000001.m.test").StatusCode, "a name whose route is a tcp service")
	assert.Equal(t, int64(1), b.hits.Load())
}

func TestProxy_ServesAnAliasWithItsOwnCertificate(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	shop := httpRoute(alias, machineA, b.port)
	shop.Certificate, shop.Key = f.pki.aliasCert, f.pki.aliasKey
	f.write(2, "", httpRoute(nameA, machineA, b.port), shop)
	require.NoError(t, f.routes.Load())

	c, err := f.dialTLS(&tls.Config{ServerName: alias})
	require.NoError(t, err)
	assert.Equal(t, f.pki.aliasSerial, c.ConnectionState().PeerCertificates[0].SerialNumber)
	_ = c.Close()
	resp, s := f.get(f.client(true), alias)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, alias, s.Host)
}

func TestProxy_AnswersAMachineThatIsNotRunning503WithRetryAfterAndNoConnection(t *testing.T) {
	b := newBackend(t)
	stopped := httpRoute(nameB, machineB, b.port)
	stopped.Running = false
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port), stopped)
	c := f.client(true)

	resp, _ := f.get(c, nameB)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, "5", resp.Header.Get("Retry-After"))
	assert.Zero(t, b.hits.Load(), "the Machine is not dialled")
	resp, _ = f.get(c, nameA)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "control: a running Machine on the same backend is reached")
}

func TestProxy_ForwardsOnATableNoLongerThanTheMaxAgeAfterHostdTookIt(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	c := f.client(true)

	at := f.received.Add(time.Hour - time.Second)
	f.now.Store(&at)
	resp, _ := f.get(c, nameA)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "control: within the max age")
	at = f.received.Add(time.Hour + time.Second)
	f.now.Store(&at)
	resp, _ = f.get(c, nameA)
	assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	assert.Equal(t, int64(1), b.hits.Load())
}

// tcpBackend is a Machine's tcp service: it records the first bytes of a connection and answers pong.
func tcpBackend(t *testing.T) (int, <-chan []byte) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	got := make(chan []byte, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 64)
		var all []byte
		for !strings.Contains(string(all), "ping") {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			all = append(all, buf[:n]...)
		}
		got <- all
		_, _ = c.Write([]byte("pong"))
	}()
	return l.Addr().(*net.TCPAddr).Port, got
}

func TestProxy_CarriesATCPServiceAsPlainTCPAfterAPROXYHeaderWhenItAsksForOne(t *testing.T) {
	for _, header := range []bool{true, false} {
		t.Run(fmt.Sprintf("header %v", header), func(t *testing.T) {
			port, got := tcpBackend(t)
			r := httpRoute(nameA, machineA, port)
			r.Protocol = hostapi.RouteTCP
			if header {
				r.ProxyProtocol = "v2"
			}
			f := newFixture(t, time.Hour, "", r)

			c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"h2", "http/1.1"}})
			require.NoError(t, err)
			defer func() { _ = c.Close() }()
			assert.Empty(t, c.ConnectionState().NegotiatedProtocol, "a tcp service takes none of the HTTP protocols a client offers")
			_, err = c.Write([]byte("ping"))
			require.NoError(t, err)
			first := <-got
			if header {
				want, err := wire.ProxyHeader(netip.MustParseAddrPort(c.LocalAddr().String()), netip.MustParseAddrPort(f.addr))
				require.NoError(t, err)
				assert.Equal(t, append(want, "ping"...), first, "the client's address, then the stream")
			} else {
				assert.Equal(t, []byte("ping"), first, "control: the stream alone")
			}
			answer := make([]byte, 4)
			_, err = io.ReadFull(c, answer)
			require.NoError(t, err)
			assert.Equal(t, "pong", string(answer))
		})
	}
}

func TestProxy_ClosesATCPServiceOfAMachineThatIsNotRunningWithoutDialling(t *testing.T) {
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
	stopped := r
	stopped.Hostname, stopped.Running = nameB, false
	f := newFixture(t, time.Hour, "", r, stopped)

	read := func(name string) error {
		c, err := f.dialTLS(&tls.Config{ServerName: name})
		require.NoError(t, err)
		defer func() { _ = c.Close() }()
		_, err = io.ReadFull(c, make([]byte, 4))
		return err
	}
	require.NoError(t, read(nameA), "control: a running Machine answers")
	require.Error(t, read(nameB))
	assert.Equal(t, int64(1), dialled.Load(), "the stopped Machine was not dialled")
}

func TestProxy_WritesThePROXYHeaderOfEachClientBeforeAnHTTPRequestWhenTheServiceAsksForOne(t *testing.T) {
	port, got := tcpBackend(t)
	r := httpRoute(nameA, machineA, port)
	r.ProxyProtocol = "v2"
	f := newFixture(t, time.Hour, "", r)

	c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nX-End: ping\r\n\r\n", nameA)
	require.NoError(t, err)
	want, err := wire.ProxyHeader(netip.MustParseAddrPort(c.LocalAddr().String()), netip.MustParseAddrPort(f.addr))
	require.NoError(t, err)
	select {
	case first := <-got:
		assert.Equal(t, want, first[:len(want)])
		assert.Contains(t, string(first[len(want):]), "GET / HTTP/1.1")
	case <-time.After(10 * time.Second):
		t.Fatal("the Machine received nothing")
	}
}

func TestProxy_UpgradesAWebSocketToTheMachine(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "websocket" {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
		line, _ := rw.ReadString('\n')
		_, _ = rw.WriteString("echo " + line)
		_ = rw.Flush()
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, l.Addr().(*net.TCPAddr).Port))

	c, err := f.dialTLS(&tls.Config{ServerName: nameA, NextProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	defer func() { _ = c.Close() }()
	_, err = fmt.Fprintf(c, "GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", nameA)
	require.NoError(t, err)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	_, err = c.Write([]byte("hello\n"))
	require.NoError(t, err)
	line, err := br.ReadString('\n')
	require.NoError(t, err)
	assert.Equal(t, "echo hello\n", line)
}

func health(t *testing.T, f *fixture) proxy.Health {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.HealthHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var h proxy.Health
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&h))
	return h
}

func revisionFile(t *testing.T, f *fixture) string {
	t.Helper()
	raw, err := os.ReadFile(f.revision())
	require.NoError(t, err)
	return strings.TrimSpace(string(raw))
}

func TestRoutes_ATableOfALowerRevisionOrOneThatDoesNotParseLeavesTheLastGoodOne(t *testing.T) {
	a, b := newBackend(t), newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, a.port))
	c := f.client(false)

	f.write(3, "", httpRoute(nameA, machineA, b.port))
	require.NoError(t, f.routes.Load())
	f.get(c, nameA)
	require.Equal(t, int64(1), b.hits.Load(), "control: a higher revision is taken")
	assert.Equal(t, "3", revisionFile(t, f))

	f.write(2, "", httpRoute(nameA, machineA, a.port))
	require.Error(t, f.routes.Load(), "a lower revision")
	bad := httpRoute(nameA, machineA, a.port)
	bad.Protocol = "udp"
	f.write(4, "", bad)
	require.Error(t, f.routes.Load(), "a route that does not parse")
	f.writeRaw([]byte(`{"revision": 5, "routes": []}`))
	require.Error(t, f.routes.Load(), "a file without the time hostd took it")
	f.writeRaw([]byte("{not json"))
	require.Error(t, f.routes.Load(), "a file that does not parse")
	require.NoError(t, os.Remove(f.path()))
	require.Error(t, f.routes.Load(), "no file")

	f.get(c, nameA)
	assert.Equal(t, int64(2), b.hits.Load(), "revision 3 still routes")
	assert.Zero(t, a.hits.Load())
	assert.Equal(t, int64(3), health(t, f).Revision)
	assert.Equal(t, "3", revisionFile(t, f))
}

func TestRoutes_AnotherPushOfTheSameRevisionIsTakenSoTheTableStaysYoung(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	c := f.client(false)

	at := f.received.Add(90 * time.Minute)
	f.now.Store(&at)
	resp, _ := f.get(c, nameA)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, "the first push is out of date")
	assert.True(t, health(t, f).Expired)
	f.received = f.received.Add(time.Hour)
	f.write(1, "", httpRoute(nameA, machineA, b.port))
	require.NoError(t, f.routes.Load())
	resp, _ = f.get(c, nameA)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.False(t, health(t, f).Expired)
}

func TestRoutes_WatchLoadsTheTableHostdWritesWithoutBeingAsked(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { f.routes.Watch(ctx, 10*time.Millisecond) })
	t.Cleanup(func() { cancel(); wg.Wait() })

	f.write(5, "", httpRoute(nameA, machineA, b.port), httpRoute(nameB, machineB, b.port))
	require.Eventually(t, func() bool { return health(t, f).Revision == 5 }, 5*time.Second, 10*time.Millisecond)
	resp, _ := f.get(f.client(true), nameB)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "5", revisionFile(t, f))
}

func TestProxy_AnswersAMachineThatDoesNotAnswer502(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := l.Addr().(*net.TCPAddr).Port
	require.NoError(t, l.Close())
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, closed), httpRoute(nameB, machineB, b.port))
	c := f.client(true)

	resp, _ := f.get(c, nameA)
	assert.Equal(t, http.StatusBadGateway, resp.StatusCode)
	resp, _ = f.get(c, nameB)
	assert.Equal(t, http.StatusOK, resp.StatusCode, "control: a Machine that answers")
}

// failingOnce is a listener whose first accept fails, as one with no file descriptor left does.
type failingOnce struct {
	net.Listener
	failed atomic.Bool
}

func (l *failingOnce) Accept() (net.Conn, error) {
	if !l.failed.Swap(true) {
		return nil, errors.New("accept: too many open files")
	}
	return l.Listener.Accept()
}

func TestProxy_KeepsAcceptingAfterAnAcceptFails(t *testing.T) {
	b := newBackend(t)
	f := newFixture(t, time.Hour, "", httpRoute(nameA, machineA, b.port))
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	l := &failingOnce{Listener: inner}
	srv := proxy.New(proxy.Config{Zone: f.pki.zone, Routes: f.routes, MaxAge: time.Hour, Now: time.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, l); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	f.addr = inner.Addr().String()

	c, err := f.dialTLS(&tls.Config{ServerName: nameA})
	require.NoError(t, err)
	_ = c.Close()
	assert.True(t, l.failed.Load(), "the accept before this connection's failed")
}
