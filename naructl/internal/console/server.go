package console

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"time"

	"github.com/garamsh/naru/lib/identity"
)

//go:embed web
var webFiles embed.FS

const (
	eventLimit    = 200
	readTimeout   = 15 * time.Second
	callTimeout   = 20 * time.Second
	shutdownGrace = 5 * time.Second
)

// Config is what a Server is built from.
type Config struct {
	Orch Orch
	// Fleet shows microVMs and groups; nil when orch's API does not carry them yet.
	Fleet Fleet
	// Sign signs an admission with the operator's key. The Server never holds the key.
	Sign func(identity.Admission) ([]byte, error)
	// AdmissionTTL is how long a signed admission stays valid.
	AdmissionTTL time.Duration
	// AllowedHosts are the Host headers a request may carry; empty allows any. A loopback server lists
	// its own addresses so that a page on another origin that resolves its name to loopback is refused.
	AllowedHosts []string
	Now          func() time.Time
	Log          *slog.Logger
}

// Server serves the console's pages.
type Server struct {
	cfg   Config
	csrf  string
	pages map[string]*template.Template
}

// New returns a Server with a CSRF token of its own, made for this process.
func New(cfg Config) (*Server, error) {
	if cfg.Orch == nil || cfg.Sign == nil {
		return nil, errors.New("console: Orch and Sign are required")
	}
	if cfg.AdmissionTTL <= 0 {
		return nil, errors.New("console: AdmissionTTL must be positive")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("console: csrf token: %w", err)
	}
	s := &Server{cfg: cfg, csrf: hex.EncodeToString(raw), pages: map[string]*template.Template{}}
	for _, name := range []string{"hosts", "microvms", "groups", "events", "pending"} {
		t, err := template.ParseFS(webFiles, "web/base.html", "web/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("console: page %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Handler returns the console's routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/hosts", http.StatusSeeOther) })
	mux.HandleFunc("GET /hosts", s.hosts)
	mux.HandleFunc("GET /microvms", s.microvms)
	mux.HandleFunc("GET /groups", s.groups)
	mux.HandleFunc("GET /events", s.events)
	mux.HandleFunc("GET /pending", s.pending)
	mux.HandleFunc("POST /pending/admit", s.admit)
	mux.HandleFunc("GET /static/{file}", s.static)
	return s.guard(mux)
}

// static serves the stylesheet and the script, and nothing else of web/.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	switch name := r.PathValue("file"); name {
	case "style.css", "app.js":
		http.ServeFileFS(w, r, webFiles, "web/"+name)
	default:
		http.NotFound(w, r)
	}
}

// guard refuses a request whose Host is not one of the server's own and sets the headers every page
// carries.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.cfg.AllowedHosts) > 0 && !slices.Contains(s.cfg.AllowedHosts, r.Host) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; script-src 'self'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// Serve serves on ln until ctx ends.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: readTimeout, ReadTimeout: readTimeout}
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		done <- srv.Shutdown(sctx)
	}()
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return <-done
}

// Listen opens addr. Unless allowRemote, addr must be a loopback IP address.
func Listen(addr string, allowRemote bool) (net.Listener, error) {
	if !allowRemote {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("listen address %q: %w", addr, err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("listen address %q is not a loopback IP address; the console serves loopback unless --allow-remote is given", addr)
		}
	}
	return net.Listen("tcp", addr)
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := s.pages[name].ExecuteTemplate(w, "base", p); err != nil {
		s.cfg.Log.Error("render page", "page", name, "err", err)
	}
}

func (s *Server) csrfOK(token string) bool {
	return subtle.ConstantTimeCompare([]byte(token), []byte(s.csrf)) == 1
}

func (s *Server) orchCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), callTimeout)
}
