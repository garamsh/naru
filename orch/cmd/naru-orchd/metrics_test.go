package main

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
)

func TestServeMetrics_ServesTheMemberAndProcessMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go serveMetrics(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), addr, newRegistry("host-1"))

	var body string
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond)
	assert.Contains(t, body, `naru_orchd_info{member="host-1"} 1`)
	assert.Contains(t, body, "naru_orchd_start_time_seconds")
	assert.Contains(t, body, "process_resident_memory_bytes")
}
