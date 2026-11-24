package proxy

import (
	"context"
	"errors"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// targetKey is the context key of the target a request goes to.
type targetKey struct{}

// target is where a request goes, and the addresses of the client's connection, which a PROXY
// header names.
type target struct {
	route         route
	client, local netip.AddrPort
}

// ServeHTTP routes an http service's request by its own Host, which must be a name the
// connection's certificate covers and that has an http route, else 421, so a client that reused
// one connection for another name retries on a new one (ADR 0112 item 4).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	leaf, ok := r.Context().Value(leafKey{}).(*x509.Certificate)
	if !ok {
		http.Error(w, "the connection has no certificate", http.StatusMisdirectedRequest)
		return
	}
	host := hostOf(r.Host)
	t := s.conf.Routes.table()
	rt, ok := t.lookup(host)
	if !ok || rt.Protocol != hostapi.RouteHTTP || leaf.VerifyHostname(host) != nil {
		http.Error(w, "this connection does not serve "+host, http.StatusMisdirectedRequest)
		return
	}
	if t.expired(s.conf.Now(), s.conf.MaxAge) {
		http.Error(w, "the proxy's routes are out of date", http.StatusServiceUnavailable)
		return
	}
	if !rt.Running {
		w.Header().Set("Retry-After", retryAfter)
		http.Error(w, "the Machine is not running", http.StatusServiceUnavailable)
		return
	}
	to := target{route: rt}
	to.client, _ = netip.ParseAddrPort(r.RemoteAddr)
	if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		to.local, _ = netip.ParseAddrPort(a.String())
	}
	r = r.WithContext(context.WithValue(r.Context(), targetKey{}, to))
	if rt.ProxyProtocol == "v2" {
		s.withHeader.ServeHTTP(w, r)
		return
	}
	s.plain.ServeHTTP(w, r)
}

// hostOf returns the name of a Host header or :authority, without its port.
func hostOf(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

// reverseProxy returns the proxy of http requests to Machines. With header, each connection to a
// Machine starts with the PROXY protocol v2 header of the client it carries, and is used for that
// client's request alone.
func (s *Server) reverseProxy(header bool) *httputil.ReverseProxy {
	transport := &http.Transport{
		DialContext:         s.dial,
		IdleConnTimeout:     idleTimeout,
		MaxIdleConnsPerHost: 16,
		DisableKeepAlives:   header,
	}
	if header {
		transport.DialContext = s.dialWithHeader
	}
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			to, ok := pr.In.Context().Value(targetKey{}).(target)
			if !ok {
				panic("proxy: a request reached the reverse proxy without a target")
			}
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = net.JoinHostPort(to.route.Address.String(), strconv.Itoa(to.route.Port))
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.conf.Log.Warn("could not reach the Machine", "host", hostOf(r.Host), "err", err)
			http.Error(w, "the Machine did not answer", http.StatusBadGateway)
		},
	}
}

// dialWithHeader connects to a Machine and writes the PROXY protocol v2 header of the client whose
// request the context carries.
func (s *Server) dialWithHeader(ctx context.Context, network, address string) (net.Conn, error) {
	header, err := clientHeader(ctx)
	if err != nil {
		return nil, err
	}
	c, err := s.dial(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write(header); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// clientHeader is the PROXY header of the request in ctx: from the client's address to the
// address it reached.
func clientHeader(ctx context.Context) ([]byte, error) {
	to, ok := ctx.Value(targetKey{}).(target)
	if !ok {
		return nil, errors.New("no client to write a PROXY header for")
	}
	return wire.ProxyHeader(to.client, to.local)
}
