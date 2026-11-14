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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/groupsvc"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/restore"
	"github.com/garamsh/naru/orch/internal/snapshot"
)

// maxBody bounds a request body.
const maxBody = 1 << 20

// Snapshots takes a sealed etcd snapshot now and returns the newest one written.
type Snapshots interface {
	Take(ctx context.Context) (orchapi.SnapshotResult, error)
	Latest() ([]byte, error)
}

// Restorer restores the plain etcd snapshot read from r, whose SHA-256 the caller states in hex,
// into a member that holds no data, and returns once the member runs. It returns
// restore.ErrRejected for a snapshot it will not restore and ErrRestored once a restore has begun.
type Restorer interface {
	Restore(ctx context.Context, r io.Reader, sha256Hex string) error
}

// ErrRestored is returned by a Restorer that has already restored, or is restoring.
var ErrRestored = errors.New("this member is not waiting for a restore")

// maxRestore bounds a restore's body; etcd's default size limit is 2 GiB, and a member's keys are small.
const maxRestore = 1 << 30

// Networks is the networks service the handlers call.
type Networks interface {
	Apply(ctx context.Context, by, name string, spec orchapi.NetworkSpec) (orchapi.Network, error)
	Get(ctx context.Context, name string) (orchapi.Network, error)
	List(ctx context.Context) ([]orchapi.Network, error)
	Delete(ctx context.Context, by, name string) error
}

// Deps is what the handlers call.
type Deps struct {
	Inventory inventory.Service
	Groups    groupsvc.Service
	Networks  Networks
	Signer    inventory.Signer
	Auth      auth.Authenticator
	Snapshots Snapshots
	// Metrics counts the requests; nil counts none.
	Metrics *Metrics
	Log     *slog.Logger
}

// RestoreDeps is what a member that waits for a restore calls.
type RestoreDeps struct {
	Restorer Restorer
	Auth     auth.Authenticator
	Metrics  *Metrics
	Log      *slog.Logger
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
	mux.HandleFunc("GET /v1/microvms", h.guard(auth.Read, h.microVMs))
	mux.HandleFunc("GET /v1/groups", h.guard(auth.Read, h.groupList))
	mux.HandleFunc("GET /v1/groups/{name}", h.guard(auth.Read, h.groupGet))
	mux.HandleFunc("PUT /v1/groups/{name}", h.guard(auth.Operate, h.groupApply))
	mux.HandleFunc("DELETE /v1/groups/{name}", h.guard(auth.Operate, h.groupDelete))
	mux.HandleFunc("GET /v1/networks", h.guard(auth.Read, h.networkList))
	mux.HandleFunc("GET /v1/networks/{name}", h.guard(auth.Read, h.networkGet))
	mux.HandleFunc("PUT /v1/networks/{name}", h.guard(auth.Operate, h.networkApply))
	mux.HandleFunc("DELETE /v1/networks/{name}", h.guard(auth.Operate, h.networkDelete))
	mux.HandleFunc("GET /v1/crl", h.guard(auth.Any, h.crl))
	mux.HandleFunc("POST /v1/admit", h.guard(auth.Operate, h.admit))
	mux.HandleFunc("POST /v1/enrollment-keys", h.guard(auth.Operate, h.createKey))
	mux.HandleFunc("DELETE /v1/enrollment-keys/{id}", h.guard(auth.Operate, h.revokeKey))
	mux.HandleFunc("PUT /v1/hosts/{id}/labels", h.guard(auth.Operate, h.labels))
	mux.HandleFunc("POST /v1/hosts/{id}/drain", h.guard(auth.Operate, h.drain))
	mux.HandleFunc("POST /v1/hosts/{id}/undrain", h.guard(auth.Operate, h.undrain))
	mux.HandleFunc("POST /v1/snapshot", h.guard(auth.Administer, h.snapshot))
	mux.HandleFunc("GET /v1/snapshots/latest", h.guard(auth.Administer, h.latestSnapshot))
	mux.HandleFunc("PUT /v1/restore", h.guard(auth.Administer, h.alreadyRestored))
	mux.HandleFunc("POST /v1/heartbeat", h.guard(auth.Report, h.heartbeat))
	// Enrollment is the one path without a client certificate: a key, or a proof of one, authenticates.
	mux.HandleFunc("POST /v1/enroll", h.enroll)
	mux.HandleFunc("GET /v1/enroll/{hostID}", h.poll)
	return h.middleware(h.Metrics.count(mux))
}

