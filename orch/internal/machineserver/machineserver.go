// Package machineserver serves the Machine API, machines/v1alpha1, on a listener of its own (ADR
// 0108 item 3). It authenticates the certificate against the revocation list, serves the role caller
// alone, calls the machines service for that caller, and translates every failure to the API's
// status and code in one place, writeError.
package machineserver

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
	"net/http"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/auth"
	"github.com/garamsh/naru/orch/internal/machines"
)

// maxBody bounds a request body: a spec's files hold at most 1 MiB, base64 in JSON.
const maxBody = 4 << 20

// Deps is what the handlers call.
type Deps struct {
	Machines machines.Service
	Auth     auth.Authenticator
	// Credentials is the signer a caller registers and renews through, and Sources bounds the
	// registrations of each source address.
	Credentials Credentials
	Sources     Limiter
	Log         *slog.Logger
}

type handler struct{ Deps }

type ctxKey struct{}

// caller is a handler's work for the caller the certificate names. It returns the status and body
// of a success.
type caller func(r *http.Request, name string) (int, any, error)

// New returns the Machine API's handler. Every path outside machines/v1alpha1 is 404.
func New(d Deps) http.Handler {
	h := &handler{d}
	m := http.NewServeMux()
	p := machineapi.PathPrefix
	m.HandleFunc("PUT "+p+"machines/{name}", h.serve(h.create))
	m.HandleFunc("GET "+p+"machines", h.serve(h.list))
	m.HandleFunc("GET "+p+"machines/{name}", h.serve(h.get))
	m.HandleFunc("POST "+p+"machines/{name}/start", h.serve(h.start))
	m.HandleFunc("POST "+p+"machines/{name}/stop", h.serve(h.stop))
	m.HandleFunc("DELETE "+p+"machines/{name}", h.serve(h.delete))
	m.HandleFunc("PUT "+p+"snapshots/{name}", h.serve(h.putSnapshot))
	m.HandleFunc("GET "+p+"snapshots", h.serve(h.snapshots))
	m.HandleFunc("GET "+p+"snapshots/{name}", h.serve(h.snapshot))
	m.HandleFunc("DELETE "+p+"snapshots/{name}", h.serve(h.deleteSnapshot))
	m.HandleFunc("GET "+p+"events", h.serve(h.events))
	m.HandleFunc("POST "+p+"register", h.register)
	m.HandleFunc("POST "+p+"renew", h.serve(h.renew))
	for _, route := range []string{
		"POST " + p + "machines/{name}/exec", "GET " + p + "machines/{name}/files", "PUT " + p + "machines/{name}/files",
		"PUT " + p + "aliases/{hostname}", "GET " + p + "aliases/{hostname}", "DELETE " + p + "aliases/{hostname}",
	} {
		m.HandleFunc(route, h.serve(unimplemented))
	}
	m.HandleFunc("/", h.serve(func(r *http.Request, _ string) (int, any, error) { return 0, nil, errNoRoute }))
	return h.middleware(m)
}

// Unavailable is the handler of a member that cannot serve the API, such as one that waits for a
// restore: 503 to every request, saying why.
func Unavailable(why string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, machineapi.ErrorBody{Error: why, Code: machineapi.CodeUnavailable})
	})
}

// TLSConfig is the listener's configuration: TLS 1.3, and a client certificate, when one is sent,
// that verifies against clientCA. Every operation but register refuses a request without one.
func TLSConfig(cert tls.Certificate, clientCA *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: clientCA}
}

// middleware recovers a panic as a 500 and gives each request a logger with a trace id.
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
				writeJSON(w, http.StatusInternalServerError, machineapi.ErrorBody{Error: "internal error", Code: machineapi.CodeInternal})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func logger(r *http.Request) *slog.Logger { return r.Context().Value(ctxKey{}).(*slog.Logger) }

// errCaller is the refusal of a certificate of another role than caller.
var errCaller = errors.New("the Machine API serves the role caller only")

// errNoRoute is a path the API does not serve.
var errNoRoute = errors.New("no such operation")

// errUnimplemented is an operation a later change of orch serves.
var errUnimplemented = errors.New("orch does not serve this operation yet")

func unimplemented(*http.Request, string) (int, any, error) { return 0, nil, errUnimplemented }

// serve authenticates the certificate, refuses every role but caller, and answers fn's result.
func (h *handler) serve(fn caller) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, err := h.Auth.Authenticate(r.Context(), r.TLS.VerifiedChains)
		if err != nil {
			writeError(w, r, err)
			return
		}
		name, ok := identity.CallerName(who)
		if !ok {
			logger(r).Warn("refused: not a caller", "subject", who.Subject, "role", string(who.Role))
			writeError(w, r, errCaller)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, logger(r).With("user_id", name)))
		status, body, err := fn(r, name)
		if err != nil {
			writeError(w, r, err)
			return
		}
		if body == nil {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, body)
	}
}

