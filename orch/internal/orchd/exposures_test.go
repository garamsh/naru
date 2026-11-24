package orchd_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// exposureHosts is one host with a LAN interface and a microVM on each of the networks a and b,
// and with proxy a healthy naru-proxy.
type exposureHosts struct {
	mu      sync.Mutex
	leaseds map[string]exposures.Lease
	proxy   bool
}

func (e *exposureHosts) List(context.Context) ([]exposures.Host, error) {
	return []exposures.Host{{ID: "h1", LAN: "eth0", Networks: []string{"a", "b"}, Members: map[string][]string{"a": {"10.1.0.5"}, "b": {"10.2.0.5"}}, Proxy: e.proxy}}, nil
}

func (e *exposureHosts) Put(_ context.Context, _, address string, l exposures.Lease) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.leaseds[address] = l
	return nil
}

func (e *exposureHosts) Held(context.Context, string) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for a := range e.leaseds {
		out = append(out, a)
	}
	return out, nil
}

func (e *exposureHosts) Delete(_ context.Context, _, address string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.leaseds, address)
	return nil
}

func (e *exposureHosts) Addresses(_ context.Context, network string) ([]string, error) {
	return map[string][]string{"a": {"10.1.0.5"}, "b": {"10.2.0.5"}}[network], nil
}

func (e *exposureHosts) lease(address string) (exposures.Lease, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.leaseds[address]
	return l, ok
}

