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
	return []exposures.Host{{ID: "h1", LAN: "eth0", Networks: []string{"a", "b"}, Members: map[string][]string{"a": {"10.1.0.5"}, "b": {"10.2.0.5"}}}}, nil
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
	// lan lists network a only; wide lists both.
	_, err := o.op.ApplyAddressPool(ctx, "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	_, err = o.op.ApplyAddressPool(ctx, "wide", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.80/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)

	a := o.c.CA.Issue(t, identity.ExposerPrefix+"a", identity.RoleExposer)
	b := o.c.CA.Issue(t, identity.ExposerPrefix+"b", identity.RoleExposer)
	ca, cb := o.c.Client(t, 0, &a), o.c.Client(t, 0, &b)
	spec := func(network, pool, backend string) orchapi.ExposureSpec {
		return orchapi.ExposureSpec{Network: network, Pool: pool, Backends: []string{backend}, Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, NodePort: 30443}}}
	}

	e, err := ca.ApplyExposure(ctx, "web", spec("a", "lan", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e.Address)
	require.Eventually(t, func() bool {
		l, ok := eh.lease(e.Address)
		return ok && l.Network == "a" && l.Seconds == exposures.LeaseSeconds
	}, 30*time.Second, 50*time.Millisecond, "the leader leases the address to a host")

	// The body names another network, in a pool that lists both, so only the body is wrong.
	_, err = ca.ApplyExposure(ctx, "other", spec("b", "wide", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the spec names another network")
	_, err = ca.ApplyExposure(ctx, "other", spec("a", "wide", "10.1.0.5"))
	require.NoError(t, err, "control: the same pool, the caller's own network")

	// The body is the caller's own network, so only the pool is wrong.
	_, err = cb.ApplyExposure(ctx, "web", spec("b", "lan", "10.2.0.5"))
	require.Equal(t, 403, status(err), "the pool lan does not list network b")
	_, err = cb.ApplyExposure(ctx, "web", spec("b", "wide", "10.2.0.5"))
	require.NoError(t, err, "control: a pool that lists network b")

	// Another network's id is not there: the same answer as for an id nobody holds.
	_, err = ca.Exposure(ctx, "only-a")
	require.Equal(t, 404, status(err), "absent")
	_, err = ca.ApplyExposure(ctx, "only-a", spec("a", "lan", "10.1.0.5"))
	require.NoError(t, err)
	_, err = cb.Exposure(ctx, "only-a")
	require.Equal(t, 404, status(err), "another network's exposure reads like an absent one")
	got, err := ca.Exposure(ctx, "only-a")
	require.NoError(t, err, "control: its own network reads it")
	require.Equal(t, "a", got.Network)
	require.NoError(t, cb.DeleteExposure(ctx, "only-a"), "a delete of another network's id is a no-op")
	still, err := ca.Exposure(ctx, "only-a")
	require.NoError(t, err)
	require.Equal(t, got.Address, still.Address, "and it left the exposure alone")
	// b's own "web" is b's: a's "web" is as it was.
	webA, err := ca.Exposure(ctx, "web")
	require.NoError(t, err)
	require.Equal(t, e.Address, webA.Address)
	require.Equal(t, []string{"10.1.0.5"}, webA.Backends)

	// The list shows the caller's own only.
	mine, err := cb.Exposures(ctx)
	require.NoError(t, err)
	require.Len(t, mine, 1)
	require.Equal(t, "b", mine[0].Network)

	// The role reads nothing but exposures, and writes no pool; a reader reads what it should.
	_, err = ca.Hosts(ctx)
	require.Equal(t, 403, status(err), "reads hosts")
	_, err = o.reader.Hosts(ctx)
	require.NoError(t, err, "control: a reader reads hosts")
	_, err = ca.Groups(ctx)
	require.Equal(t, 403, status(err), "reads groups")
	_, err = o.reader.Groups(ctx)
	require.NoError(t, err, "control: a reader reads groups")
	_, err = ca.AddressPools(ctx)
	require.Equal(t, 403, status(err), "reads pools")
	_, err = o.reader.AddressPools(ctx)
	require.NoError(t, err, "control: a reader reads pools")
	_, err = ca.ApplyAddressPool(ctx, "mine", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.9.0/29"}, Networks: []string{"a"}})
	require.Equal(t, 403, status(err), "writes a pool")
	_, err = o.op.ApplyAddressPool(ctx, "mine", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.9.0/29"}, Networks: []string{"a"}})
	require.NoError(t, err, "control: an operator writes the same pool")

	// Neither a reader nor an operator writes an exposure, and a reader gets one by id from no one.
	_, err = o.op.ApplyExposure(ctx, "x", spec("a", "lan", "10.1.0.5"))
	require.Equal(t, 403, status(err), "an operator is no exposer")
	_, err = o.reader.ApplyExposure(ctx, "x", spec("a", "lan", "10.1.0.5"))
	require.Equal(t, 403, status(err), "a reader is no exposer")
	_, err = o.reader.Exposure(ctx, "web")
	require.Equal(t, 403, status(err), "a reader names no network for an id")
	all, err := o.reader.Exposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 4, "a reader sees every network's exposures")

	// An exposer whose Subject names no network is refused.
	bare := o.c.CA.Issue(t, "exposer.", identity.RoleExposer)
	_, err = o.c.Client(t, 0, &bare).Exposures(ctx)
	require.Equal(t, 403, status(err))

	// An operator releases a network's exposure by the network and id; an exposer may not.
	err = ca.DeleteNetworkExposure(ctx, "a", "only-a")
	require.Equal(t, 403, status(err), "an exposer is no operator")
	err = o.reader.DeleteNetworkExposure(ctx, "a", "only-a")
	require.Equal(t, 403, status(err), "nor is a reader")
	_, err = ca.Exposure(ctx, "only-a")
	require.NoError(t, err, "the refused calls left it")
	require.NoError(t, o.op.DeleteNetworkExposure(ctx, "a", "only-a"), "control: an operator")
	require.Eventually(t, func() bool { _, ok := eh.lease(still.Address); return !ok }, 30*time.Second, 50*time.Millisecond, "the host is told to remove the lease")
	require.Eventually(t, func() bool { _, err := ca.Exposure(ctx, "only-a"); return status(err) == 404 }, 30*time.Second, 50*time.Millisecond, "and the record goes once it confirmed")
	require.NoError(t, o.op.DeleteNetworkExposure(ctx, "a", "only-a"), "a second delete is a no-op")

	require.NoError(t, ca.DeleteExposure(ctx, "web"))
	require.Eventually(t, func() bool { _, ok := eh.lease(e.Address); return !ok }, 30*time.Second, 50*time.Millisecond, "the lease is removed once the host confirmed")
	require.Eventually(t, func() bool {
		list, err := o.reader.Exposures(ctx)
		return err == nil && len(list) == 2
	}, 30*time.Second, 50*time.Millisecond, "the reconciler then deletes the record")
}

func TestMembersWithoutHostsAnswer501(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	ex := o.c.CA.Issue(t, identity.ExposerPrefix+"a", identity.RoleExposer)
	_, err := o.c.Client(t, 0, &ex).Exposures(t.Context())
	require.Equal(t, 501, status(err))
	_, err = o.op.ApplyNetwork(t.Context(), "a", orchapi.NetworkSpec{Prefix: "10.1.0.0/24"})
	require.NoError(t, err, "control: the member serves its other endpoints")
}

func (e *exposureHosts) Machines(_ context.Context, network string) ([]exposures.Machine, error) {
	return []exposures.Machine{{Name: "m-" + network, Host: "h1", Address: map[string]string{"a": "10.1.0.5", "b": "10.2.0.5"}[network], Running: true}}, nil
}

func TestAnExposerListsOnlyItsOwnNetworksMembers(t *testing.T) {
	o := startOne(t, orchtest.Options{
		NewHosts: func(inventory.Service) groups.Hosts { return orchtest.NewFakeHosts("h1") },
		NewExposureHosts: func(inventory.Service) orchd.ExposureHosts {
			return &exposureHosts{leaseds: map[string]exposures.Lease{}}
		},
	})
	ctx := t.Context()
	for name, prefix := range map[string]string{"a": "10.1.0.0/24", "b": "10.2.0.0/24"} {
		_, err := o.op.ApplyNetwork(ctx, name, orchapi.NetworkSpec{Prefix: prefix})
		require.NoError(t, err)
	}
	ex := o.c.CA.Issue(t, identity.ExposerPrefix+"a", identity.RoleExposer)
	ca := o.c.Client(t, 0, &ex)

	got, err := ca.NetworkMembers(ctx, "a")
	require.NoError(t, err)
	require.Empty(t, got, "no group has a member on the network")
	_, err = ca.NetworkMembers(ctx, "b")
	require.Equal(t, 404, status(err), "another network's members are not there")
	got, err = o.reader.NetworkMembers(ctx, "b")
	require.NoError(t, err, "control: a reader lists any network")
	require.Empty(t, got)
	_, err = o.reader.NetworkMembers(ctx, "nope")
	require.Equal(t, 404, status(err), "control: an unknown network")
	_, err = o.anon.NetworkMembers(ctx, "a")
	require.Equal(t, 401, status(err))
}
