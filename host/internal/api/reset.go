package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/garamsh/naru/lib/atomicfile"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
)

// resetCheckTimeout bounds reading the revocation list for the revoked-pair check.
const resetCheckTimeout = 30 * time.Second

// MaintenanceInfo is what a host in maintenance mode reports at GET /v1/maintenance.
type MaintenanceInfo struct {
	HostID string
	Reason string
}

// MaintenanceHandler returns the routes of a host in maintenance mode (ADR 0082): the maintenance
// report and the reset, behind the same middleware as Handler. Nothing else is served, as no
// runtime is behind it.
func (s *Server) MaintenanceHandler(info MaintenanceInfo) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/maintenance", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, hostapi.Maintenance{HostID: info.HostID, Reason: info.Reason})
	})
	s.routeReset(mux)
	return s.recovering(s.logged(s.authorised(mux)))
}

// routeReset registers the reset call.
func (s *Server) routeReset(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/reset", s.reset)
}

// resetMarker is the file hostd leaves for init, which reads it first after it mounts the data disk.
type resetMarker struct {
	Scope string `json:"scope"`
}

// reset records a reset of the data disk by scope and reboots the host, which carries it out at its
// next boot (ADR 0082). The confirmation must be the host's ID. A scope that clears the data disk
// is refused with 409 while the pair the configuration names is revoked, as the host would serve
// that pair again after the reboot and be unreachable.
func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	var req hostapi.ResetRequest
	if !decode(w, r, &req) {
		return
	}
	switch req.Scope {
	case hostapi.ResetStore, hostapi.ResetData, hostapi.ResetAll:
	default:
		invalid(w, "scope")
		return
	}
	if s.deps.HostID == "" || s.deps.ResetMarker == "" || s.deps.Reboot == nil {
		writeJSON(w, http.StatusNotImplemented, hostapi.ErrorBody{Error: "this host was started without a reset"})
		return
	}
	if req.Confirm != s.deps.HostID {
		invalid(w, "confirm")
		return
	}
	if req.Scope != hostapi.ResetStore {
		if msg := s.revokedPair(r.Context()); msg != "" {
			writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: msg, Code: hostapi.CodeRevokedPair})
			return
		}
	}
	b, _ := json.Marshal(resetMarker{Scope: req.Scope})
	if err := atomicfile.Write(s.deps.ResetMarker, b, 0o600); err != nil {
		s.deps.Log.Error("cannot record the reset", "file", s.deps.ResetMarker, "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "the reset cannot be recorded"})
		return
	}
	s.deps.Log.Warn("reset requested; the host reboots", "scope", req.Scope)
	writeJSON(w, http.StatusAccepted, hostapi.ResetResponse{Scope: req.Scope})
	go func() {
		// The answer is on its way; the reboot ends this process, so give it a moment to be sent.
		time.Sleep(time.Second)
		if err := s.deps.Reboot(); err != nil {
			s.deps.Log.Error("cannot reboot for the reset", "err", err.Error())
			_ = os.Remove(s.deps.ResetMarker)
		}
	}()
}

// revokedPair returns why the pair the configuration names cannot serve, or "" when it can, or
// when no pair or list is configured to check. A list that cannot be had refuses the reset: the
// check cannot be answered.
func (s *Server) revokedPair(ctx context.Context) string {
	if s.deps.ConfigPair == nil || s.deps.Revocations == nil {
		return ""
	}
	pair, err := s.deps.ConfigPair()
	if err != nil {
		return "the pair the configuration names cannot be read: " + err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, resetCheckTimeout)
	defer cancel()
	if err := s.deps.Revocations.Check(ctx, [][]*x509.Certificate{pair}); err != nil {
		return "the pair the configuration names is revoked or cannot be checked, and the host would serve it again after a reset: " + err.Error()
	}
	return ""
}

// ServerTLS returns the configuration of a listener that serves pair, requires a client certificate
// that chains to pool and, when revocations is not nil, refuses a revoked one at the handshake.
func ServerTLS(pair tls.Certificate, pool *x509.CertPool, revocations *identity.RevocationList) *tls.Config {
	conf := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert, NextProtos: []string{"http/1.1"},
	}
	if revocations != nil {
		conf.VerifyConnection = revocations.VerifyConnection
	}
	return conf
}
