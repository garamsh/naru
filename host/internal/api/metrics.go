package api

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/identity"
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

var (
	listAgeDesc = prometheus.NewDesc("naru_hostd_revocation_list_age_seconds", "Seconds since the revocation list held was fetched; absent while none is held.", nil, nil)
	listSeqDesc = prometheus.NewDesc("naru_hostd_revocation_list_seq", "Sequence number of the revocation list held; absent while none is held.", nil, nil)
	listErrDesc = prometheus.NewDesc("naru_hostd_revocation_list_fetch_error", "1 while the last fetch of the revocation list failed, labelled with why.", []string{"error"}, nil)
)

// revocationCollector exposes the revocation list's age, sequence number and last fetch error.
type revocationCollector struct{ list *identity.RevocationList }

func (revocationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- listAgeDesc
	ch <- listSeqDesc
	ch <- listErrDesc
}

func (c revocationCollector) Collect(ch chan<- prometheus.Metric) {
	st := c.list.Status()
	if age, held := c.list.Age(); held {
		ch <- prometheus.MustNewConstMetric(listAgeDesc, prometheus.GaugeValue, age.Seconds())
		ch <- prometheus.MustNewConstMetric(listSeqDesc, prometheus.GaugeValue, float64(st.Seq))
	}
	if st.LastError != "" {
		ch <- prometheus.MustNewConstMetric(listErrDesc, prometheus.GaugeValue, 1, st.LastError)
	}
}

// newMetrics registers the host API's metrics on reg: requests by route and code, microVMs by
// state and, when list is not nil, the revocation list's age and last fetch error.
func newMetrics(reg prometheus.Registerer, rt mvmd.Runtime, list *identity.RevocationList, log *slog.Logger) *prometheus.CounterVec {
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "naru_hostd_requests_total", Help: "Requests to the host API by route and status code.",
	}, []string{"route", "code"})
	reg.MustRegister(requests, microvmCollector{rt: rt, log: log})
	if list != nil {
		reg.MustRegister(revocationCollector{list})
	}
	return requests
}

// observe counts a finished request under its route pattern and status code.
func (s *Server) observe(pattern string, status int) {
	if pattern == "" {
		pattern = routeNone
	}
	s.requests.WithLabelValues(pattern, strconv.Itoa(status)).Inc()
}
