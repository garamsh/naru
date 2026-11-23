package network

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"time"
)

// proxySignature opens every PROXY protocol v2 header.
var proxySignature = [12]byte{'\r', '\n', '\r', '\n', 0, '\r', '\n', 'Q', 'U', 'I', 'T', '\n'}

const (
	proxyVersionCommand = 0x21 // version 2, command PROXY
	proxyIPv4TCP        = 0x11 // address family AF_INET, transport STREAM
	proxyIPv4Length     = 12   // two addresses and two ports
)

// ProxyHeader returns the PROXY protocol v2 header for a TCP connection over IPv4 from src to dst:
// the signature, the version and command, the family and transport, the length, and the addresses
// and ports.
func ProxyHeader(src, dst netip.AddrPort) ([]byte, error) {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	if !s.Is4() || !d.Is4() {
		return nil, errors.New("the PROXY header carries IPv4 addresses only")
	}
	b := make([]byte, 0, 16+proxyIPv4Length)
	b = append(b, proxySignature[:]...)
	b = append(b, proxyVersionCommand, proxyIPv4TCP)
	b = binary.BigEndian.AppendUint16(b, proxyIPv4Length)
	b = append(b, s.AsSlice()...)
	b = append(b, d.AsSlice()...)
	b = binary.BigEndian.AppendUint16(b, src.Port())
	b = binary.BigEndian.AppendUint16(b, dst.Port())
	return b, nil
}

const (
	dialTimeout = 3 * time.Second
	// connectTimeout bounds the whole search for a backend that answers.
	connectTimeout = 10 * time.Second
	// DefaultIdle is how long a proxied connection may be silent in a direction before it is closed.
	DefaultIdle = 5 * time.Minute
	// maxProxyConns bounds the connections one host proxies at once, and maxPerSource those of one
	// client address, so one client cannot take every slot.
	maxProxyConns = 4096
	maxPerSource  = 128
)

// limiter bounds the connections a host proxies in all and from each client address.
type limiter struct {
	mu            sync.Mutex
	total, source int
	n             int
	by            map[netip.Addr]int
}

func newLimiter(total, source int) *limiter {
	return &limiter{total: total, source: source, by: map[netip.Addr]int{}}
}

// acquire takes a slot for src, or reports that the host or src is at its limit.
func (l *limiter) acquire(src netip.Addr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n >= l.total || l.by[src] >= l.source {
		return false
	}
	l.n++
	l.by[src]++
	return true
}

func (l *limiter) release(src netip.Addr) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.n--
	if l.by[src]--; l.by[src] <= 0 {
		delete(l.by, src)
	}
}

// idleConn closes a connection that is silent for idle in a direction: every read and write sets its
// own deadline, so a client that sends nothing, or a peer that stopped reading, holds no slot for long.
type idleConn struct {
	net.Conn
	idle time.Duration
}

func (c idleConn) Read(b []byte) (int, error) {
	_ = c.SetReadDeadline(time.Now().Add(c.idle))
	return c.Conn.Read(b)
}

func (c idleConn) Write(b []byte) (int, error) {
	_ = c.SetWriteDeadline(time.Now().Add(c.idle))
	return c.Conn.Write(b)
}

func (c idleConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// proxy accepts the connections of one listener and carries each to a backend's NodePort behind a
// PROXY protocol v2 header.
type proxy struct {
	ln       net.Listener
	nodePort int
	dial     func(ctx context.Context, addr string) (net.Conn, error)
	limit    *limiter
	idle     time.Duration
	log      *slog.Logger
	cancel   context.CancelFunc // closes the listener and every connection

	mu       sync.Mutex
	backends []netip.Addr
}

func (p *proxy) setBackends(b []netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append([]netip.Addr(nil), b...)
}

func (p *proxy) idleTime() time.Duration {
	if p.idle > 0 {
		return p.idle
	}
	return DefaultIdle
}

// serve accepts until ctx ends or the listener closes, and closes every connection it carries then.
// An accept that fails otherwise, such as for lack of file descriptors, is logged and tried again
// after a pause that grows to a second.
func (p *proxy) serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = p.ln.Close()
	}()
	var pause time.Duration
	for {
		c, err := p.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			pause = min(max(2*pause, 5*time.Millisecond), time.Second)
			if p.log != nil {
				p.log.Error("cannot accept a connection", "address", p.ln.Addr().String(), "err", err.Error())
			}
			select {
			case <-time.After(pause):
				continue
			case <-ctx.Done():
				return
			}
		}
		pause = 0
		src, err := netip.ParseAddrPort(c.RemoteAddr().String())
		if err != nil || !p.limit.acquire(src.Addr()) {
			_ = c.Close()
			continue
		}
		go func() {
			defer p.limit.release(src.Addr())
			p.carry(ctx, c)
		}()
	}
}

// carry connects client to a backend, tries the backends in random order, writes the header as the
// first bytes of the connection, and then copies both ways until both sides end or one is idle.
func (p *proxy) carry(ctx context.Context, client net.Conn) {
	defer client.Close()
	src, err1 := netip.ParseAddrPort(client.RemoteAddr().String())
	dst, err2 := netip.ParseAddrPort(client.LocalAddr().String())
	if err := errors.Join(err1, err2); err != nil {
		return
	}
	header, err := ProxyHeader(src, dst)
	if err != nil {
		return
	}
	raw := p.connect(ctx)
	if raw == nil {
		return
	}
	defer raw.Close()
	_ = raw.SetWriteDeadline(time.Now().Add(p.idleTime()))
	if _, err := raw.Write(header); err != nil {
		return
	}
	backend, clientSide := idleConn{raw, p.idleTime()}, idleConn{client, p.idleTime()}
	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = raw.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(to, from idleConn) {
		_, _ = io.Copy(to, from)
		_ = to.CloseWrite()
		done <- struct{}{}
	}
	go pipe(backend, clientSide)
	go pipe(clientSide, backend)
	<-done
	<-done
}

// connect returns a connection to one backend, or nil when none answers within connectTimeout.
func (p *proxy) connect(ctx context.Context) net.Conn {
	p.mu.Lock()
	order := rand.Perm(len(p.backends))
	backends := append([]netip.Addr(nil), p.backends...)
	p.mu.Unlock()
	ctx, stop := context.WithTimeout(ctx, connectTimeout)
	defer stop()
	for _, i := range order {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := p.dial(dctx, netip.AddrPortFrom(backends[i], uint16(p.nodePort)).String())
		cancel()
		if err == nil {
			return c
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	return nil
}
