package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/garamsh/naru/host/internal/network"
	"github.com/garamsh/naru/lib/wire"
)

const (
	// proxyProbeTimeout bounds one ask of the proxy's health.
	proxyProbeTimeout = time.Second
	// proxyProbeInterval is how often hostd asks the proxy, the interval of orch's pass.
	proxyProbeInterval = 5 * time.Second
	// proxyFailures is how many probes in a row must fail before the proxy is reported unhealthy, so
	// one slow answer does not move a proxy exposure's address and move it back.
	proxyFailures = 3
)

// proxyFlags are the proxy's ports from --proxy-health-addr and --proxy-tls-port, which go
// together; none when both are empty. maxAge, --proxy-route-max-age, is what a proxy may be given.
func proxyFlags(health string, tlsPort int, maxAge time.Duration) (network.ProxyPorts, error) {
	if maxAge <= 0 || maxAge > wire.ProxyMaxRouteMaxAge {
		return network.ProxyPorts{}, errors.New("--proxy-route-max-age must be positive and " + wire.ProxyMaxRouteMaxAge.String() + " at most")
	}
	if (health == "") != (tlsPort == 0) || tlsPort < 0 || tlsPort > 65535 {
		return network.ProxyPorts{}, errors.New("--proxy-health-addr and --proxy-tls-port go together, and the port is 1 to 65535")
	}
	if health == "" {
		return network.ProxyPorts{}, nil
	}
	return network.ProxyPorts{TLS: uint16(tlsPort), Redirect: wire.ProxyRedirectPort}, nil
}

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

// proxyCheck is the proxy's health as GET /v1/host reports it: unhealthy until a probe succeeds,
// healthy from a probe that succeeds, and unhealthy again only after proxyFailures probes in a row
// fail.
type proxyCheck struct {
	mu       sync.Mutex
	healthy  bool
	failures int
}

// observe takes one probe's result.
func (h *proxyCheck) observe(ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ok {
		h.healthy, h.failures = true, 0
		return
	}
	h.failures++
	if h.failures >= proxyFailures {
		h.healthy = false
	}
}

// Healthy is the last state observe left, for api.Deps.ProxyHealthy; it does not probe.
func (h *proxyCheck) Healthy(context.Context) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.healthy
}

// run probes at once and then every interval until ctx ends.
func (h *proxyCheck) run(ctx context.Context, probe func(context.Context) bool, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.observe(probe(ctx))
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
