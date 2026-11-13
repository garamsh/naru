package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// handler returns the routes behind the middleware: a panic is answered with 500, each request
// is logged once, and the client's role is checked before any route runs. A request that is not
// a GET needs Admin, and every other needs Reader.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/version", s.version)
	mux.HandleFunc("GET /v1/services", s.services)
	mux.HandleFunc("GET /v1/services/{name}/logs", s.logs)
	mux.HandleFunc("GET /v1/time", s.time)
	mux.HandleFunc("GET /v1/config", s.getConfig)
	mux.HandleFunc("POST /v1/config", s.postConfig)
	mux.HandleFunc("POST /v1/reboot", s.postReboot)
	mux.HandleFunc("POST /v1/upgrade", s.postUpgrade)
	return s.recovering(s.logged(s.authorised(mux)))
}

// authorised answers 403 to a client whose role does not cover the method, and 503 to a request
// that changes the Node once a reboot is accepted.
func (s *Server) authorised(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		need := Reader
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			need = Admin
		}
		if roleOf(clientCertificate(r)) < need {
			writeJSON(w, http.StatusForbidden, errorBody{Error: "the client certificate's role does not allow this request"})
			return
		}
		if need == Admin && s.rebooting.Load() {
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: "the Node is rebooting"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recovering answers 500 to a request whose handler panicked.
func (s *Server) recovering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.deps.Log.Error("Node API handler panicked", "method", r.Method, "path", r.URL.Path, "panic", v)
				writeJSON(w, http.StatusInternalServerError, errorBody{Error: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// logged logs each request when it ends: its method, path, status, the client's subject and
// role, and a trace_id made for it. Bodies are never logged.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			cert := clientCertificate(r)
			s.deps.Log.Info("Node API request", "trace_id", traceID(), "method", r.Method, "path", r.URL.Path,
				"status", rec.status, "subject", cert.Subject.String(), "duration", time.Since(start).String())
		}()
		next.ServeHTTP(rec, r)
	})
}

// traceID returns a random identifier for one request: init has no tracer to take one from.
func traceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection's flusher.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// errorBody is the body of every refusal and failure.
type errorBody struct {
	Error  string   `json:"error"`
	Fields []string `json:"fields,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A client that went away has nowhere to report a failed write to.
	_ = json.NewEncoder(w).Encode(body)
}
