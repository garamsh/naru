package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/garamsh/naru/lib/metrics"
)

// newRegistry is this member's registry: the Go and process collectors, which metrics.New registers,
// and what names the member and when it started. orchd registers its API counters on it too.
func newRegistry(member string) *prometheus.Registry {
	reg := metrics.New()
	promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
		Name: "naru_orchd_info", Help: "One series per running naru-orchd, labelled with its etcd member name.",
	}, []string{"member"}).WithLabelValues(member).Set(1)
	promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name: "naru_orchd_start_time_seconds", Help: "When naru-orchd started, as Unix seconds.",
	}).Set(float64(time.Now().Unix()))
	return reg
}

// serveMetrics serves reg at /metrics on addr until ctx ends. A listener that stops is logged; orch
// does not stop with it.
func serveMetrics(ctx context.Context, log *slog.Logger, addr string, reg *prometheus.Registry) {
	if err := metrics.Serve(ctx, addr, reg); err != nil {
		log.Error("the metrics listener stopped", "addr", addr, "err", err.Error())
	}
}
