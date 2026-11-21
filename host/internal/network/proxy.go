package network

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
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
	// maxProxyConns bounds the connections one host proxies at once.
	maxProxyConns = 4096
)

// proxy accepts the connections of one listener and carries each to a backend's NodePort behind a
// PROXY protocol v2 header.
type proxy struct {
	ln       net.Listener
	nodePort int
	dial     func(ctx context.Context, addr string) (net.Conn, error)
	slots    chan struct{}
	cancel   context.CancelFunc // closes the listener and every connection

	mu       sync.Mutex
	backends []netip.Addr
}

func (p *proxy) setBackends(b []netip.Addr) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.backends = append([]netip.Addr(nil), b...)
}

// serve accepts until ctx ends or the listener closes, and closes every connection it carries then.
func (p *proxy) serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		_ = p.ln.Close()
	}()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		select {
		case p.slots <- struct{}{}:
		default:
			_ = c.Close()
			continue
		}
		go func() {
			defer func() { <-p.slots }()
			p.carry(ctx, c)
		}()
	}
}

// carry connects client to a backend, tries the backends in random order, writes the header as the
// first bytes of the connection, and then copies both ways.
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
	backend := p.connect(ctx)
	if backend == nil {
		return
	}
	defer backend.Close()
	if _, err := backend.Write(header); err != nil {
		return
	}
	stop := context.AfterFunc(ctx, func() { _ = client.Close(); _ = backend.Close() })
	defer stop()
	done := make(chan struct{}, 2)
	pipe := func(to, from net.Conn) {
		_, _ = io.Copy(to, from)
		if cw, ok := to.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}
	go pipe(backend, client)
	go pipe(client, backend)
	<-done
	<-done
}

// connect returns a connection to one backend, or nil when none answers.
func (p *proxy) connect(ctx context.Context) net.Conn {
	p.mu.Lock()
	order := rand.Perm(len(p.backends))
	backends := append([]netip.Addr(nil), p.backends...)
	p.mu.Unlock()
	for _, i := range order {
		dctx, cancel := context.WithTimeout(ctx, dialTimeout)
		c, err := p.dial(dctx, netip.AddrPortFrom(backends[i], uint16(p.nodePort)).String())
		cancel()
		if err == nil {
			return c
		}
	}
	return nil
}
