package proxy

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// limiter bounds the client connections the proxy holds in all and from each client address, so
// one client cannot take every descriptor the proxy has.
type limiter struct {
	mu            sync.Mutex
	total, source int
	n             int
	by            map[netip.Addr]int
}

func newLimiter(total, source int) *limiter {
	return &limiter{total: total, source: source, by: map[netip.Addr]int{}}
}

// acquire takes a slot for src, or reports that the proxy or src is at its limit.
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

// clientConn is a client's connection under its TLS. It holds the connection's slot until it is
// closed, and it is idle only when it has carried nothing in either direction for idle: a read
// whose deadline passes while a write went out lately waits on, so a client that only receives
// keeps its connection. A deadline the layer above sets, TLS's or the HTTP server's, still ends a
// read or a write at its time.
type clientConn struct {
	net.Conn
	idle    time.Duration
	last    atomic.Int64
	release func()
	once    sync.Once

	mu sync.Mutex
	// rd and wd are the deadlines the layer above set, zero for none.
	rd, wd time.Time
}

func newClientConn(c net.Conn, idle time.Duration, release func()) *clientConn {
	cc := &clientConn{Conn: c, idle: idle, release: release}
	cc.touch()
	return cc
}

func (c *clientConn) touch() { c.last.Store(time.Now().UnixNano()) }

// idleAt is when the connection is idle if it carries nothing before then.
func (c *clientConn) idleAt() time.Time { return time.Unix(0, c.last.Load()).Add(c.idle) }

// earlier is the earlier of a deadline the layer above set, zero for none, and t.
func earlier(set, t time.Time) time.Time {
	if !set.IsZero() && set.Before(t) {
		return set
	}
	return t
}

func (c *clientConn) Read(b []byte) (int, error) {
	for {
		c.mu.Lock()
		rd := c.rd
		_ = c.Conn.SetReadDeadline(earlier(rd, c.idleAt()))
		c.mu.Unlock()
		n, err := c.Conn.Read(b)
		if n > 0 {
			c.touch()
		}
		if n > 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
			return n, err
		}
		c.mu.Lock()
		rd = c.rd
		c.mu.Unlock()
		if now := time.Now(); (rd.IsZero() || now.Before(rd)) && now.Before(c.idleAt()) {
			continue
		}
		return n, err
	}
}

func (c *clientConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	_ = c.Conn.SetWriteDeadline(earlier(c.wd, time.Now().Add(c.idle)))
	c.mu.Unlock()
	n, err := c.Conn.Write(b)
	if n > 0 {
		c.touch()
	}
	return n, err
}

func (c *clientConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rd = t
	return c.Conn.SetReadDeadline(earlier(t, c.idleAt()))
}

func (c *clientConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wd = t
	return c.Conn.SetWriteDeadline(earlier(t, time.Now().Add(c.idle)))
}

func (c *clientConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.SetWriteDeadline(t))
}

// CloseWrite closes the sending side, which TLS's CloseWrite reaches after its close_notify.
func (c *clientConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

// Close closes the connection and gives back its slot, once.
func (c *clientConn) Close() error {
	c.once.Do(c.release)
	return c.Conn.Close()
}
