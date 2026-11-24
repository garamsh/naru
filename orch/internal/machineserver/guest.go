package machineserver

import (
	"errors"
	"io"
	"net/http"

	"github.com/garamsh/naru/lib/machineapi"
)

// raw is a body answered as it is, as a guest file's content is.
type raw []byte

func (h *handler) exec(r *http.Request, caller string) (int, any, error) {
	var e machineapi.ExecRequest
	if err := decode(r, &e); err != nil {
		return 0, nil, err
	}
	out, err := h.Machines.Exec(r.Context(), caller, r.PathValue("name"), e)
	return http.StatusOK, out, err
}

func (h *handler) readFile(r *http.Request, caller string) (int, any, error) {
	b, err := h.Machines.ReadFile(r.Context(), caller, r.PathValue("name"), r.URL.Query().Get("path"))
	return http.StatusOK, raw(b), err
}

func (h *handler) writeFile(r *http.Request, caller string) (int, any, error) {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, machineapi.MaxGuestBytes))
	var big *http.MaxBytesError
	if errors.As(err, &big) {
		return 0, nil, errTooLarge
	}
	if err != nil {
		return 0, nil, errBody
	}
	return http.StatusNoContent, nil, h.Machines.WriteFile(r.Context(), caller, r.PathValue("name"), r.URL.Query().Get("path"), b)
}
