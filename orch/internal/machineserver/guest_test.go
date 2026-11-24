package machineserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

func TestWriteError_AnswersAGuestCallsFailuresWithTheirCodes(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   machineapi.Code
	}{
		{fmt.Errorf("%w: web is stopped", machines.ErrNotRunning), http.StatusConflict, machineapi.CodeNotRunning},
		{fmt.Errorf("%w: /data/a", machines.ErrFileNotFound), http.StatusNotFound, machineapi.CodeFileNotFound},
		{fmt.Errorf("%w: the output", machines.ErrTooLarge), http.StatusRequestEntityTooLarge, machineapi.CodeInvalid},
		{fmt.Errorf("%w: dial tcp 10.0.0.9:7443: refused", machines.ErrHostUnavailable), http.StatusServiceUnavailable, machineapi.CodeUnavailable},
	} {
		r := httptest.NewRequest(http.MethodPost, "/machines/v1alpha1/machines/web/exec", nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, slog.New(slog.DiscardHandler)))
		w := httptest.NewRecorder()
		writeError(w, r, c.err)
		var e machineapi.ErrorBody
		if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if w.Code != c.status || e.Code != c.code {
			t.Errorf("%v: answered %d %s, want %d %s", c.err, w.Code, e.Code, c.status, c.code)
		}
		if c.code == machineapi.CodeUnavailable && e.Error != machines.ErrHostUnavailable.Error() {
			t.Errorf("a host's failure names no host to the caller: %q", e.Error)
		}
	}
}
