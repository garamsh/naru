package run

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/init/service"
	"github.com/garamsh/naru/lib/metrics"
)

// statuses is what the metrics read from the services: the manager's Status.
type statuses interface {
	Status() []service.Status
}

// serviceStates are the states a naru_init_service_state series is reported for; the series of the
// state a service is in is 1 and the others are 0.
var serviceStates = []service.State{service.StateRunning, service.StateBackoff}

// serviceCollector reports the state and the restarts of each supervised service as they are at
// the scrape.
type serviceCollector struct {
	from     statuses
	state    *prometheus.Desc
	restarts *prometheus.Desc
}

func newServiceCollector(from statuses) *serviceCollector {
	return &serviceCollector{
		from:     from,
		state:    prometheus.NewDesc("naru_init_service_state", "Whether the service is in the state, 1 or 0.", []string{"service", "state"}, nil),
		restarts: prometheus.NewDesc("naru_init_service_restarts", "How many times init started the service again after it exited or failed to start, since it was started.", []string{"service"}, nil),
	}
}

func (c *serviceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.state
	ch <- c.restarts
}

func (c *serviceCollector) Collect(ch chan<- prometheus.Metric) {
	for _, s := range c.from.Status() {
		for _, st := range serviceStates {
			v := 0.0
			if s.State == st {
				v = 1
			}
			ch <- prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, v, s.Name, string(st))
		}
		ch <- prometheus.MustNewConstMetric(c.restarts, prometheus.CounterValue, float64(s.Restarts), s.Name)
	}
}

// revocationCollector reports the age, the sequence number and the last fetch error of the
// revocation list the Node API checks its clients against, as they are at the scrape. It reports
// nothing while the API has no list, and reads the list through current because a reload may
// replace it.
type revocationCollector struct {
	current        func() *identity.RevocationList
	age, seq, fail *prometheus.Desc
}

func newRevocationCollector(current func() *identity.RevocationList) *revocationCollector {
	return &revocationCollector{
		current: current,
		age:     prometheus.NewDesc("naru_init_revocation_list_age_seconds", "Seconds since the Node API last verified the signed revocation list.", nil, nil),
		seq:     prometheus.NewDesc("naru_init_revocation_list_seq", "Sequence number of the revocation list the Node API holds.", nil, nil),
		fail:    prometheus.NewDesc("naru_init_revocation_list_fetch_error", "1 while the last fetch of the revocation list failed; the error is the label.", []string{"error"}, nil),
	}
}

func (c *revocationCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.age
	ch <- c.seq
	ch <- c.fail
}

func (c *revocationCollector) Collect(ch chan<- prometheus.Metric) {
	list := c.current()
	if list == nil {
		return
	}
	st := list.Status()
	if age, held := list.Age(); held {
		ch <- prometheus.MustNewConstMetric(c.age, prometheus.GaugeValue, age.Seconds())
		ch <- prometheus.MustNewConstMetric(c.seq, prometheus.GaugeValue, float64(st.Seq))
	}
	if st.LastError != "" {
		ch <- prometheus.MustNewConstMetric(c.fail, prometheus.GaugeValue, 1, st.LastError)
	}
}

// serveMetrics serves /metrics of init on addr, which is the address spec.metrics names, with the Go and process
// collectors, the state of the services and, when revocations is not nil, the Node API's revocation list, until ctx ends. It does nothing when addr is empty, as
// it is when the spec has no metrics. A listener that cannot start is logged, and init goes on: the services do not depend
// on being scraped.
func serveMetrics(ctx context.Context, addr string, from statuses, revocations func() *identity.RevocationList, log *slog.Logger) {
	if addr == "" {
		return
	}
	reg := metrics.New()
	reg.MustRegister(newServiceCollector(from))
	if revocations != nil {
		reg.MustRegister(newRevocationCollector(revocations))
	}
	go func() {
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Error("serve metrics", "addr", addr, "err", err)
		}
	}()
	log.Info("serving metrics", "addr", addr)
}
