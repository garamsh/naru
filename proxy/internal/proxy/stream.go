package proxy

import (
	"context"
	"crypto/tls"
	"errors"
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
	var wg sync.WaitGroup
	wg.Go(func() { pipe(back, c) })
	pipe(c, back)
	wg.Wait()
}

// halfCloser is a connection whose sending side closes alone.
type halfCloser interface {
	net.Conn
	CloseWrite() error
}

// pipe copies from src to dst until src ends or is silent for idleTimeout, then closes dst's
// sending side, so the other direction runs on until it ends too.
func pipe(dst, src halfCloser) {
	buf := make([]byte, 32<<10)
	for {
		_ = src.SetReadDeadline(time.Now().Add(idleTimeout))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				_ = src.Close()
			}
			break
		}
	}
	_ = dst.CloseWrite()
}
