package api

import (
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics counts the API's requests by the route that served them and the status code it answered.
type Metrics struct {
	requests *prometheus.CounterVec
}

// NewMetrics registers the request counter on reg.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{requests: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "naru_orch_api_requests_total",
		Help: "Requests the orch API answered, by route pattern and status code.",
	}, []string{"route", "code"})}
	reg.MustRegister(m.requests)
	return m
}

// count wraps the mux, which sets the request's Pattern as it routes; a request no route matched is
// counted under "unmatched" so the label's values stay the routes the API has. A nil *Metrics counts nothing.
func (m *Metrics) count(mux *http.ServeMux) http.Handler {
	if m == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		defer func() {
			code := rec.code
			if v := recover(); v != nil {
				// The middleware answers 500 for a panic; count it as that, and let it.
				code = http.StatusInternalServerError
				defer panic(v)
			}
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			m.requests.WithLabelValues(route, strconv.Itoa(code)).Inc()
		}()
		mux.ServeHTTP(rec, r)
	})
}

// statusWriter records the status code the handler wrote.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the connection, as the restore handler's deadlines need.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
