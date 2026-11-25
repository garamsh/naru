package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/wire"
)

func TestProbeProxy_IsHealthyOnlyWhenTheProxyAnswers200(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(int(status.Load()))
	}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	probe := probeProxy(addr)
	assert.True(t, probe(context.Background()))
	status.Store(http.StatusInternalServerError)
	assert.False(t, probe(context.Background()), "a proxy that answers otherwise")
	srv.Close()
	assert.False(t, probe(context.Background()), "a proxy that does not answer")
}

func TestProxyCheck_IsUnhealthyOnlyAfterThreeFailedProbesInARowAndHealthyAfterOneSuccess(t *testing.T) {
	var h proxyCheck
	ctx := context.Background()
	assert.False(t, h.Healthy(ctx), "unhealthy until a probe succeeds")
	h.observe(false)
	assert.False(t, h.Healthy(ctx), "a probe that fails before any success")
	h.observe(true)
	assert.True(t, h.Healthy(ctx), "healthy from one success")
	h.observe(false)
	assert.True(t, h.Healthy(ctx), "one missed probe")
	h.observe(false)
	assert.True(t, h.Healthy(ctx), "two missed probes")
	h.observe(true)
	h.observe(false)
	h.observe(false)
	assert.True(t, h.Healthy(ctx), "a success between counts the failures again")
	h.observe(false)
	assert.False(t, h.Healthy(ctx), "three missed probes in a row")
	h.observe(true)
	assert.True(t, h.Healthy(ctx), "healthy again after one success")
}

func TestProxyCheck_RunProbesAtOnceAndThenEveryInterval(t *testing.T) {
	// run probes with interval until it ends, and returns the probes it made.
	run := func(interval time.Duration, until func(h *proxyCheck, probes int32) bool) {
		t.Helper()
		var h proxyCheck
		var probes atomic.Int32
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			h.run(ctx, func(context.Context) bool { probes.Add(1); return true }, interval)
			close(done)
		}()
		assert.Eventually(t, func() bool { return until(&h, probes.Load()) }, time.Second, time.Millisecond)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("run does not end with its context")
		}
	}
	run(time.Hour, func(h *proxyCheck, _ int32) bool { return h.Healthy(context.Background()) })
	run(10*time.Millisecond, func(_ *proxyCheck, probes int32) bool { return probes >= 3 })
}

func TestProxyFlags_TakeTheHealthAddressAndTheTLSPortTogether(t *testing.T) {
	p, err := proxyFlags("", 0)
	require.NoError(t, err)
	assert.Equal(t, network.ProxyPorts{}, p, "neither flag: the host runs no proxy")
	p, err = proxyFlags("127.0.0.1:9443", 8443)
	require.NoError(t, err)
	assert.Equal(t, network.ProxyPorts{TLS: 8443, Redirect: wire.ProxyRedirectPort}, p)
	_, err = proxyFlags("127.0.0.1:9443", 65535)
	require.NoError(t, err, "the highest port")
	for _, c := range []struct {
		health string
		port   int
	}{{"127.0.0.1:9443", 0}, {"", 8443}, {"127.0.0.1:9443", -1}, {"127.0.0.1:9443", 65536}} {
		_, err := proxyFlags(c.health, c.port)
		assert.Error(t, err, "health %q, port %d", c.health, c.port)
	}
}
