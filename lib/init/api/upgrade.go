package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/garamsh/naru/lib/init/upgrade"
)

// postUpgrade receives the upgrade bundle as the request body, whose SHA-256 the caller gives in
// the sha256 query parameter, stages it in the trial slot, and answers 202 before the Node reboots
// into it.
func (s *Server) postUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.deps.Upgrades == nil {
		writeJSON(w, http.StatusNotImplemented, errorBody{Error: "this Node cannot upgrade"})
		return
	}
	staged, err := s.deps.Upgrades.Install(r.Body, r.URL.Query().Get("sha256"))
	if err != nil {
		s.upgradeRefused(w, r, err)
		return
	}
	s.reply(w, http.StatusAccepted, struct {
		Actions []actionBody `json:"actions"`
	}{[]actionBody{{"upgrade", staged.To}, {"reboot", "node"}}}, true)
}

// upgradeRefused answers an upgrade that was not staged. A bundle the caller got wrong is 400, one
// that is too large 413, a Node that cannot take one now 409, and anything else 500 with the
// reason left to the log. The unread rest of a bad body is read first, within the cap, so the
// client that is still sending it receives the answer.
func (s *Server) upgradeRefused(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, upgrade.ErrBundle), errors.Is(err, upgrade.ErrTooLarge):
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, upgrade.MaxBundleBytes))
	}
	switch {
	case errors.Is(err, upgrade.ErrBundle):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	case errors.Is(err, upgrade.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, errorBody{Error: err.Error()})
	case errors.Is(err, upgrade.ErrBusy):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	default:
		s.deps.Log.Error("stage an upgrade", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the upgrade could not be staged; see the Node's log"})
	}
}
