package signer_test

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

func TestStatusOf_DeniesOnlyWhatItNamesAndAnswers500ForAnyOtherError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"a role that does not allow the call", fmt.Errorf("x: %w", signer.ErrForbidden), http.StatusForbidden},
		{"an unknown role", identity.ErrRole, http.StatusForbidden},
		{"a revoked certificate", identity.ErrRevoked, http.StatusForbidden},
		{"a chain to an authority not held", x509.UnknownAuthorityError{}, http.StatusForbidden},
		{"a replayed request", signer.ErrReplay, http.StatusForbidden},
		{"a malformed request", signer.ErrBadRequest, http.StatusBadRequest},
		{"a signer with no authorities", signer.ErrNotInitialised, http.StatusConflict},
		{"a failure of the signer's own", errors.New("write the staging directory: no space left on device"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, signer.StatusOf(c.err), c.name)
	}
}

func TestHandler_AnswersAPathItDoesNotServeWith404(t *testing.T) {
	e := newEnv(t)

	status, _ := e.do("GET", "/v1/nothing", nil)

	assert.Equal(t, http.StatusNotFound, status)
}