// errTooLarge and errBody are a body the API does not take.
var (
	errTooLarge = errors.New("the request body is too large")
	errBody     = errors.New("the request body is not the operation's JSON")
)

// decode reads the one JSON value of the body into out, refusing a field the version does not name.
func decode(r *http.Request, out any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	err := dec.Decode(out)
	if err == nil {
		_, err = dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			err = errBody
		}
	}
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		return errTooLarge
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return errBody
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError is the one place an error becomes the Machine API's status and code. What is not a
// known failure is logged and answered as a 500 without its detail.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, msg := http.StatusInternalServerError, machineapi.CodeInternal, "internal error"
	switch {
	case errors.Is(err, identity.ErrRevoked), errors.Is(err, identity.ErrRole), errors.Is(err, identity.ErrNoChain), errors.Is(err, errCaller):
		status, code, msg = http.StatusForbidden, machineapi.CodeForbidden, err.Error()
	case errors.Is(err, errTokenRefused):
		status, code, msg = http.StatusForbidden, machineapi.CodeTokenRefused, errTokenRefused.Error()
	case errors.Is(err, identity.ErrCallerNotDue):
		status, code, msg = http.StatusConflict, machineapi.CodeNotDue, err.Error()
	case errors.Is(err, errRateLimited):
		status, code, msg = http.StatusTooManyRequests, machineapi.CodeRateLimited, err.Error()
	case errors.Is(err, errSigner):
		status, code, msg = http.StatusServiceUnavailable, machineapi.CodeUnavailable, errSigner.Error()
		logger(r).Error("the signer failed", "err", err.Error())
	case errors.Is(err, identity.ErrNoRevocationList), errors.Is(err, identity.ErrStaleRevocationList):
		status, code, msg = http.StatusServiceUnavailable, machineapi.CodeUnavailable, "the revocation list is not available"
		logger(r).Error("no revocation list", "err", err.Error())
	case errors.Is(err, errNoRoute):
		status, code, msg = http.StatusNotFound, machineapi.CodeNotFound, err.Error()
	case errors.Is(err, errUnimplemented):
		status, code, msg = http.StatusNotImplemented, machineapi.CodeUnimplemented, err.Error()
	case errors.Is(err, errTooLarge):
		status, code, msg = http.StatusRequestEntityTooLarge, machineapi.CodeInvalid, err.Error()
	case errors.Is(err, errBody), errors.Is(err, machines.ErrBadCursor):
		status, code, msg = http.StatusBadRequest, machineapi.CodeInvalid, err.Error()
	case errors.Is(err, machineapi.ErrInvalid):
		status, code, msg = http.StatusUnprocessableEntity, machineapi.CodeInvalid, err.Error()
	case errors.Is(err, machines.ErrNotFound):
		status, code, msg = http.StatusNotFound, machineapi.CodeNotFound, err.Error()
	case errors.Is(err, machines.ErrConflict):
		status, code, msg = http.StatusConflict, machineapi.CodeConflict, err.Error()
	case errors.Is(err, machines.ErrDeleting):
		status, code, msg = http.StatusConflict, machineapi.CodeDeleting, err.Error()
	case errors.Is(err, machines.ErrCursorExpired):
		status, code, msg = http.StatusGone, machineapi.CodeCursorExpired, err.Error()
	case errors.Is(err, machines.ErrMachineNotFound):
		status, code, msg = http.StatusUnprocessableEntity, machineapi.CodeMachineNotFound, err.Error()
	case errors.Is(err, machines.ErrSnapshotNotFound):
		status, code, msg = http.StatusUnprocessableEntity, machineapi.CodeSnapshotNotFound, err.Error()
	case errors.Is(err, machines.ErrSnapshotMismatch):
		status, code, msg = http.StatusUnprocessableEntity, machineapi.CodeSnapshotMismatch, err.Error()
	case errors.Is(err, rpctypes.ErrNoSpace) || rpctypes.Error(err) == rpctypes.ErrNoSpace:
		status, code, msg = http.StatusServiceUnavailable, machineapi.CodeUnavailable, "orch's database is full and refuses writes until the housekeeper has compacted it"
		logger(r).Error("the database is full", "err", err.Error())
	default:
		logger(r).Error("request failed", "err", err.Error())
	}
	writeJSON(w, status, machineapi.ErrorBody{Error: msg, Code: code})
}
