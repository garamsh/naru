// Package proxy is naru-proxy: it terminates TLS for Machines' stable names and aliases, routes
// each connection or request by the table hostd writes, and forwards it to the Machine over the
// host mesh (docs/architecture/proxy.md, ADR 0112 item 4).
package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"time"

	"github.com/garamsh/naru/lib/hostapi"
)

const (
	// handshakeTimeout bounds a client's TLS handshake.
	handshakeTimeout = 10 * time.Second
	// dialTimeout bounds a connection to a Machine.
	dialTimeout = 5 * time.Second
	// idleTimeout closes a tcp service's connection silent for this long in a direction, and an
	// http connection with no request for this long.
	idleTimeout = 5 * time.Minute
	// retryAfter is what a client is told to wait for a Machine that is not running.
	retryAfter = "5"
)

// Config is what a Server is made from.
type Config struct {
	// Zone is the zone's wildcard certificate and key, which serves every name an alias's own
	// certificate does not.
	Zone tls.Certificate
	// Routes is the table the proxy forwards on.
	Routes *Routes
	// MaxAge is how long after hostd took a table the proxy forwards on it (--route-max-age).
	MaxAge time.Duration
	// Now is the clock the table's age is read by.
	Now func() time.Time
	Log *slog.Logger
}

// Server serves the proxy's listeners.
type Server struct {
	conf  Config
	http  *http.Server
	conns *connQueue
	// leaves holds the certificate each http connection's handshake chose until the HTTP server
	// takes the connection, so a request's Host is checked against that certificate.
	leaves sync.Map
	plain  *httputil.ReverseProxy
	// withHeader carries a service with the PROXY protocol, whose header names one client: it keeps
	// no connection to the Machine for another request.
	withHeader *httputil.ReverseProxy
}

// errUnknownName refuses a handshake for a name the proxy has no route or certificate for.
var errUnknownName = errors.New("no route for this name")

// New returns a server for conf.
func New(conf Config) *Server {
	s := &Server{conf: conf, conns: newConnQueue()}
	s.plain = s.reverseProxy(false)
	s.withHeader = s.reverseProxy(true)
	s.http = &http.Server{
		Handler:           s,
		ReadHeaderTimeout: handshakeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(conf.Log.Handler(), slog.LevelDebug),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			leaf, ok := s.leaves.LoadAndDelete(c)
			if !ok {
				return ctx
			}
			return context.WithValue(ctx, leafKey{}, leaf)
		},
	}
	return s
}

// Serve accepts on each listener until ctx ends, then closes them and the connections the HTTP
// server holds.
func (s *Server) Serve(ctx context.Context, listeners ...net.Listener) error {
	errs := make(chan error, len(listeners)+1)
	go func() { errs <- s.http.Serve(s.conns) }()
	for _, l := range listeners {
		go func() { errs <- s.accept(ctx, l) }()
	}
	select {
	case <-ctx.Done():
	case err := <-errs:
		s.shutdown(listeners)
		return err
	}
	s.shutdown(listeners)
	return nil
}

func (s *Server) shutdown(listeners []net.Listener) {
	for _, l := range listeners {
		_ = l.Close()
	}
	_ = s.conns.Close()
	_ = s.http.Close()
}

func (s *Server) accept(ctx context.Context, l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

// leafKey is the context key of the certificate an http connection's handshake chose.
type leafKey struct{}

// handle completes the TLS handshake of a client, choosing the certificate by the SNI, and passes
// the connection on by its route's protocol: a tcp service's to the Machine, an http one's to the
// HTTP server.
func (s *Server) handle(ctx context.Context, raw net.Conn) {
	var chosen route
	var leaf *x509.Certificate
	conf := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			r, ok := s.conf.Routes.table().lookup(hello.ServerName)
			if !ok {
				return nil, errUnknownName
			}
			cert := r.cert
			if cert == nil {
				if s.conf.Zone.Leaf.VerifyHostname(r.Hostname) != nil {
					return nil, errUnknownName
				}
				cert = &s.conf.Zone
			}
			chosen, leaf = r, cert.Leaf
			c := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{*cert}}
			if r.Protocol == hostapi.RouteHTTP {
				c.NextProtos = []string{"h2", "http/1.1"}
			}
			return c, nil
		},
	}
	c := tls.Server(raw, conf)
	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := c.HandshakeContext(hctx); err != nil {
		s.conf.Log.Debug("refused a handshake", "remote", raw.RemoteAddr().String(), "err", err)
		_ = c.Close()
		return
	}
	if chosen.Protocol == hostapi.RouteTCP {
		s.stream(ctx, c, chosen)
		return
	}
	s.leaves.Store(c, leaf)
	if !s.conns.push(c) {
		s.leaves.Delete(c)
		_ = c.Close()
	}
}

// dial connects to a Machine from the host's mesh address, which the table carries.
func (s *Server) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	if t := s.conf.Routes.table(); t != nil && t.source.IsValid() {
		d.LocalAddr = &net.TCPAddr{IP: t.source.AsSlice()}
	}
	return d.DialContext(ctx, network, address)
}

// connQueue is the listener the HTTP server accepts from: the http connections whose handshake
// handle completed.
type connQueue struct {
	ch   chan net.Conn
	done chan struct{}
	once sync.Once
}

func newConnQueue() *connQueue {
	return &connQueue{ch: make(chan net.Conn), done: make(chan struct{})}
}

func (q *connQueue) push(c net.Conn) bool {
	select {
	case q.ch <- c:
		return true
	case <-q.done:
		return false
	}
}

func (q *connQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.done:
		return nil, net.ErrClosed
	}
}

func (q *connQueue) Close() error {
	q.once.Do(func() { close(q.done) })
	return nil
}

func (q *connQueue) Addr() net.Addr { return &net.TCPAddr{} }
