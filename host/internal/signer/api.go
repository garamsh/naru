package signer

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"

	"github.com/garamsh/naru/lib/identity"
)

const (
	// RequestHeader carries a signed request on a GET, which has no body.
	RequestHeader = "X-Naru-Request"
	maxBodyBytes  = 1 << 20
)

// Handler returns the API served on the unix socket.
func (s *Signer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/host", s.handle(func(r *http.Request) (any, error) {
		var req identity.HostRequest
		if err := readJSON(r, &req); err != nil {
			return nil, err
		}
		return s.IssueHost(req)
	}))
	mux.HandleFunc("POST /v1/orch", handleSigned(s, s.IssueOrch))
	mux.HandleFunc("POST /v1/operator", handleSigned(s, s.IssueOperator))
	mux.HandleFunc("POST /v1/revoke", handleSigned(s, s.Revoke))
	mux.HandleFunc("POST /v1/reissue-root", handleSigned(s, s.ReissueRoot))
	mux.HandleFunc("POST /v1/crl", handleSigned(s, s.PushCRL))
	mux.HandleFunc("GET /v1/crl", s.handle(func(*http.Request) (any, error) { return s.CRL() }))
	mux.HandleFunc("GET /v1/ca", s.handle(func(*http.Request) (any, error) { return s.Authorities() }))
	mux.HandleFunc("GET /v1/bundle", s.handle(func(r *http.Request) (any, error) {
		signed, err := base64.StdEncoding.DecodeString(r.Header.Get(RequestHeader))
		if err != nil || len(signed) == 0 {
			return nil, fmt.Errorf("%w: header %s holds no base64 request", ErrBadRequest, RequestHeader)
		}
		return s.Bundle(signed)
	}))
	mux.HandleFunc("POST /v1/restore", s.handle(func(r *http.Request) (any, error) {
		var body identity.RestoreBody
		if err := readJSON(r, &body); err != nil {
			return nil, err
		}
		return struct{}{}, s.Restore(body)
	}))
	return s.wrap(mux)
}

// PeerHandler returns the API served to other signers over TLS: replication only.
func (s *Signer) PeerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/replicate", handleSigned(s, s.Replicate))
	return s.wrap(mux)
}

// handleSigned serves an endpoint whose body is a signed request.
func handleSigned[T any](s *Signer, fn func([]byte) (T, error)) http.HandlerFunc {
	return s.handle(func(r *http.Request) (any, error) {
		body, err := readBody(r)
		if err != nil {
			return nil, err
		}
		return fn(body)
	})
}

// handle runs fn and writes its result: a []byte as it is, anything else as JSON.
func (s *Signer) handle(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			code := statusOf(err)
			s.log.Warn("refused a request", "method", r.Method, "path", r.URL.Path, "status", code, "reason", err.Error())
			http.Error(w, err.Error(), code)
			return
		}
		if b, ok := v.([]byte); ok {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(b)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

// wrap turns a panic into a 500 and logs each request's method, path and outcome, never its body.
func (s *Signer) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				s.log.Error("a handler panicked", "path", r.URL.Path, "panic", fmt.Sprint(p))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path)
		next.ServeHTTP(w, r)
	})
}

// statusOf maps a refusal to its HTTP status. A caller the authorities do not vouch for or whose
// role does not allow the action is 403; a malformed request 400; a state the signer is not in 409.
func statusOf(err error) int {
	switch {
	case errors.Is(err, ErrInitialised), errors.Is(err, ErrNotInitialised):
		return http.StatusConflict
	case errors.Is(err, ErrBadRequest), errors.Is(err, identity.ErrFormat):
		return http.StatusBadRequest
	case errors.Is(err, ErrForbidden),
		errors.Is(err, identity.ErrRole),
		errors.Is(err, identity.ErrRevoked),
		errors.Is(err, identity.ErrNoChain),
		errors.Is(err, identity.ErrAdmissionRole),
		errors.Is(err, identity.ErrAdmissionExpired),
		errors.Is(err, identity.ErrAdmissionSignature),
		errors.Is(err, identity.ErrRequestAction),
		errors.Is(err, identity.ErrRequestExpired),
		errors.Is(err, identity.ErrRequestSignature),
		errors.Is(err, identity.ErrRecoverySignature),
		errors.Is(err, ErrReplay),
		errors.Is(err, identity.ErrCRLSignature),
		errors.Is(err, identity.ErrOpen),
		errors.Is(err, identity.ErrPassphrase):
		return http.StatusForbidden
	}
	return http.StatusForbidden
}

func readBody(r *http.Request) ([]byte, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read body: %w", ErrBadRequest, err)
	}
	return b, nil
}

func readJSON(r *http.Request, v any) error {
	b, err := readBody(r)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	return nil
}

// ListenSocket listens on the unix socket at path, replacing a stale one, and sets its mode. The
// group that may reach the socket is the deployment's to set.
func ListenSocket(path string, mode os.FileMode, log *slog.Logger) (net.Listener, error) {
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("set mode of %s: %w", path, err)
	}
	log.Info("listening", "socket", path, "mode", mode.String())
	return l, nil
}
