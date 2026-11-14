package metrics_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/metrics"
)

func freeAddr(t *testing.T) string {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer probe.Close()
	return probe.Addr().String()
}

func get(addr, path string) (int, string, error) {
	resp, err := http.Get("http://" + addr + path)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), nil
}

func TestServe_ServesTheRegistryAndStopsWithItsContext(t *testing.T) {
	reg := metrics.New()
	up := prometheus.NewGauge(prometheus.GaugeOpts{Name: "naru_test_up", Help: "u"})
	reg.MustRegister(up)
	up.Set(1)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- metrics.Serve(ctx, addr, reg) }()

	var body string
	require.Eventually(t, func() bool {
		code, b, err := get(addr, "/metrics")
		body = b
		return err == nil && code == http.StatusOK
	}, 5*time.Second, 20*time.Millisecond)
	assert.Contains(t, body, "naru_test_up 1\n")
	assert.Contains(t, body, "go_goroutines", "the Go collector is registered by New")
	assert.Contains(t, body, "process_open_fds", "the process collector is registered by New")

	code, _, err := get(addr, "/other")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, code)

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after its context ended")
	}
}

func TestServe_ReportsAnAddressItCannotListenOn(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer busy.Close()

	assert.Error(t, metrics.Serve(context.Background(), busy.Addr().String(), metrics.New()))
}
