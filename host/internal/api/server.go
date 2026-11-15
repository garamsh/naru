// Package api is the host API of naru-hostd (docs/architecture/host.md): HTTPS and JSON over
// mutual TLS, TLS 1.3 only, with the role of a client taken from its certificate's Organization as
// the Node API does (ADR 0017), and mvmd behind it.
package api

import (
	"context"
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
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
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
	// Networks holds the host's virtual networks and the tunnel's peers. Nil means the host was
	// started without them: the calls that need them are 501, and a microVM is created on none.
	Networks Networks
	Log      *slog.Logger
	// Metrics is the registry the host API's metrics are registered on, made once by the composition site.
	Metrics prometheus.Registerer
	// Revocations is the list the listener checks clients against; its age and last fetch error are
	// exposed as metrics. Nil means no revocation is checked.
	Revocations *identity.RevocationList
	// LeaderTokenFile keeps the highest leader token orch has sent (ADR 0071). Empty keeps it in
	// memory only.
	LeaderTokenFile string
}

// Server is the host API.
type Server struct {
	deps     Deps
	requests *prometheus.CounterVec
	tokens   leaderTokens
}

// New returns a Server, whose metrics are registered on deps.Metrics.
func New(deps Deps) *Server {
	return &Server{deps: deps, requests: newMetrics(deps.Metrics, deps.Runtime, deps.Revocations, deps.Log), tokens: leaderTokens{path: deps.LeaderTokenFile}}
}

// Handler returns the routes behind the middleware: a panic is answered with 500, each request is
// logged once, and the client's role is checked before any route runs. A request that is not a GET
// needs the provisioner role, and a GET the reader's, which a provisioner has too. A change that
// carries a leader token is refused when the token is lower than the highest seen.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/host", s.getHost)
	mux.HandleFunc("POST /v1/microvms", s.createMicroVM)
	mux.HandleFunc("GET /v1/microvms", s.listMicroVMs)
	mux.HandleFunc("GET /v1/microvms/{name}", s.getMicroVM)
	mux.HandleFunc("DELETE /v1/microvms/{name}", s.deleteMicroVM)
	mux.HandleFunc("POST /v1/microvms/{name}/stop", s.stopMicroVM)
	mux.HandleFunc("POST /v1/microvms/{name}/start", s.startMicroVM)
	mux.HandleFunc("POST /v1/microvms/{name}/fence", s.renewFence)
	mux.HandleFunc("GET /v1/microvms/{name}/fence", s.getFence)
	mux.HandleFunc("POST /v1/microvms/{name}/snapshot", s.snapshotMicroVM)
	mux.HandleFunc("GET /v1/snapshots", s.listSnapshots)
	mux.HandleFunc("GET /v1/snapshots/{name}", s.getSnapshot)
	mux.HandleFunc("DELETE /v1/snapshots/{name}", s.deleteSnapshot)
	mux.HandleFunc("POST /v1/snapshots/{name}/resume", s.resumeSnapshot)
	mux.HandleFunc("PUT /v1/networks/{name}", s.putNetwork)
	mux.HandleFunc("GET /v1/networks", s.listNetworks)
	mux.HandleFunc("GET /v1/networks/{name}", s.getNetwork)
	mux.HandleFunc("DELETE /v1/networks/{name}", s.deleteNetwork)
	mux.HandleFunc("PUT /v1/peers", s.putPeers)
	mux.HandleFunc("GET /v1/peers", s.getPeers)
	mux.HandleFunc("GET /v1/tunnel", s.getTunnel)
	s.routeBootSets(mux)
	return s.recovering(s.logged(s.authorised(s.leaderToken(mux))))
}

// authorised answers 403 to a client whose role does not cover the method.
func (s *Server) authorised(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := Reader
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			need = Provisioner
		}
		if roleOf(clientCertificate(r)) < need {
			writeJSON(w, http.StatusForbidden, hostapi.ErrorBody{Error: "the client certificate's role does not allow this request"})
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
				writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "internal error"})
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// TLSConfig returns the configuration of the listener: TLS 1.3 only, the serving pair at
// certFile and keyFile, and a client certificate required and verified against the client CA at
// clientCAFile. When revocations is not nil, a certificate on the signer's list is refused at the
// handshake, and so is every client while the list is older than its max age or was never had.
func TLSConfig(certFile, keyFile, clientCAFile string, revocations *identity.RevocationList) (*tls.Config, error) {
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
	conf := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"},
	}
	if revocations != nil {
		conf.VerifyConnection = revocations.VerifyConnection
	}
	return conf, nil
}

// DrainTimeout is how long Serve waits for the requests in flight after its context ends, before
// it closes their connections. A create or a delete may take up to callTimeout, so a stop signal
// that is followed by a kill leaves one that the drain could not finish.
const DrainTimeout = 30 * time.Second

// Serve serves the API on ln until ctx ends, and then drains: it closes ln, so a new connection is
// refused, lets the requests in flight finish for up to DrainTimeout and returns nil. An error of
// the listener before that is returned. A refused handshake is not logged: it would fill the
// console for anyone who can reach the port.
func (s *Server) Serve(ctx context.Context, ln net.Listener, conf *tls.Config) error {
	srv := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second, TLSConfig: conf,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		drain, cancel := context.WithTimeout(context.Background(), DrainTimeout)
		defer cancel()
		err := srv.Shutdown(drain)
		if err != nil {
			_ = srv.Close()
		}
		done <- err
	}()
	if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}