// NewRestoring returns the handler of a member that holds no data and waits for a restore: PUT
// /v1/restore, with the same certificate check and root role as the full API, and 503 for the rest.
func NewRestoring(d RestoreDeps) http.Handler {
	h := &handler{Deps{Auth: d.Auth, Metrics: d.Metrics, Log: d.Log}}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/restore", h.guard(auth.Administer, func(w http.ResponseWriter, r *http.Request, who identity.Identity) {
		h.restore(d.Restorer, w, r, who)
	}))
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, orchapi.Error{Message: "this member waits for a restore"})
	})
	return h.middleware(h.Metrics.count(mux))
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
	case errors.Is(err, auth.ErrNoRevocationList), errors.Is(err, auth.ErrStaleRevocationList):
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
	case errors.Is(err, restore.ErrRejected):
		status, msg = http.StatusBadRequest, err.Error()
	case errors.Is(err, snapshot.ErrNoSnapshot):
		status, msg = http.StatusNotFound, err.Error()
	case errors.Is(err, ErrRestored):
		status, msg = http.StatusConflict, err.Error()
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

func (h *handler) drain(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	out, err := h.Inventory.Drain(r.Context(), who.Subject, r.PathValue("id"))
	reply(w, r, out, err)
}

func (h *handler) undrain(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	out, err := h.Inventory.Undrain(r.Context(), who.Subject, r.PathValue("id"))
	reply(w, r, out, err)
}

func (h *handler) microVMs(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.MicroVMs(r.Context())
	reply(w, r, out, err)
}

func (h *handler) groupList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.List(r.Context())
	reply(w, r, out, err)
}

func (h *handler) groupGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Groups.Get(r.Context(), r.PathValue("name"))
	reply(w, r, out, err)
}

func (h *handler) groupApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var spec orchapi.GroupSpec
	if !decode(w, r, &spec) {
		return
	}
	name := r.PathValue("name")
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.Name != name {
		writeError(w, r, fmt.Errorf("%w: the body names group %q and the path %q", inventory.ErrInvalid, spec.Name, name))
		return
	}
	out, err := h.Groups.Apply(r.Context(), who.Subject, spec)
	reply(w, r, out, err)
}

func (h *handler) groupDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Groups.Delete(r.Context(), who.Subject, r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) networkList(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Networks.List(r.Context())
	reply(w, r, out, err)
}

func (h *handler) networkGet(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Networks.Get(r.Context(), r.PathValue("name"))
	reply(w, r, out, err)
}

func (h *handler) networkApply(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	var spec orchapi.NetworkSpec
	if !decode(w, r, &spec) {
		return
	}
	out, err := h.Networks.Apply(r.Context(), who.Subject, r.PathValue("name"), spec)
	reply(w, r, out, err)
}

func (h *handler) networkDelete(w http.ResponseWriter, r *http.Request, who identity.Identity) {
	if err := h.Networks.Delete(r.Context(), who.Subject, r.PathValue("name")); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handler) snapshot(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	out, err := h.Snapshots.Take(r.Context())
	reply(w, r, out, err)
}

func (h *handler) latestSnapshot(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	sealed, err := h.Snapshots.Latest()
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(sealed)
}

// restore streams the body to the restorer. The server's read timeout is lifted for it: a snapshot
// is larger than any other body.
func (h *handler) restore(rs Restorer, w http.ResponseWriter, r *http.Request, who identity.Identity) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(10 * time.Minute))
	_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Minute))
	logger(r).Info("restoring a snapshot", "subject", who.Subject)
	if err := rs.Restore(r.Context(), http.MaxBytesReader(w, r.Body, maxRestore), r.Header.Get(orchapi.HeaderSnapshotSHA256)); err != nil {
		writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// alreadyRestored answers a restore sent to a member that runs: restoring replaces nothing.
func (h *handler) alreadyRestored(w http.ResponseWriter, r *http.Request, _ identity.Identity) {
	writeError(w, r, ErrRestored)
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
