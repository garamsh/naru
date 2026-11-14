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

	"github.com/garamsh/naru/orch/internal/auth"
)

func TestAnUnknownOrStaleRevocationListIsA503(t *testing.T) {
	for _, err := range []error{auth.ErrNoRevocationList, fmt.Errorf("x: %w", auth.ErrStaleRevocationList)} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, slog.New(slog.NewTextHandler(io.Discard, nil))))
		writeError(w, r, err)
		require.Equal(t, http.StatusServiceUnavailable, w.Code, err.Error())
	}
}
