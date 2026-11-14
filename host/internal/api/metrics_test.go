package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
)

func (e *env) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	promhttp.HandlerFor(e.metrics, promhttp.HandlerOpts{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

func TestMetrics_RequestsByRouteAndCode(t *testing.T) {
	e := start(t)
	reader := e.as(t, ReaderOrganization)
	e.do(t, reader, "GET", "/v1/host", nil)
	e.do(t, reader, "GET", "/v1/host", nil)
	e.do(t, reader, "GET", "/v1/microvms/missing", nil)
	e.do(t, reader, "POST", "/v1/microvms", createBody())
	e.do(t, e.as(t, ProvisionerOrganization), "POST", "/v1/microvms", createBody())

	body := e.scrape(t)
	assert.Contains(t, body, `naru_hostd_requests_total{code="200",route="GET /v1/host"} 2`)
	assert.Contains(t, body, `naru_hostd_requests_total{code="404",route="GET /v1/microvms/{name}"} 1`)
	assert.Contains(t, body, `naru_hostd_requests_total{code="201",route="POST /v1/microvms"} 1`)
	// The refusal for the role is never routed, and is told apart from the accepted request.
	assert.Contains(t, body, `naru_hostd_requests_total{code="403",route="none"} 1`)
}

func TestMetrics_MicroVMsByStateAreReadOnEachScrape(t *testing.T) {
	e := start(t)
	e.rt.machines["a"] = mvmd.Machine{ID: "a", State: mvmd.StateRunning}
	e.rt.machines["b"] = mvmd.Machine{ID: "b", State: mvmd.StateRunning}
	e.rt.machines["c"] = mvmd.Machine{ID: "c", State: mvmd.StateStopped}

	body := e.scrape(t)
	assert.Contains(t, body, `naru_hostd_microvms{state="running"} 2`)
	assert.Contains(t, body, `naru_hostd_microvms{state="stopped"} 1`)
	assert.Contains(t, body, `naru_hostd_microvms{state="faulted"} 0`)

	e.rt.mu.Lock()
	delete(e.rt.machines, "a")
	e.rt.mu.Unlock()
	assert.Contains(t, e.scrape(t), `naru_hostd_microvms{state="running"} 1`)
}

func TestMetrics_AScrapeThatCannotListReportsNoMicroVMs(t *testing.T) {
	e := start(t)
	e.rt.machines["a"] = mvmd.Machine{ID: "a", State: mvmd.StateRunning}
	require.Contains(t, e.scrape(t), `naru_hostd_microvms{state="running"} 1`)

	e.rt.err = errors.New("mvmd is down")
	assert.NotContains(t, e.scrape(t), "naru_hostd_microvms{")
}
