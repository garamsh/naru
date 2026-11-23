package api

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"

	"github.com/garamsh/naru/lib/identity"
)

func TestAnUnknownOrStaleRevocationListIsA503(t *testing.T) {
	for _, err := range []error{identity.ErrNoRevocationList, fmt.Errorf("x: %w", identity.ErrStaleRevocationList)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
		writeError(w, r, err)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, err.Error())
	}
}

func TestAFullDatabaseIsAnInsufficientStorageThatSaysSoAndAnotherStoreFailureStaysA500(t *testing.T) {
	answer := func(err error) (int, string) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPut, "/v1/profile", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
		writeError(w, r, err)
		return w.Code, w.Body.String()
	}
	for name, err := range map[string]error{
		"etcd's error":                rpctypes.ErrNoSpace,
		"wrapped by a store":          fmt.Errorf("put profile: %w", rpctypes.ErrNoSpace),
		"as the client's gRPC status": rpctypes.ErrGRPCNoSpace,
	} {
		code, body := answer(err)
		require.Equal(t, http.StatusInsufficientStorage, code, name)
		require.Contains(t, body, "database space exceeded", name)
	}
	code, body := answer(rpctypes.ErrTimeout)
	require.Equal(t, http.StatusInternalServerError, code, "control: another etcd failure is still a 500")
	require.NotContains(t, body, "database space exceeded")
}
