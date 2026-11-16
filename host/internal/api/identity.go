package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/garamsh/naru/host/internal/enroll"
	"github.com/garamsh/naru/lib/hostapi"
)

// Identity is the pair the host reports to orch with, which PUT /v1/identity replaces: *enroll.Agent.
type Identity interface {
	// Rotate installs the certificate and key, PEM, and returns the serial of the certificate.
	Rotate(ctx context.Context, certificate, key []byte) (serial string, err error)
}

// maxIdentityBytes bounds the body of a replacement: a certificate and an ECDSA key are under 2 KiB.
const maxIdentityBytes = 64 << 10

// identityRequest is the body of PUT /v1/identity: the PEM of a certificate and of its PKCS #8 key.
type identityRequest struct {
	Certificate string `json:"certificate"`
	Key         string `json:"key"`
}

// identityAnswer is what PUT /v1/identity answers with: the serial, in hexadecimal, of the certificate
// the host now reports with.
type identityAnswer struct {
	Serial string `json:"serial"`
}

// putIdentity replaces the pair the host reports to orch with (ADR 0077). Orch has to accept the new
// pair before it replaces anything, so a 200 means the host is already reporting with it, and
// anything else leaves the old pair in use. A host that is not enrolled with an orch answers 409.
func (s *Server) putIdentity(w http.ResponseWriter, r *http.Request) {
	if s.deps.Identity == nil {
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: "this host was started with no orch to enroll with, so it has no pair to replace"})
		return
	}
	var req identityRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIdentityBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body is not one JSON object of a certificate and a key"})
		return
	}
	serial, err := s.deps.Identity.Rotate(r.Context(), []byte(req.Certificate), []byte(req.Key))
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, identityAnswer{Serial: serial})
	case errors.Is(err, enroll.ErrNotEnrolled):
		writeJSON(w, http.StatusConflict, hostapi.ErrorBody{Error: err.Error()})
	case errors.Is(err, enroll.ErrBadPair):
		writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: err.Error(), Fields: []string{"certificate", "key"}})
	case errors.Is(err, enroll.ErrOrchRefused):
		writeJSON(w, http.StatusBadGateway, hostapi.ErrorBody{Error: err.Error()})
	default:
		s.deps.Log.Error("cannot replace the host's pair", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, hostapi.ErrorBody{Error: "cannot replace the pair"})
	}
}
