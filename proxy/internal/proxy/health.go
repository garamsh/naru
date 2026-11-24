package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// Health is the answer of GET /healthz: the revision of the table the proxy forwards on (0 before
// the first), when hostd took it, and whether it is older than the max age. hostd counts a proxy
// that answers as a candidate for a proxy exposure (ADR 0112 item 8).
type Health struct {
	Revision   int64     `json:"revision"`
	ReceivedAt time.Time `json:"receivedAt,omitzero"`
	Expired    bool      `json:"expired"`
}

// HealthHandler answers GET /healthz.
func (s *Server) HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		t := s.conf.Routes.table()
		h := Health{Expired: t.expired(s.conf.Now(), s.conf.MaxAge)}
		if t != nil {
			h.Revision, h.ReceivedAt = t.revision, t.receivedAt
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h)
	})
	return mux
}
