package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// maxFenceSeconds bounds the length of a fence, a day, so the duration cannot overflow.
const maxFenceSeconds = 24 * 60 * 60

// decode reads the one JSON object of the request's body into v, and answers 400 and reports false
// when the body is anything else.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil || dec.More() {
		writeJSON(w, http.StatusBadRequest, hostapi.ErrorBody{Error: "the body is not one JSON object of the fields the API names"})
		return false
	}
	return true
}

func invalid(w http.ResponseWriter, fields ...string) {
	writeJSON(w, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: "the request is invalid", Fields: fields})
}

func fenceView(d mvmd.Deadline) hostapi.Fence {
	return hostapi.Fence{
		Phase: d.Phase, LengthSeconds: uint64(d.Length / time.Second), RemainingSeconds: uint64(d.Remaining / time.Second),
		Renewal: d.LastNumber,
	}
}

// renewFence sets the deadline of one microVM's own fence. A microVM that has none is 409, an
// unknown one 404.
func (s *Server) renewFence(w http.ResponseWriter, r *http.Request) {
	var req hostapi.FenceRequest
	if !decode(w, r, &req) {
		return
	}
	var fields []string
	if req.LengthSeconds == 0 || req.LengthSeconds > maxFenceSeconds {
		fields = append(fields, "lengthSeconds")
	}
	if req.Renewal == 0 {
		fields = append(fields, "renewal")
	}
	if len(fields) > 0 {
		invalid(w, fields...)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := s.deps.Runtime.RenewMachine(ctx, r.PathValue("name"), time.Duration(req.LengthSeconds)*time.Second, req.Renewal)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fenceView(d))
}

// getFence reports the state of one microVM's own fence.
func (s *Server) getFence(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := s.deps.Runtime.InspectMachineDeadline(ctx, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, fenceView(d))
}
