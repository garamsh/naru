package api

import (
	"net/http"
	"time"

	"github.com/garamsh/naru/lib/init/timesync"
)

// UseTime has GET /v1/time report the clock through status. Until it is called the endpoint
// reports a Node that keeps no time.
func (s *Server) UseTime(status func() timesync.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeStatus = status
}

type syncBody struct {
	// At is RFC 3339.
	At            string  `json:"at"`
	Server        string  `json:"server"`
	OffsetSeconds float64 `json:"offsetSeconds"`
	DelaySeconds  float64 `json:"delaySeconds"`
	Correction    string  `json:"correction"`
}

type timeBody struct {
	Configured bool     `json:"configured"`
	Servers    []string `json:"servers"`
	// Synchronised is true once a server has set the clock since init started.
	Synchronised bool      `json:"synchronised"`
	LastSync     *syncBody `json:"lastSync,omitempty"`
	// LastError and LastAttemptAt describe the latest attempt, when it found no server.
	LastError     string `json:"lastError,omitempty"`
	LastAttemptAt string `json:"lastAttemptAt,omitempty"`
}

func (s *Server) time(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	status := s.timeStatus
	s.mu.Unlock()
	var st timesync.Status
	if status != nil {
		st = status()
	}
	body := timeBody{Configured: len(st.Servers) > 0, Servers: st.Servers, Synchronised: st.Last != nil, LastError: st.Err}
	if body.Servers == nil {
		body.Servers = []string{}
	}
	if st.Last != nil {
		body.LastSync = &syncBody{
			At:            st.Last.At.UTC().Format(time.RFC3339),
			Server:        st.Last.Server,
			OffsetSeconds: st.Last.Offset.Seconds(),
			DelaySeconds:  st.Last.Delay.Seconds(),
			Correction:    string(st.Last.Correction),
		}
	}
	if !st.Failed.IsZero() {
		body.LastAttemptAt = st.Failed.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, body)
}
