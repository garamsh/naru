package orchd_test

import (
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/orchtest"
)

// counted is the value of the request counter for a route and status code.
func counted(t *testing.T, reg *prometheus.Registry, route, code string) float64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "naru_orch_api_requests_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["route"] == route && labels["code"] == code {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestAPIRequestsAreCountedByRouteAndStatus(t *testing.T) {
	reg := prometheus.NewRegistry()
	o := startOne(t, orchtest.Options{Metrics: reg})

	_, err := o.op.Hosts(t.Context())
	require.NoError(t, err)
	_, err = o.op.Hosts(t.Context())
	require.NoError(t, err)
	_, err = o.reader.Snapshot(t.Context())
	require.Equal(t, http.StatusForbidden, status(err))
	_, err = o.op.Network(t.Context(), "none")
	require.Equal(t, http.StatusNotFound, status(err))

	require.Equal(t, 2.0, counted(t, reg, "GET /v1/hosts", "200"))
	require.Equal(t, 1.0, counted(t, reg, "POST /v1/snapshot", "403"), "a refusal is counted under its status")
	require.Equal(t, 1.0, counted(t, reg, "GET /v1/networks/{name}", "404"), "by the route pattern, not the path")
}
