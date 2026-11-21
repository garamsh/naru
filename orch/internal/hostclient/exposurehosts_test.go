package hostclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/hostclient"
)

// exposureAPI answers for one host: its microVMs, or nothing until the call's context ends.
type exposureAPI struct {
	vms   []hostapi.MicroVM
	hangs bool
}

func (a *exposureAPI) Host(ctx context.Context) (hostapi.Host, error) {
	if a.hangs {
		<-ctx.Done()
		return hostapi.Host{}, ctx.Err()
	}
	return hostapi.Host{LANInterface: "eth0"}, ctx.Err()
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
		{ID: "h1", LAN: "eth0", Networks: []string{"a"}},
		{ID: "h2", LAN: "eth0"},
		{ID: "h3", LAN: "eth0", Networks: []string{"b"}},
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
	require.Len(t, hosts, 2, "h2 and h3 answered, h1 is left out")
	require.Equal(t, "h2", hosts[0].ID)
	require.Equal(t, "h3", hosts[1].ID)
}
