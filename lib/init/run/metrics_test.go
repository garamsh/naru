package run

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/config"
	"github.com/garamsh/naru/lib/init/service"
)

type fakeStatuses []service.Status

func (f fakeStatuses) Status() []service.Status { return f }

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().String()
}

func scrape(t *testing.T, addr string) string {
	t.Helper()
	var body []byte
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		body, _ = io.ReadAll(resp.Body)
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond)
	return string(body)
}

func TestServeMetrics_ReportsServiceStatesAndRestartsAndProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	from := fakeStatuses{
		{Name: "naru-hostd", State: service.StateRunning, PID: 7, Restarts: 2},
		{Name: "naru-orchd", State: service.StateBackoff, Restarts: 5},
	}

	serveMetrics(ctx, addr, from, slog.New(slog.NewTextHandler(io.Discard, nil)))
	body := scrape(t, addr)

	assert.Contains(t, body, `naru_init_service_state{service="naru-hostd",state="running"} 1`)
	assert.Contains(t, body, `naru_init_service_state{service="naru-hostd",state="backoff"} 0`)
	assert.Contains(t, body, `naru_init_service_state{service="naru-orchd",state="backoff"} 1`)
	assert.Contains(t, body, `naru_init_service_restarts{service="naru-hostd"} 2`)
	assert.Contains(t, body, `naru_init_service_restarts{service="naru-orchd"} 5`)
	assert.Contains(t, body, "process_resident_memory_bytes")
}

func TestServeMetrics_WithNoAddressServesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)

	serveMetrics(ctx, config.Spec{}.MetricsAddr(), fakeStatuses{}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	assert.Empty(t, config.Spec{}.MetricsAddr(), "a spec without metrics names no address")
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err, "nothing took the address")
	_ = ln.Close()
}
