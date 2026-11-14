package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/garamsh/naru/lib/metrics"
)

// serveMetrics serves /metrics on addr until ctx ends: the Go and process collectors, which
// metrics.New registers, and what names this member and when it started. A listener that stops
// is logged; orch does not stop with it.
func serveMetrics(ctx context.Context, log *slog.Logger, addr, member string) {
	reg := metrics.New()
	promauto.With(reg).NewGaugeVec(prometheus.GaugeOpts{
		Name: "naru_orchd_info", Help: "One series per running naru-orchd, labelled with its etcd member name.",
	}, []string{"member"}).WithLabelValues(member).Set(1)
	promauto.With(reg).NewGauge(prometheus.GaugeOpts{
		Name: "naru_orchd_start_time_seconds", Help: "When naru-orchd started, as Unix seconds.",
	}).Set(float64(time.Now().Unix()))
	if err := metrics.Serve(ctx, addr, reg); err != nil {
		log.Error("the metrics listener stopped", "addr", addr, "err", err.Error())
	}
}
