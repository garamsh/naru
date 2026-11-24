package hostclient_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/hostclient"
)

// exposureAPI answers for one host: its microVMs, or nothing until the call's context ends.
type exposureAPI struct {
	vms   []hostapi.MicroVM
	hangs bool
	proxy *hostapi.ProxyStatus
}

func (a *exposureAPI) Host(ctx context.Context) (hostapi.Host, error) {
	if a.hangs {
		<-ctx.Done()
		return hostapi.Host{}, ctx.Err()
	}
	return hostapi.Host{LANInterface: "eth0", Proxy: a.proxy}, ctx.Err()
}

func (a *exposureAPI) MicroVMs(ctx context.Context) ([]hostapi.MicroVM, error) {
	if a.hangs {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return a.vms, ctx.Err()
}

func (a *exposureAPI) PutExposure(context.Context, string, hostapi.ExposureRequest) (hostapi.Exposure, error) {
	return hostapi.Exposure{}, nil
}
func (a *exposureAPI) Exposures(context.Context) ([]hostapi.Exposure, error) { return nil, nil }
func (a *exposureAPI) DeleteExposure(context.Context, string) error          { return nil }

func exposureHosts(apis map[string]*exposureAPI) *hostclient.ExposureHosts {
	var hosts []orchapi.Host
	for _, id := range []string{"h1", "h2", "h3"} {
		if apis[id] != nil {
			hosts = append(hosts, orchapi.Host{ID: id, Address: id + ":7443"})
		}
	}
	return hostclient.NewExposureHosts(&invFake{hosts: hosts}, func(id, _ string) hostclient.ExposureAPI { return apis[id] })
}

func TestOnlyRunningMicroVMsAreBackendsAndMakeAHostACandidate(t *testing.T) {
	apis := map[string]*exposureAPI{
		"h1": {vms: []hostapi.MicroVM{
			{Name: "up", Network: "a", State: "running", Addresses: []string{"10.1.0.5"}},
			{Name: "paused", Network: "a", State: "paused", Addresses: []string{"10.1.0.6"}},
			{Name: "dead", Network: "a", State: "stopped", Addresses: []string{"10.1.0.7"}},
			{Name: "broken", Network: "a", State: "failed", Addresses: []string{"10.1.0.8"}},
			{Name: "booting", Network: "a", State: "created", Addresses: []string{"10.1.0.9"}},
		}},
		"h2": {vms: []hostapi.MicroVM{{Name: "dead", Network: "a", State: "stopped", Addresses: []string{"10.1.0.10"}}}},
		"h3": {vms: []hostapi.MicroVM{{Name: "other", Network: "b", State: "running", Addresses: []string{"10.2.0.5"}}}},
	}
	h := exposureHosts(apis)
	got, err := h.Addresses(t.Context(), "a")
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"10.1.0.5", "10.1.0.6"}, got, "running and paused count; stopped, failed and created do not")
	other, err := h.Addresses(t.Context(), "b")
	require.NoError(t, err)
	require.Equal(t, []string{"10.2.0.5"}, other, "control: another network's running microVM is found")

	hosts, err := h.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []exposures.Host{
		{ID: "h1", LAN: "eth0", Networks: []string{"a"}, Members: map[string][]string{"a": {"10.1.0.5", "10.1.0.6"}}},
		{ID: "h2", LAN: "eth0"},
		{ID: "h3", LAN: "eth0", Networks: []string{"b"}, Members: map[string][]string{"b": {"10.2.0.5"}}},
	}, hosts, "h2 only has a stopped microVM on a")
}

func TestAHostThatDoesNotAnswerHoldsBackNoOtherProbe(t *testing.T) {
	apis := map[string]*exposureAPI{
		"h1": {hangs: true},
		"h2": {vms: []hostapi.MicroVM{{Name: "up", Network: "a", State: "running", Addresses: []string{"10.1.0.5"}}}},
		"h3": {vms: []hostapi.MicroVM{{Name: "up", Network: "a", State: "running", Addresses: []string{"10.1.0.6"}}}},
	}
	h := exposureHosts(apis)
	// The caller's deadline is shorter than one probe. Probed one after the other, the hung host
	// would use all of it and the hosts after it would be reported lost.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	// Each probe takes its own share; here the hung host ends at the caller's deadline, and the
	// others must have answered before it.
	done := make(chan struct{})
	var hosts []exposures.Host
	go func() {
		defer close(done)
		hosts, _ = h.List(ctx)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the probe waited on the hung host")
	}
	require.Len(t, hosts, 3, "h1 is listed, not dropped")
	require.Equal(t, "h1", hosts[0].ID)
	require.True(t, hosts[0].Silent, "as one that did not answer")
	require.Equal(t, "h2", hosts[1].ID)
	require.False(t, hosts[1].Silent)
	require.Equal(t, "h3", hosts[2].ID)
	require.False(t, hosts[2].Silent)
}

// The lease orch puts reaches the host API in the protocol's one spelling, which hostd's
// conformance test (lib/hostapi/hostapitest) shows it takes (ADR 0107).
func TestALeaseReachesTheHostInTheSharedProtocolSpelling(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	h := hostclient.NewExposureHosts(&invFake{hosts: []orchapi.Host{{ID: "h1", Address: "h1:7443"}}},
		func(string, string) hostclient.ExposureAPI { return hostapi.NewClient(srv.URL, srv.Client()) })
	for _, p := range wire.Protocols() {
		l := exposures.Lease{Network: "a", Ports: []orchapi.ExposurePort{{Protocol: p, Port: 443, BackendPort: 30443}}, Backends: []string{"10.1.0.5"}, Seconds: 15}
		require.NoError(t, h.Put(t.Context(), "h1", "192.168.0.80", l))
		var sent hostapi.ExposureRequest
		require.NoError(t, json.Unmarshal(body, &sent))
		require.Equal(t, []hostapi.ExposurePort{{Protocol: p, Port: 443, BackendPort: 30443}}, sent.Ports)
		require.Contains(t, string(body), `"protocol":"`+string(p)+`"`)
	}
}

func TestAHostIsAProxyCandidateOnlyWhileItsProxyIsHealthy(t *testing.T) {
	apis := map[string]*exposureAPI{
		"h1": {proxy: &hostapi.ProxyStatus{Healthy: true}},
		"h2": {proxy: &hostapi.ProxyStatus{Healthy: false}},
		"h3": {},
	}
	hosts, err := exposureHosts(apis).List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []exposures.Host{
		{ID: "h1", LAN: "eth0", Proxy: true},
		{ID: "h2", LAN: "eth0"},
		{ID: "h3", LAN: "eth0"},
	}, hosts, "a proxy that fails its check, and a host that runs none, are no candidates")
}

func TestAProxyExposuresLeaseReachesTheHostAsOne(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	h := hostclient.NewExposureHosts(&invFake{hosts: []orchapi.Host{{ID: "h1", Address: "h1:7443"}}},
		func(string, string) hostclient.ExposureAPI { return hostapi.NewClient(srv.URL, srv.Client()) })
	l := exposures.Lease{ToProxy: true, Ports: []orchapi.ExposurePort{{Protocol: wire.ProtocolTCP, Port: 443, BackendPort: 443}}, Seconds: 15}
	require.NoError(t, h.Put(t.Context(), "h1", "192.168.0.90", l))
	var sent hostapi.ExposureRequest
	require.NoError(t, json.Unmarshal(body, &sent))
	require.True(t, sent.ToProxy)
	require.Empty(t, sent.Network)
	require.Empty(t, sent.Backends)
}