func TestTheOperatorsExposureIsScopedToItsNetworkAndPoolAndTheLeaderLeasesItsAddress(t *testing.T) {
	eh := &exposureHosts{leaseds: map[string]exposures.Lease{}}
	o := startOne(t, orchtest.Options{
		NewHosts:         func(inventory.Service) groups.Hosts { return orchtest.NewFakeHosts("h1") },
		NewExposureHosts: func(inventory.Service) orchd.ExposureHosts { return eh },
	})
	ctx := t.Context()
	for name, prefix := range map[string]string{"a": "10.1.0.0/24", "b": "10.2.0.0/24"} {
		_, err := o.op.ApplyNetwork(ctx, name, orchapi.NetworkSpec{Prefix: prefix})
		require.NoError(t, err)
	}
	// lan lists network a only; wide lists both.
	_, err := o.op.ApplyAddressPool(ctx, "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	_, err = o.op.ApplyAddressPool(ctx, "wide", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.80/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)
	spec := func(network, pool, backend string) orchapi.ExposureSpec {
		return orchapi.ExposureSpec{Network: network, Pool: pool, Backends: []string{backend}, Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, BackendPort: 6443}}}
	}

	e, err := o.op.ApplyNetworkExposure(ctx, "a", "web", spec("a", "lan", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e.Address)
	require.Equal(t, orchapi.OwnerOperator, e.Owner)
	require.Eventually(t, func() bool {
		l, ok := eh.lease(e.Address)
		return ok && l.Network == "a" && l.Seconds == exposures.LeaseSeconds
	}, 30*time.Second, 50*time.Millisecond, "the leader leases the address to a host")

	// The body names another network than the path, in a pool that lists both, so only the body is wrong.
	_, err = o.op.ApplyNetworkExposure(ctx, "a", "other", spec("b", "wide", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the spec names another network")
	_, err = o.op.ApplyNetworkExposure(ctx, "a", "other", spec("a", "wide", "10.1.0.5"))
	require.NoError(t, err, "control: the same pool, the path's network")

	// The body is the path's network, so only the pool is wrong.
	_, err = o.op.ApplyNetworkExposure(ctx, "b", "web", spec("b", "lan", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the pool lan does not list network b")
	_, err = o.op.ApplyNetworkExposure(ctx, "b", "web", spec("b", "wide", "10.2.0.5"))
	require.NoError(t, err, "control: a pool that lists network b")

	// Another network's id is not there: b's "web" is b's, and a's is as it was.
	webA, err := o.reader.NetworkExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, e.Address, webA.Address)
	require.Equal(t, []string{"10.1.0.5"}, webA.Backends)
	_, err = o.reader.NetworkExposure(ctx, "b", "other")
	require.Equal(t, 404, status(err), "a's other is not b's")

	// A reader writes no exposure and reads every network's.
	_, err = o.reader.ApplyNetworkExposure(ctx, "a", "x", spec("a", "lan", "10.1.0.5"))
	require.Equal(t, 403, status(err), "a reader writes no exposure")
	all, err := o.reader.Exposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3, "a reader sees every network's exposures")

	// A certificate of the exposer role, which naru no longer issues, is not let in.
	ex := o.c.CA.Issue(t, "exposer.a", identity.Role("exposer"))
	_, err = o.c.Client(t, 0, &ex).Exposures(ctx)
	require.Error(t, err)
	require.NotEqual(t, 501, status(err), "refused before the call is served")

	// An operator releases a network's exposure by the network and id; a reader may not.
	err = o.reader.DeleteNetworkExposure(ctx, "a", "other")
	require.Equal(t, 403, status(err), "a reader is no operator")
	require.NoError(t, o.op.DeleteNetworkExposure(ctx, "a", "web"))
	require.Eventually(t, func() bool { _, ok := eh.lease(e.Address); return !ok }, 30*time.Second, 50*time.Millisecond, "the host is told to remove the lease")
	require.Eventually(t, func() bool { _, err := o.reader.NetworkExposure(ctx, "a", "web"); return status(err) == 404 }, 30*time.Second, 50*time.Millisecond, "and the record goes once it confirmed")
	require.NoError(t, o.op.DeleteNetworkExposure(ctx, "a", "web"), "a second delete is a no-op")
	require.Eventually(t, func() bool {
		list, err := o.reader.Exposures(ctx)
		return err == nil && len(list) == 2
	}, 30*time.Second, 50*time.Millisecond, "the reconciler then deletes the record")
}

func TestAProxyExposureDrawsFromAPoolAndTheLeaderLeasesItToAProxyHost(t *testing.T) {
	eh := &exposureHosts{leaseds: map[string]exposures.Lease{}, proxy: true}
	o := startOne(t, orchtest.Options{
		NewHosts:         func(inventory.Service) groups.Hosts { return orchtest.NewFakeHosts("h1") },
		NewExposureHosts: func(inventory.Service) orchd.ExposureHosts { return eh },
	})
	ctx := t.Context()
	_, err := o.op.ApplyNetwork(ctx, "a", orchapi.NetworkSpec{Prefix: "10.1.0.0/24"})
	require.NoError(t, err)
	_, err = o.op.ApplyAddressPool(ctx, "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	_, err = o.op.ApplyAddressPool(ctx, "edge", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)

	_, err = o.op.ApplyProxyExposure(ctx, "web", orchapi.ProxyExposureSpec{Pool: "none", Ports: []int{443}})
	require.Equal(t, 403, status(err), "no such pool")
	_, err = o.reader.ApplyProxyExposure(ctx, "web", orchapi.ProxyExposureSpec{Pool: "edge", Ports: []int{443}})
	require.Equal(t, 403, status(err), "a reader makes no proxy exposure")
	_, err = o.op.ApplyProxyExposure(ctx, "web", orchapi.ProxyExposureSpec{Pool: "edge", Ports: []int{80}})
	require.Equal(t, 400, status(err), "a proxy exposure carries 443")

	e, err := o.op.ApplyProxyExposure(ctx, "web", orchapi.ProxyExposureSpec{Pool: "edge", Ports: []int{443, 80}})
	require.NoError(t, err)
	require.Equal(t, "192.168.1.80", e.Address)
	require.Equal(t, []int{443, 80}, e.Ports)
	require.Eventually(t, func() bool {
		l, ok := eh.lease(e.Address)
		return ok && l.ToProxy && l.Network == "" && len(l.Backends) == 0 && len(l.Ports) == 2
	}, 30*time.Second, 50*time.Millisecond, "the leader leases the address to the proxy's host")
	require.Eventually(t, func() bool {
		got, err := o.reader.ProxyExposure(ctx, "web")
		return err == nil && got.Host == "h1" && got.Phase == orchapi.ExposureAnnounced
	}, 30*time.Second, 50*time.Millisecond)

	// A network's exposure of the same id is its network's, and the two listings are apart.
	_, err = o.op.ApplyNetworkExposure(ctx, "a", "web", orchapi.ExposureSpec{Network: "a", Pool: "lan", Backends: []string{"10.1.0.5"}, Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, BackendPort: 6443}}})
	require.NoError(t, err)
	list, err := o.reader.ProxyExposures(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	all, err := o.reader.Exposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the networks' listing holds no proxy exposure")

	e, err = o.op.ApplyProxyExposure(ctx, "web", orchapi.ProxyExposureSpec{Pool: "edge", Ports: []int{443}})
	require.NoError(t, err, "its ports change in place")
	require.Equal(t, "192.168.1.80", e.Address)
	_, err = o.op.ApplyAddressPool(ctx, "edge", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.88/29"}, Networks: []string{"a"}})
	require.Equal(t, 409, status(err), "a pool keeps the address a proxy exposure holds")

	require.Equal(t, 403, status(o.reader.DeleteProxyExposure(ctx, "web")), "a reader is no operator")
	require.NoError(t, o.op.DeleteProxyExposure(ctx, "web"))
	require.Eventually(t, func() bool { _, ok := eh.lease("192.168.1.80"); return !ok }, 30*time.Second, 50*time.Millisecond, "the host is told to remove the lease")
	require.Eventually(t, func() bool { _, err := o.reader.ProxyExposure(ctx, "web"); return status(err) == 404 }, 30*time.Second, 50*time.Millisecond)
	webA, err := o.reader.NetworkExposure(ctx, "a", "web")
	require.NoError(t, err, "control: network a's web is untouched")
	require.Equal(t, "192.168.0.80", webA.Address)
}

func TestExposuresWithoutHostsAnswer501(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	_, err := o.reader.Exposures(t.Context())
	require.Equal(t, 501, status(err))
	_, err = o.op.ApplyNetwork(t.Context(), "a", orchapi.NetworkSpec{Prefix: "10.1.0.0/24"})
	require.NoError(t, err, "control: the member serves its other endpoints")
}
