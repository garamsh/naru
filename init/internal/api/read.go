package api

import (
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/garamsh/naru/init/internal/upgrade"
)

type versionBody struct {
	Version       string  `json:"version"`
	BootID        string  `json:"bootId"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	// Slot is "default" or "trial". LastUpgrade is how the last upgrade ended, and absent when none has.
	Slot        string           `json:"slot"`
	LastUpgrade *upgrade.Outcome `json:"lastUpgrade,omitempty"`
}

func (s *Server) version(w http.ResponseWriter, _ *http.Request) {
	bootID, err := s.deps.BootID()
	if err != nil {
		s.deps.Log.Error("read the boot ID", "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "the boot ID cannot be read"})
		return
	}
	body := versionBody{Version: s.deps.Version, BootID: bootID, UptimeSeconds: s.deps.Uptime().Seconds(), Slot: upgrade.SlotDefault}
	if u := s.deps.Upgrades; u != nil {
		body.Slot = u.Slot()
		if last, ok := u.Last(); ok {
			body.LastUpgrade = &last
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// stateStopped is the state of a configured service that is not supervised.
const stateStopped = "stopped"

type serviceBody struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	PID      int    `json:"pid"`
	Restarts int    `json:"restarts"`
	// StartedAt is RFC 3339, and absent for a service that has not started.
	StartedAt string `json:"startedAt,omitempty"`
}

// serviceList returns the services of the stored configuration in its order, each with the
// state init supervises it in, then the supervised services the configuration does not name,
// by name.
func (s *Server) serviceList() []serviceBody {
	_, cfg := s.config()
	status := map[string]serviceBody{}
	var extra []string
	for _, st := range s.deps.Services.Status() {
		b := serviceBody{Name: st.Name, State: string(st.State), PID: st.PID, Restarts: st.Restarts}
		if !st.StartedAt.IsZero() {
			b.StartedAt = st.StartedAt.UTC().Format(time.RFC3339)
		}
		status[st.Name] = b
		extra = append(extra, st.Name)
	}
	out := []serviceBody{}
	for _, svc := range cfg.Spec.Services {
		b, ok := status[svc.Name]
		if !ok {
			b = serviceBody{Name: svc.Name, State: stateStopped}
		}
		out = append(out, b)
		extra = slices.DeleteFunc(extra, func(n string) bool { return n == svc.Name })
	}
	for _, n := range extra {
		out = append(out, status[n])
	}
	return out
}

func (s *Server) services(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Services []serviceBody `json:"services"`
	}{s.serviceList()})
}

// logs writes the kept lines of one service as text, and with follow=true keeps the response
// open and writes each new line as it is kept, until the client closes it.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !slices.ContainsFunc(s.serviceList(), func(b serviceBody) bool { return b.Name == name }) {
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no service named " + strconv.Quote(name)})
		return
	}
	follow := false
	if v := r.URL.Query().Get("follow"); v != "" {
		var err error
		if follow, err = strconv.ParseBool(v); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody{Error: "follow must be true or false", Fields: []string{"follow"}})
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	flush := http.NewResponseController(w).Flush
	var after uint64
	for {
		lines, last, changed := s.deps.Logs.Lines(name, after)
		after = last
		if len(lines) > 0 {
			if _, err := io.WriteString(w, strings.Join(lines, "\n")+"\n"); err != nil {
				return
			}
		}
		if !follow {
			return
		}
		// Flushing before the first line sends the headers, so a client sees the stream open.
		if err := flush(); err != nil {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}
