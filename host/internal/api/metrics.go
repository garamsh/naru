package api

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/host/internal/mvmd"
)

// machineStates are the states a microVM is reported in; each is exposed on every scrape, so a
// state with none reads 0 and not absent.
var machineStates = []string{mvmd.StateCreated, mvmd.StateRunning, mvmd.StatePaused, mvmd.StateStopped, mvmd.StateFaulted}

// routeNone labels a request that no route answered, among them one refused for its role.
const routeNone = "none"

var microvmsDesc = prometheus.NewDesc("naru_hostd_microvms", "MicroVMs on this host by state.", []string{"state"}, nil)

// microvmCollector reads the microVMs from mvmd on each scrape. A scrape that cannot list them
// exposes none.
type microvmCollector struct {
	rt  mvmd.Runtime
	log *slog.Logger
}

func (c microvmCollector) Describe(ch chan<- *prometheus.Desc) { ch <- microvmsDesc }

func (c microvmCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	list, err := c.rt.ListMachines(ctx)
	if err != nil {
		c.log.Warn("cannot count the microVMs for a scrape", "err", err.Error())
		return
	}
	count := map[string]float64{}
	for _, s := range machineStates {
		count[s] = 0
	}
	for _, m := range list {
		count[m.State]++
	}
	for s, n := range count {
		ch <- prometheus.MustNewConstMetric(microvmsDesc, prometheus.GaugeValue, n, s)
	}
}

// newMetrics registers the host API's metrics on reg: requests by route and code, and microVMs by
// state.
func newMetrics(reg prometheus.Registerer, rt mvmd.Runtime, log *slog.Logger) *prometheus.CounterVec {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "naru_hostd_requests_total", Help: "Requests to the host API by route and status code.",
	}, []string{"route", "code"})
	reg.MustRegister(requests, microvmCollector{rt: rt, log: log})
	return requests
}

// observe counts a finished request under its route pattern and status code.
func (s *Server) observe(pattern string, status int) {
	if pattern == "" {
		pattern = routeNone
	}
	s.requests.WithLabelValues(pattern, strconv.Itoa(status)).Inc()
}
