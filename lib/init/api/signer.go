package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"path"
	"strings"
	"time"
)

const (
	// signerPrefix is the route that forwards to the signer: /v1/signer/<path> reaches the signer's /v1/<path>.
	signerPrefix = "/v1/signer"
	// signerHost is the Host the forwarded request carries, the name naru-signer answers to.
	signerHost = "naru-signer"
	// signerMaxBody is the largest request body the signer reads; a larger one is refused here.
	signerMaxBody = 1 << 20
	// signerRequestHeader carries a signed request on the signer's GET /v1/bundle.
	signerRequestHeader = "X-Naru-Request"
)

// forSigner reports whether r is for the signer route.
func forSigner(r *http.Request) bool {
	return r.URL.Path == signerPrefix || strings.HasPrefix(r.URL.Path, signerPrefix+"/")
}

// newSignerProxy returns the handler that forwards a request to the unix socket of the signer,
// whatever the method. It signs nothing and adds no header: the signer authenticates each request
// by the signature it carries, and only the request's Content-Type and signed-request header
// reach it.
func (s *Server) newSignerProxy() http.Handler {
	return &httputil.ReverseProxy{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", s.deps.SignerSocket)
			},
			ResponseHeaderTimeout: 30 * time.Second,
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host, pr.Out.Host = "http", signerHost, signerHost
			pr.Out.URL.Path, pr.Out.URL.RawPath = "/v1/"+pr.In.PathValue("path"), ""
			kept := http.Header{}
			for _, name := range []string{"Content-Type", signerRequestHeader} {
				if v := pr.In.Header.Values(name); len(v) > 0 {
					kept[http.CanonicalHeaderKey(name)] = v
				}
			}
			pr.Out.Header = kept
		},
		ErrorHandler: s.signerFailed,
	}
}

// signer forwards the request to the signer of this Node. A Node that runs none answers 404.
func (s *Server) signer(w http.ResponseWriter, r *http.Request) {
	if _, cfg := s.config(); cfg.Spec.Signer == nil {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "this Node runs no signer: its configuration has no spec.signer"})
		return
	}
	if p := r.PathValue("path"); path.Clean("/"+p) != "/"+p {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: "the signer path is not a clean path"})
		return
	}
	if r.ContentLength > signerMaxBody {
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "the request body is larger than the signer reads"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, signerMaxBody)
	s.signerProxy.ServeHTTP(w, r)
}

// signerFailed answers a request the signer did not answer: 413 for a body over the bound, 503
// when its socket cannot be dialled, and 502 for any other failure of the exchange.
func (s *Server) signerFailed(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	var op *net.OpError
	switch {
	case errors.As(err, &tooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: "the request body is larger than the signer reads"})
	case errors.As(err, &op) && op.Op == "dial":
		s.deps.Log.Warn("the signer's socket cannot be dialled", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the signer cannot be reached; see the Node's log"})
	default:
		s.deps.Log.Warn("the signer's exchange failed", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusBadGateway, errorBody{Error: "the signer's exchange failed; see the Node's log"})
	}
}
