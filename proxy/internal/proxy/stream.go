package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/wire"
)

// stream carries a tcp service's connection, whose TLS the proxy has terminated, to the Machine as
// plain TCP, after a PROXY protocol v2 header when the service asks for one. A Machine that is not
// running, or a table out of date, closes the connection without dialling.
func (s *Server) stream(ctx context.Context, c *tls.Conn, rt route) {
	defer func() { _ = c.Close() }()
	if !rt.Running || s.conf.Routes.table().expired(s.conf.Now(), s.conf.MaxAge) {
		return
	}
	conn, err := s.dial(ctx, "tcp", net.JoinHostPort(rt.Address.String(), strconv.Itoa(rt.Port)))
	if err != nil {
		s.conf.Log.Warn("could not reach the Machine", "host", rt.Hostname, "err", err)
		return
	}
	defer func() { _ = conn.Close() }()
	back, ok := conn.(halfCloser)
	if !ok {
		panic("proxy: a TCP connection that cannot close its sending side")
	}
	if rt.ProxyProtocol == "v2" {
		src, _ := netip.ParseAddrPort(c.RemoteAddr().String())
		dst, _ := netip.ParseAddrPort(c.LocalAddr().String())
		header, err := wire.ProxyHeader(src, dst)
		if err != nil {
			s.conf.Log.Warn("could not write the PROXY header", "host", rt.Hostname, "err", err)
			return
		}
		if _, err := back.Write(header); err != nil {
			return
		}
	}
	cc, ok := c.NetConn().(*clientConn)
	if !ok {
		panic("proxy: a client connection not taken by accept")
	}
	// Each side ends at its end of stream, closing the other's sending side; a connection idle in
	// both directions, or a failure, closes both, so neither copy waits on.
	var wg sync.WaitGroup
	wg.Go(func() {
		if !pipe(back, c, nil) {
			_ = c.Close()
			_ = back.Close()
		}
	})
	if !pipe(c, back, cc.idleAt) {
		_ = c.Close()
		_ = back.Close()
	}
	wg.Wait()
}

// halfCloser is a connection whose sending side closes alone.
type halfCloser interface {
	net.Conn
	CloseWrite() error
}

// pipe copies from src to dst until src ends, then closes dst's sending side and reports true. It
// reports false when a read or write fails, or when src is silent past idleAt: the client's
// connection keeps its own idle clock, which counts what the other side writes to it, so pipe
// passes nil for it and the time the client's connection goes idle for the Machine's.
func pipe(dst, src halfCloser, idleAt func() time.Time) bool {
	buf := make([]byte, 32<<10)
	for {
		if idleAt != nil {
			_ = src.SetReadDeadline(idleAt())
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return false
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, io.EOF):
			_ = dst.CloseWrite()
			return true
		case idleAt != nil && errors.Is(err, os.ErrDeadlineExceeded) && time.Now().Before(idleAt()):
		default:
			return false
		}
	}
}
