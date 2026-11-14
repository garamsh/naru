package run

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

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

// serveMetrics serves /metrics of init on addr, which is the address spec.metrics names, with the Go and process
// collectors and the state of the services, until ctx ends. It does nothing when addr is empty, as
// it is when the spec has no metrics. A listener that cannot start is logged, and init goes on: the services do not depend
// on being scraped.
func serveMetrics(ctx context.Context, addr string, from statuses, log *slog.Logger) {
	if addr == "" {
		return
	}
	reg := metrics.New()
	reg.MustRegister(newServiceCollector(from))
	go func() {
		if err := metrics.Serve(ctx, addr, reg); err != nil {
			log.Error("serve metrics", "addr", addr, "err", err)
		}
	}()
	log.Info("serving metrics", "addr", addr)
}
