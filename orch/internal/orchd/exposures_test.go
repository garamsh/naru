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

// exposureHosts is one host with a LAN interface and a microVM on each of the networks a and b.
type exposureHosts struct {
	mu      sync.Mutex
	leaseds map[string]exposures.Lease
}

func (e *exposureHosts) List(context.Context) ([]exposures.Host, error) {
	return []exposures.Host{{ID: "h1", LAN: "eth0", Networks: []string{"a", "b"}}}, nil
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

func TestAnExposerIsScopedToItsNetworkAndTheLeaderLeasesItsAddress(t *testing.T) {
	eh := &exposureHosts{leaseds: map[string]exposures.Lease{}}
	o := startOne(t, orchtest.Options{
		NewHosts:         func(inventory.Service) groups.Hosts { return orchtest.NewFakeHosts("h1") },
		NewExposureHosts: func(inventory.Service) orchd.ExposureHosts { return eh },
	})
	ctx := t.Context()
	for _, n := range []orchapi.NetworkSpec{{Prefix: "10.1.0.0/24"}, {Prefix: "10.2.0.0/24"}} {
		name := map[string]string{"10.1.0.0/24": "a", "10.2.0.0/24": "b"}[n.Prefix]
		_, err := o.op.ApplyNetwork(ctx, name, n)
		require.NoError(t, err)
	}
	_, err := o.op.ApplyAddressPool(ctx, "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)

	a := o.c.CA.Issue(t, identity.ExposerPrefix+"a", identity.RoleExposer)
	b := o.c.CA.Issue(t, identity.ExposerPrefix+"b", identity.RoleExposer)
	ca, cb := o.c.Client(t, 0, &a), o.c.Client(t, 0, &b)
	spec := func(network string, backend string) orchapi.ExposureSpec {
		return orchapi.ExposureSpec{Network: network, Pool: "lan", Backends: []string{backend}, Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, NodePort: 30443}}}
	}

	e, err := ca.ApplyExposure(ctx, "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e.Address)
	require.Eventually(t, func() bool {
		l, ok := eh.lease(e.Address)
		return ok && l.Network == "a" && l.Seconds == exposures.LeaseSeconds
	}, 30*time.Second, 50*time.Millisecond, "the leader leases the address to a host")

	// 403: another network, in the body.
	_, err = ca.ApplyExposure(ctx, "other", spec("b", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the spec names another network")
	// 403: a pool that does not list the exposer's network.
	_, err = cb.ApplyExposure(ctx, "web", spec("b", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the pool lan does not list network b")
	// 403: another network's exposure, read and deleted; the list shows the caller's own only.
	_, err = cb.Exposure(ctx, "web")
	require.Equal(t, 403, status(err), "read another network's exposure")
	require.Equal(t, 403, status(cb.DeleteExposure(ctx, "web")), "delete another network's exposure")
	none, err := cb.Exposures(ctx)
	require.NoError(t, err)
	require.Empty(t, none)
	// 403: the role reads nothing but exposures, and writes no pool.
	_, err = ca.Hosts(ctx)
	require.Equal(t, 403, status(err), "reads hosts")
	_, err = ca.MicroVMs(ctx)
	require.Equal(t, 403, status(err), "reads microVMs")
	_, err = ca.Groups(ctx)
	require.Equal(t, 403, status(err), "reads groups")
	_, err = ca.AddressPools(ctx)
	require.Equal(t, 403, status(err), "reads pools")
	_, err = ca.ApplyAddressPool(ctx, "mine", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.9.0/29"}, Networks: []string{"a"}})
	require.Equal(t, 403, status(err), "writes a pool")
	// Neither a reader nor an operator writes an exposure.
	_, err = o.op.ApplyExposure(ctx, "web", spec("a", "10.1.0.5"))
	require.Equal(t, 403, status(err), "an operator is no exposer")
	_, err = o.reader.ApplyExposure(ctx, "web", spec("a", "10.1.0.5"))
	require.Equal(t, 403, status(err), "a reader is no exposer")
	all, err := o.reader.Exposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "a reader sees every network's exposures")

	// An exposer whose Subject names no network is refused.
	bare := o.c.CA.Issue(t, "exposer.", identity.RoleExposer)
	_, err = o.c.Client(t, 0, &bare).Exposures(ctx)
	require.Equal(t, 403, status(err))

	require.NoError(t, ca.DeleteExposure(ctx, "web"))
	require.Eventually(t, func() bool { _, ok := eh.lease(e.Address); return !ok }, 30*time.Second, 50*time.Millisecond, "the orphan lease is removed")
}
