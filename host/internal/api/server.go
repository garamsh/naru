// Package api is the host API of naru-hostd (docs/architecture/host.md): HTTPS and JSON over
// mutual TLS, TLS 1.3 only, with the role of a client taken from its certificate's Organization as
// the Node API does (ADR 0017), and mvmd behind it.
package api

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// Deps is what the server reads and acts through.
type Deps struct {
	Runtime mvmd.Runtime
	// HostFile is the file init writes from spec.host: the region and labels /v1/host reports. It is
	// read on every request, so an apply that changes spec.host shows with no restart.
	HostFile string
	// MachineGateway is the gateway of the Machines' network, an address of the prefix mvmd gives
	// their addresses from. mvmd's API does not report it, and an OS-form microVM's seed names it.
	// Invalid means unset, and an OS-form request is then refused.
	MachineGateway netip.Addr
	Log            *slog.Logger
	// Metrics is the registry the host API's metrics are registered on, made once by the composition site.
	Metrics prometheus.Registerer
}

// Server is the host API.
type Server struct {
	deps     Deps
	requests *prometheus.CounterVec
}

// New returns a Server, whose metrics are registered on deps.Metrics.
func New(deps Deps) *Server {
	return &Server{deps: deps, requests: newMetrics(deps.Metrics, deps.Runtime, deps.Log)}
}

// Handler returns the routes behind the middleware: a panic is answered with 500, each request is
// logged once, and the client's role is checked before any route runs. A request that is not a GET
// needs the provisioner role, and a GET the reader's, which a provisioner has too.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/host", s.getHost)
	mux.HandleFunc("POST /v1/microvms", s.createMicroVM)
	mux.HandleFunc("GET /v1/microvms", s.listMicroVMs)
	mux.HandleFunc("GET /v1/microvms/{name}", s.getMicroVM)
	mux.HandleFunc("DELETE /v1/microvms/{name}", s.deleteMicroVM)
	mux.HandleFunc("POST /v1/microvms/{name}/stop", s.stopMicroVM)
	mux.HandleFunc("POST /v1/microvms/{name}/start", s.startMicroVM)
	return s.recovering(s.logged(s.authorised(mux)))
}

// authorised answers 403 to a client whose role does not cover the method.
func (s *Server) authorised(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := Reader
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			need = Provisioner
		}
		if roleOf(clientCertificate(r)) < need {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "the client certificate's role does not allow this request"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recovering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.deps.Log.Error("host API handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logged logs each request when it ends: method, path, status, the client's subject and a trace_id
// made for it, and counts it by route and status. Bodies are never logged.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			s.observe(r.Pattern, rec.status)
			s.deps.Log.Info("host API request", "trace_id", traceID(), "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "subject", clientCertificate(r).Subject.String(), "duration", time.Since(start).String())
		}()
		next.ServeHTTP(rec, r)
	})
}

func traceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// errorBody is the body of every refusal and failure.
type errorBody struct {
	Error  string   `json:"error"`
	Fields []string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// TLSConfig returns the configuration of the listener: TLS 1.3 only, the serving pair at
// certFile and keyFile, and a client certificate required and verified against the client CA at
// clientCAFile.
func TLSConfig(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(clientCAFile) //nolint:gosec // a path of the Node's own configuration
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New(clientCAFile + " holds no certificate")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"},
	}, nil
}

// Serve serves the API on ln until it is closed. A refused handshake is not logged: it would fill
// the console for anyone who can reach the port.
func (s *Server) Serve(ln net.Listener, conf *tls.Config) error {
	srv := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, TLSConfig: conf,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return srv.ServeTLS(ln, "", "")
}
