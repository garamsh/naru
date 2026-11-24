package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// redirect answers a plain HTTP request with a redirect to https on the name it asked for, the path
// and query kept, and 308 so a client repeats the method and body (ADR 0112 item 8's port 80). A
// request without a name to redirect to is 400.
func redirect(w http.ResponseWriter, r *http.Request) {
	host := hostOf(r.Host)
	if host == "" {
		http.Error(w, "the request names no host", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
}

// ServeRedirects answers plain HTTP on each listener with a redirect to https, until ctx ends. Its
// connections count against the same limits as the TLS listeners' and close by the same idle rule.
func (s *Server) ServeRedirects(ctx context.Context, listeners ...net.Listener) error {
	srv := &http.Server{
		Handler:           http.HandlerFunc(redirect),
		ReadHeaderTimeout: handshakeTimeout,
		IdleTimeout:       s.conf.Idle,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          s.http.ErrorLog,
	}
	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func() { errs <- srv.Serve(&limitedListener{Listener: l, s: s, ctx: ctx}) }()
	}
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return nil
	case err := <-errs:
		_ = srv.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// limitedListener hands the HTTP server only the connections the proxy's limits allow, each with
// the idle rule of a client's connection. An accept that fails is retried as the TLS listeners'
// are.
type limitedListener struct {
	net.Listener
	s   *Server
	ctx context.Context
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			if l.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil, err
			}
			l.s.conf.Log.Warn("an accept failed", "listen", l.Addr().String(), "err", err)
			select {
			case <-l.ctx.Done():
				return nil, net.ErrClosed
			case <-time.After(acceptRetry):
			}
			continue
		}
		if cc, ok := l.s.admit(c); ok {
			return cc, nil
		}
	}
}
