// Package api serves the Node API: HTTPS with JSON over mutual TLS 1.3, with a role taken from
// the client certificate's Organization
// (docs/architecture/adr/0017-the-node-api-is-served-by-init-over-mutual-tls-from-its-own-authority-and-naructl-is-its-client.md).
package api

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/garamsh/naru/lib/init/apply"
	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/service"
	"github.com/garamsh/naru/lib/init/store"
	"github.com/garamsh/naru/lib/init/timesync"
	"github.com/garamsh/naru/lib/init/upgrade"
)

// Engine is the configuration the Node holds and the means to change it.
type Engine interface {
	// Config returns the stored configuration and its revision.
	Config() (revision int, cfg config.NodeConfig)
	// Apply applies a change.
	Apply(apply.Request) (apply.Result, error)
}

// Services reports the state of the services init supervises.
type Services interface {
	Status() []service.Status
}

// Logs holds the lines the services wrote.
type Logs interface {
	// Lines returns the kept lines of source numbered after the given number, the number of
	// the last line kept, and a channel that closes when another line is kept.
	Lines(source string, after uint64) (lines []string, last uint64, changed <-chan struct{})
}

// Deps is what the API reads from the Node.
type Deps struct {
	Services Services
	Logs     Logs
	// Version is the naru version init reports.
	Version string
	// BootID returns the kernel's ID of this boot.
	BootID func() (string, error)
	// Uptime returns how long the Node has been up.
	Uptime func() time.Duration
	// Reboot asks init to stop the services and reboot the Node. It returns at once, and is
	// called after the reply that asked for the reboot has been sent.
	Reboot func()
	// Upgrades is the in-place upgrade; nil on a Node that has none.
	Upgrades Upgrades
	Log      *slog.Logger
}

// Upgrades is the Node's in-place A/B upgrade.
type Upgrades interface {
	// Install stages the bundle read from body, whose SHA-256 is expected to be sha256, in the
	// trial slot and arms the firmware to boot it once. It does not reboot.
	Install(body io.Reader, sha256 string) (upgrade.Staged, error)
	// Slot is "default" or "trial", the slot this boot came from.
	Slot() string
	// Last returns how the last upgrade ended, and false when none has.
	Last() (upgrade.Outcome, bool)
}

// Server is the Node API. It serves nothing until Start, and applies nothing until Attach.
type Server struct {
	deps Deps

	tls atomic.Pointer[tls.Config] // what each handshake uses; Reload replaces it

	mu     sync.Mutex
	chosen store.Stored // what GET /v1/config reports until an engine is attached
	engine Engine
	// rebooting is set once a reboot is accepted; a request that changes the Node is refused after.
	rebooting atomic.Bool
	// timeStatus reports the clock; nil until UseTime.
	timeStatus func() timesync.Status
	srv        *http.Server
	addr       net.Addr
	done       chan struct{} // closed when the serving goroutine ends
}

// New returns a Server that reads the Node through deps.
func New(deps Deps) *Server {
	return &Server{deps: deps}
}

// Start serves the Node API for the configuration chosen at boot, on its spec.api.listen, and
// returns once the listener is open. When the configuration has no spec.api it logs that the
// API is off and serves nothing. Until Attach, GET /v1/config reports chosen and POST /v1/config
// is refused with 500.
func (s *Server) Start(chosen store.Stored) error {
	a := chosen.Config.Spec.API
	if a == nil {
		s.deps.Log.Info("the Node API is off: the configuration has no spec.api")
		return nil
	}
	conf, err := tlsConfig(*a)
	if err != nil {
		return fmt.Errorf("Node API: %w", err)
	}
	ln, err := net.Listen("tcp", a.ListenAddr())
	if err != nil {
		return fmt.Errorf("Node API: %w", err)
	}
	s.tls.Store(conf)

	srv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// A refused handshake is not logged: it would fill the kept console log for anyone who can reach the port.
		ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
				return s.tls.Load(), nil
			},
		},
	}
	done := make(chan struct{})
	s.mu.Lock()
	s.chosen, s.addr, s.srv, s.done = chosen, ln.Addr(), srv, done
	s.mu.Unlock()

	go func() {
		defer close(done)
		if err := srv.ServeTLS(ln, "", ""); !errors.Is(err, http.ErrServerClosed) {
			s.deps.Log.Error("Node API stopped serving", "err", err)
		}
	}()
	s.deps.Log.Info("the Node API is listening", "addr", ln.Addr().String())
	return nil
}

// Attach hands the Server the engine that holds the Node's configuration, once the boot has
// finished. From then on the Server reads and applies through it.
func (s *Server) Attach(e Engine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.engine = e
}

// Reload makes the next handshake use the serving pair and the client CA of a. A connection
// already open keeps what it was made with, and the listener stays open.
func (s *Server) Reload(a config.API) error {
	if s.tls.Load() == nil {
		return errors.New("Node API: not serving")
	}
	conf, err := tlsConfig(a)
	if err != nil {
		return fmt.Errorf("Node API: %w", err)
	}
	s.tls.Store(conf)
	s.deps.Log.Info("the Node API reloaded its serving pair and client CA")
	return nil
}

// Addr returns the address the listener is open on, and nil when none is.
func (s *Server) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Close stops the listener, drops the connections and waits for the serving goroutine.
func (s *Server) Close() error {
	s.mu.Lock()
	srv, done := s.srv, s.done
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Close()
	<-done
	return err
}

// tlsConfig returns the configuration of one handshake: TLS 1.3 only, the serving pair, and a
// client certificate required and verified against the client CA.
func tlsConfig(a config.API) (*tls.Config, error) {
	pair, err := tls.X509KeyPair([]byte(a.Certificate), []byte(a.Key))
	if err != nil {
		return nil, fmt.Errorf("serving pair: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(a.ClientCA)) {
		return nil, errors.New("clientCA holds no certificate")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{pair},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		NextProtos:   []string{"http/1.1"},
	}, nil
}
