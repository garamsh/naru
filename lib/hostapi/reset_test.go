package hostapi_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

func TestClient_ResetPostsTheScopeAndTheConfirmation(t *testing.T) {
	c, got, body := serve(t, http.StatusAccepted, hostapi.ResetResponse{Scope: hostapi.ResetStore})

	out, err := c.Reset(t.Context(), hostapi.ResetRequest{Scope: hostapi.ResetStore, Confirm: "h-1"})

	require.NoError(t, err)
	assert.Equal(t, "POST", got.Method)
	assert.Equal(t, "/v1/reset", got.URL.Path)
	assert.JSONEq(t, `{"scope":"store","confirm":"h-1"}`, string(*body))
	assert.Equal(t, hostapi.ResetStore, out.Scope)
}

func TestClient_ResetReadsTheRevokedPairCodeOfARefusal(t *testing.T) {
	c, _, _ := serve(t, http.StatusConflict, hostapi.ErrorBody{Error: "the pair is revoked", Code: hostapi.CodeRevokedPair})

	_, err := c.Reset(t.Context(), hostapi.ResetRequest{Scope: hostapi.ResetAll, Confirm: "h-1"})

	var apiErr *hostapi.Error
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, hostapi.CodeRevokedPair, apiErr.Code)
}

func TestClient_MaintenanceReadsTheHostIDAndTheReason(t *testing.T) {
	c, got, _ := serve(t, http.StatusOK, hostapi.Maintenance{HostID: "h-1", Reason: "no valid stored configuration"})

	m, err := c.Maintenance(t.Context())

	require.NoError(t, err)
	assert.Equal(t, "GET", got.Method)
	assert.Equal(t, "/v1/maintenance", got.URL.Path)
	assert.Equal(t, hostapi.Maintenance{HostID: "h-1", Reason: "no valid stored configuration"}, m)
}
