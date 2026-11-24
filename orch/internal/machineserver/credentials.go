package machineserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/signer"
)

// Credentials is the signer as a caller's registration and renewal reach it: the member passes
// each to its own signer and adds no authority (ADR 0116 item 7).
type Credentials interface {
	RegisterCaller(ctx context.Context, token string, csr []byte) (identity.Issued, error)
	RenewCaller(ctx context.Context, signed []byte) (identity.Issued, error)
}

// Limiter bounds the registrations of one source address.
type Limiter interface {
	Allow(source string) bool
}

var (
	// errTokenRefused is a registration token the signer refused.
	errTokenRefused = errors.New("the registration token is not valid, or was spent or has expired")
	// errRateLimited is a source over its registrations.
	errRateLimited = errors.New("too many registrations from this address")
	// errSigner is a signer that cannot answer now.
	errSigner = errors.New("the signer cannot answer now")
)

// register spends a registration token for a caller certificate. It is the one operation that takes
// no client certificate, the route anonymous that certified lets through: the token is the
// authentication.
func (h *handler) register(w http.ResponseWriter, r *http.Request) {
	src, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		src = r.RemoteAddr
	}
	if !h.Sources.Allow(src) {
		writeError(w, r, errRateLimited)
		return
	}
	var req machineapi.RegisterRequest
	if err := decode(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	issued, err := h.Credentials.RegisterCaller(r.Context(), req.Token, req.CSR)
	if errors.Is(err, inventory.ErrSignerRefused) {
		err = errTokenRefused
	}
	if err != nil {
		writeError(w, r, signerError(err))
		return
	}
	writeJSON(w, http.StatusOK, machineapi.Credential{Certificate: issued.Certificate, NotAfter: issued.NotAfter})
}

// renew passes a renewal the caller signed with its current certificate, which the listener has
// already authenticated as a caller's.
func (h *handler) renew(r *http.Request, _ string) (int, any, error) {
	var req machineapi.RenewRequest
	if err := decode(r, &req); err != nil {
		return 0, nil, err
	}
	issued, err := h.Credentials.RenewCaller(r.Context(), req.Signed)
	if errors.Is(err, inventory.ErrSignerRefused) {
		err = fmt.Errorf("%w: %w", errCaller, err)
	}
	if err != nil {
		return 0, nil, signerError(err)
	}
	return http.StatusOK, machineapi.Credential{Certificate: issued.Certificate, NotAfter: issued.NotAfter}, nil
}

// signerError keeps the refusals the API answers and turns any other failure of the signer into
// errSigner, which names nothing of it.
func signerError(err error) error {
	switch {
	case errors.Is(err, errTokenRefused), errors.Is(err, errCaller), errors.Is(err, identity.ErrCallerNotDue), errors.Is(err, errBody):
		return err
	case errors.Is(err, signer.ErrInvalid):
		return fmt.Errorf("%w: %w", errBody, err)
	}
	return fmt.Errorf("%w: %w", errSigner, err)
}
