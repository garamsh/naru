package api

import (
	"net/http"
)

func (s *Server) postReboot(w http.ResponseWriter, _ *http.Request) {
	s.reply(w, http.StatusAccepted, struct {
		Actions []actionBody `json:"actions"`
	}{[]actionBody{{"reboot", "node"}}}, true)
}

// reply writes body as the answer. When reboot is set it sends the answer to the client before it
// asks init to reboot: the connection is told to close, the answer is flushed, and only then is
// Deps.Reboot called, so the services stop and the listener closes after the client has it.
func (s *Server) reply(w http.ResponseWriter, status int, body any, reboot bool) {
	if !reboot {
		writeJSON(w, status, body)
		return
	}
	s.rebooting.Store(true)
	w.Header().Set("Connection", "close")
	writeJSON(w, status, body)
	if err := http.NewResponseController(w).Flush(); err != nil {
		s.deps.Log.Error("send the reply that asks for a reboot", "err", err)
	}
	s.deps.Reboot()
}
