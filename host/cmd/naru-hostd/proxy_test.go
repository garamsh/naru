package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
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
