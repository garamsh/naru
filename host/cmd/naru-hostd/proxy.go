package main

import (
	"context"
	"net/http"
	"time"
)

// proxyProbeTimeout bounds one ask of the proxy's health, so /v1/host answers within it.
const proxyProbeTimeout = time.Second

// probeProxy returns the check of the naru-proxy whose health endpoint is at addr: whether it
// answers GET /healthz with 200 within proxyProbeTimeout.
func probeProxy(addr string) func(context.Context) bool {
	client := &http.Client{Timeout: proxyProbeTimeout}
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/healthz", nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
}
