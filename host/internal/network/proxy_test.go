package network_test

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
)

func TestProxyHeader_Bytes(t *testing.T) {
	got, err := network.ProxyHeader(netip.MustParseAddrPort("203.0.113.7:51234"), netip.MustParseAddrPort("192.168.0.80:443"))
	require.NoError(t, err)
	want := "0d0a0d0a000d0a515549540a" + // signature
		"21" + // version 2, command PROXY
		"11" + // AF_INET, STREAM
		"000c" + // 12 bytes of addresses follow
		"cb007107" + "c0a80050" + // source and destination address
		"c822" + "01bb" // source port 51234, destination port 443
	assert.Equal(t, want, hex.EncodeToString(got))

	_, err = network.ProxyHeader(netip.MustParseAddrPort("[2001:db8::1]:1"), netip.MustParseAddrPort("192.168.0.80:443"))
	assert.Error(t, err, "IPv4 only")
	mapped, err := network.ProxyHeader(netip.MustParseAddrPort("[::ffff:203.0.113.7]:51234"), netip.MustParseAddrPort("192.168.0.80:443"))
	require.NoError(t, err)
	assert.Equal(t, want, hex.EncodeToString(mapped), "control: an IPv4 address in its IPv6 form is the same header")
}

// recordedConn is the backend's end of a connection, which records each Write.
type recordedConn struct {
	net.Conn
	mu     sync.Mutex
	writes [][]byte
}

func (c *recordedConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes = append(c.writes, append([]byte(nil), b...))
	c.mu.Unlock()
	return c.Conn.Write(b)
}

func (c *recordedConn) recorded() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.writes...)
}

// proxyEnv serves the proxy of one exposure on a loopback listener, and dials its backends through dial.
type proxyEnv struct {
	*exposureEnv
	listen net.Addr
}

func startProxy(t *testing.T, dial func(ctx context.Context, addr string) (net.Conn, error)) *proxyEnv {
	t.Helper()
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	ready := make(chan net.Addr, 1)
	e.x.Listen = func(string) (net.Listener, error) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err == nil {
			ready <- ln.Addr()
		}
		return ln, err
	}
	e.x.Dial = dial
	x := web(true)
	x.Ports = []network.ExposurePort{{Protocol: "tcp", Port: 443, NodePort: 30443}}
	require.NoError(t, e.x.Put(context.Background(), x, time.Minute))
	return &proxyEnv{exposureEnv: e, listen: <-ready}
}

func TestProxy_TheHeaderIsTheFirstWriteOnTheBackendConnection(t *testing.T) {
	clientSide, backendSide := net.Pipe()
	backend := &recordedConn{Conn: clientSide}
	p := startProxy(t, func(_ context.Context, addr string) (net.Conn, error) {
		assert.Contains(t, []string{"10.8.0.5:30443", "10.8.0.6:30443"}, addr, "a backend at the NodePort")
		return backend, nil
	})

	client, err := net.Dial("tcp4", p.listen.String())
	require.NoError(t, err)
	defer client.Close()
	// The client sends its payload at once, as a TLS client does with its ClientHello.
	_, err = client.Write([]byte("CLIENT HELLO"))
	require.NoError(t, err)

	header, err := network.ProxyHeader(netip.MustParseAddrPort(client.LocalAddr().String()), netip.MustParseAddrPort(p.listen.String()))
	require.NoError(t, err)
	got := make([]byte, len(header)+len("CLIENT HELLO"))
	require.NoError(t, backendSide.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(backendSide, got)
	require.NoError(t, err)

	assert.Equal(t, header, got[:len(header)], "the header carries the client's own address and the exposed one")
	assert.Equal(t, "CLIENT HELLO", string(got[len(header):]), "the payload follows it untouched")
	writes := backend.recorded()
	require.NotEmpty(t, writes)
	assert.Equal(t, header, writes[0], "the header is the first write, alone")

	go func() { _, _ = backendSide.Write([]byte("SERVER HELLO")) }()
	reply := make([]byte, len("SERVER HELLO"))
	_, err = io.ReadFull(client, reply)
	require.NoError(t, err)
	assert.Equal(t, "SERVER HELLO", string(reply), "the reply goes back as it is, with no header")
}

func TestProxy_TriesTheNextBackendWhenOneDoesNotAnswer(t *testing.T) {
	a, b := net.Pipe()
	var mu sync.Mutex
	dialled := map[string]int{}
	p := startProxy(t, func(_ context.Context, addr string) (net.Conn, error) {
		mu.Lock()
		dialled[addr]++
		mu.Unlock()
		if addr == "10.8.0.5:30443" {
			return nil, errors.New("connection refused")
		}
		return a, nil
	})
	client, err := net.Dial("tcp4", p.listen.String())
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, b.SetReadDeadline(time.Now().Add(5*time.Second)))
	head := make([]byte, 16)
	_, err = io.ReadFull(b, head)
	require.NoError(t, err, "the working backend got the connection")
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, dialled["10.8.0.6:30443"])
}

func TestProxy_TheListenerGoesWithTheLease(t *testing.T) {
	p := startProxy(t, func(context.Context, string) (net.Conn, error) { return nil, errors.New("no backend") })
	c, err := net.Dial("tcp4", p.listen.String())
	require.NoError(t, err, "control: the listener answers while the lease is held")
	_ = c.Close()

	require.NoError(t, p.x.Delete(context.Background(), addr("192.168.0.80")))
	assert.Eventually(t, func() bool {
		c, err := net.Dial("tcp4", p.listen.String())
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	}, 5*time.Second, 10*time.Millisecond, "the listener is closed with the lease")
}

func TestProxy_ARunningConnectionEndsWithTheLease(t *testing.T) {
	a, b := net.Pipe()
	p := startProxy(t, func(context.Context, string) (net.Conn, error) { return a, nil })
	client, err := net.Dial("tcp4", p.listen.String())
	require.NoError(t, err)
	defer client.Close()
	require.NoError(t, b.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = io.ReadFull(b, make([]byte, 28))
	require.NoError(t, err)

	require.NoError(t, p.x.Delete(context.Background(), addr("192.168.0.80")))
	require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = client.Read(make([]byte, 1))
	assert.ErrorIs(t, err, io.EOF, "the connection is reset when the address is released")
}

func TestProxy_OnlyTheTCPPortsOfAProxiedExposureAreListened(t *testing.T) {
	c, link := &clock{t: t0}, newLink()
	e := startExposer(t, "", link, c)
	var opened []string
	var mu sync.Mutex
	e.x.Listen = func(a string) (net.Listener, error) {
		mu.Lock()
		opened = append(opened, a)
		mu.Unlock()
		return net.Listen("tcp4", "127.0.0.1:0")
	}
	require.NoError(t, e.x.Put(context.Background(), web(false), time.Minute))
	mu.Lock()
	assert.Empty(t, opened, "control: without the proxy there is no listener")
	mu.Unlock()

	x := web(true)
	require.NoError(t, e.x.Put(context.Background(), x, time.Minute))
	mu.Lock()
	assert.Equal(t, []string{"192.168.0.80:80"}, opened, "the TCP port is listened on at the exposed address, the UDP one is not")
	mu.Unlock()
}
