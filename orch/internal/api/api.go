// Package api is naru-orchd's HTTPS API. It authenticates the caller's certificate, checks the
// role against the endpoint's action, calls the services and translates every failure to a status
// once, in writeError.
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
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// maxBody bounds a request body.
const maxBody = 1 << 20

// Snapshots takes a sealed etcd snapshot now.
type Snapshots interface {
	Take(ctx context.Context) (orchapi.SnapshotResult, error)
}

// Deps is what the handlers call.
type Deps struct {
	Inventory inventory.Service
	Signer    inventory.Signer
	Auth      auth.Authenticator
	Snapshots Snapshots
	Log       *slog.Logger
}

type handler struct {
	Deps
}

type ctxKey struct{}

// New returns the API's handler.
func New(d Deps) http.Handler {
	h := &handler{d}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/hosts", h.guard(auth.Read, h.hosts))
	mux.HandleFunc("GET /v1/pending", h.guard(auth.Read, h.pending))
	mux.HandleFunc("GET /v1/events", h.guard(auth.Read, h.events))
	mux.HandleFunc("GET /v1/crl", h.guard(auth.Any, h.crl))
	mux.HandleFunc("POST /v1/admit", h.guard(auth.Operate, h.admit))
	mux.HandleFunc("POST /v1/enrollment-keys", h.guard(auth.Operate, h.createKey))
	mux.HandleFunc("DELETE /v1/enrollment-keys/{id}", h.guard(auth.Operate, h.revokeKey))
	mux.HandleFunc("PUT /v1/hosts/{id}/labels", h.guard(auth.Operate, h.labels))
	mux.HandleFunc("POST /v1/snapshot", h.guard(auth.Administer, h.snapshot))
	mux.HandleFunc("POST /v1/heartbeat", h.guard(auth.Report, h.heartbeat))
	// Enrollment is the one path without a client certificate: a key, or a proof of one, authenticates.
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	mux.HandleFunc("GET /v1/enroll/{hostID}", h.poll)
	return h.middleware(mux)
}

// TLSConfig is the server's configuration: the client's certificate is verified against clientCA
// when it sends one, and the handlers refuse a request that needs one and has none.
func TLSConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    clientCA,
	}
}

// middleware recovers a panic as a 500 and logs each request with a trace id.
func (h *handler) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var id [8]byte
		_, _ = rand.Read(id[:])
		trace := hex.EncodeToString(id[:])
		log := h.Log.With("trace_id", trace, "method", r.Method, "path", r.URL.Path)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, log))
		w.Header().Set("X-Trace-Id", trace)
		defer func() {
			if v := recover(); v != nil {
				log.Error("handler panicked", "panic", v)
				writeJSON(w, http.StatusInternalServerError, orchapi.Error{Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func logger(r *http.Request) *slog.Logger { return r.Context().Value(ctxKey{}).(*slog.Logger) }

type authed func(w http.ResponseWriter, r *http.Request, who identity.Identity)

// guard authenticates the certificate and checks the endpoint's action.
func (h *handler) guard(a auth.Action, fn authed) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			writeJSON(w, http.StatusUnauthorized, orchapi.Error{Message: "a client certificate is required"})
			return
		}
		who, err := h.Auth.Authenticate(r.Context(), r.TLS.VerifiedChains)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if !auth.Allowed(who, a) {
			logger(r).Warn("refused: role may not do this", "subject", who.Subject, "role", string(who.Role))
			writeJSON(w, http.StatusForbidden, orchapi.Error{Message: "the role may not do this"})
			return
		}
		fn(w, r, who)
	}
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, orchapi.Error{Message: "the request body is not valid"})
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, orchapi.Error{Message: "the request body holds more than one value"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the one place an error becomes a status. What is not a known failure is logged
// and answered as a 500 without its detail.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "internal error"
	switch {
	case errors.Is(err, identity.ErrRevoked), errors.Is(err, identity.ErrRole), errors.Is(err, identity.ErrNoChain):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, auth.ErrNoRevocationList):
		status, msg = http.StatusServiceUnavailable, "the revocation list is not available"
		logger(r).Error("no revocation list", "err", err.Error())
	case errors.Is(err, inventory.ErrRateLimited):
		status, msg = http.StatusTooManyRequests, err.Error()
	case errors.Is(err, inventory.ErrKeyRefused):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, inventory.ErrCodeMismatch), errors.Is(err, inventory.ErrSignerRefused):
		status, msg = http.StatusForbidden, err.Error()
	case errors.Is(err, inventory.ErrNotFound):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, inventory.ErrConflict), errors.Is(err, inventory.ErrTooManyPending):
		status, msg = http.StatusConflict, err.Error()
	case errors.Is(err, inventory.ErrInvalid), errors.Is(err, orchapi.ErrBadProof):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, inventory.ErrSignerFailed):
		status, msg = http.StatusBadGateway, "the signer failed"
		logger(r).Error("signer failed", "err", err.Error())
	default:
		logger(r).Error("request failed", "err", err.Error())
	}
	writeJSON(w, status, orchapi.Error{Message: msg})
}

func (h *handler) hosts(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Hosts(r.Context())
	reply(w, r, out, err)
}

func (h *handler) pending(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Pending(r.Context())
	reply(w, r, out, err)
}

func (h *handler) events(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Inventory.Events(r.Context())
	reply(w, r, out, err)
}

func (h *handler) crl(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Signer.CRL(r.Context())
	reply(w, r, out, err)
}

func (h *handler) admit(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.AdmitRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.Admit(r.Context(), who.Subject, req)
	reply(w, r, out, err)
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.CreateKeyRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.CreateKey(r.Context(), who.Subject, req.ExpiresAt)
	reply(w, r, out, err)
}

func (h *handler) revokeKey(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Inventory.RevokeKey(r.Context(), who.Subject, r.PathValue("id")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) labels(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var req orchapi.SetLabelsRequest
	if !decode(w, r, &req) {
		return
	}
	out, err := h.Inventory.SetLabels(r.Context(), who.Subject, r.PathValue("id"), req)
	reply(w, r, out, err)
}

func (h *handler) snapshot(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Snapshots.Take(r.Context())
	reply(w, r, out, err)
}

func (h *handler) heartbeat(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Inventory.Heartbeat(r.Context(), who.Subject); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) enroll(w http.ResponseWriter, r *http.Request) {
	var req orchapi.EnrollRequest
	if !decode(w, r, &req) {
		return
	}
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		src = r.RemoteAddr
	}
	out, err := h.Inventory.Enroll(r.Context(), src, req)
	reply(w, r, out, err)
}

func (h *handler) poll(w http.ResponseWriter, r *http.Request) {
	out, err := h.Inventory.Poll(r.Context(), r.PathValue("hostID"), r.Header.Get(orchapi.HeaderTime), r.Header.Get(orchapi.HeaderSignature))
	reply(w, r, out, err)
}

func reply(w http.ResponseWriter, r *http.Request, v any, err error) {
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// Serve runs the API on l, a listener that already speaks TLS, until ctx ends.
func Serve(ctx context.Context, l net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 2 * time.Minute}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(done)
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}
