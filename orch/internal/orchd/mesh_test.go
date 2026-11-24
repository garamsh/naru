package orchd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/layout"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

const meshKeys = "/naru/orch/mesh-addresses/"

// meshProfileYAML is profileYAML with a mesh pool; an empty mesh leaves the profile without one.
func meshProfileYAML(t *testing.T, machine, mesh string) []byte {
	t.Helper()
	p := layout.HostProfile{
		FirecrackerSHA256: joinDigest,
		Orch:              layout.ProfileOrch{URL: "https://orch.example:8443", CASHA256: joinDigest},
		MachinePool:       netip.MustParsePrefix(machine),
	}
	if mesh != "" {
		pool := netip.MustParsePrefix(mesh)
		p.MeshPool = &pool
	}
	b, err := p.Marshal()
	require.NoError(t, err)
	return b
}

// meshAllocations is the mesh records by address, as etcd holds them.
func (o *one) meshAllocations(t *testing.T) map[string]inventory.Allocation {
	t.Helper()
	resp, err := o.c.Members[0].Etcd().Get(t.Context(), meshKeys, clientv3.WithPrefix())
	require.NoError(t, err)
	out := map[string]inventory.Allocation{}
	for _, kv := range resp.Kvs {
		var a inventory.Allocation
		require.NoError(t, json.Unmarshal(kv.Value, &a))
		out[a.Prefix] = a
	}
	return out
}

func TestAdmissionGivesEveryHostItsOwnMeshAddressAndRemovalFreesIt(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.99.0.0/24")))

	joined := o.admitSeeded(t, newHost(t, "h-0123456789abcdef"))
	own := newHost(t, "node-1")
	_, err := o.anon.Enroll(t.Context(), own.request(o.secret))
	require.NoError(t, err)
	plain := o.admit(t, own)

	assert.Equal(t, "10.99.0.1", joined.MeshAddress, "the pool gives up its first address, its own network address")
	assert.Equal(t, "10.99.0.2", plain.MeshAddress, "a host enrolled with its own configuration is given one as well")
	listed, err := o.reader.Hosts(t.Context())
	require.NoError(t, err)
	for _, h := range listed {
		assert.NotEmpty(t, h.MeshAddress, "%s: a reader sees the mesh address", h.ID)
	}
	held := o.meshAllocations(t)
	require.Len(t, held, 2)
	assert.Equal(t, inventory.Allocation{Prefix: "10.99.0.1/32", HostID: "h-0123456789abcdef"}, held["10.99.0.1/32"], "confirmed at admission")
	assert.Equal(t, "node-1", held["10.99.0.2/32"].HostID)
	assert.Len(t, o.allocations(t), 1, "the mesh address is not a Machine prefix: the joined host holds one of those, the other host none")

	require.NoError(t, o.op.Remove(t.Context(), "node-1"))
	assert.NotContains(t, o.meshAllocations(t), "10.99.0.2/32", "a removed host's mesh address is freed")
}

func TestASignerFailureDropsTheMeshReservationAndAFullMeshPoolIssuesNothing(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	// A /31 holds two addresses and gives up none.
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.99.0.0/31")))
	a, b, c := newHost(t, "h-a"), newHost(t, "h-b"), newHost(t, "h-c")
	for _, h := range []host{a, b, c} {
		_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
		require.NoError(t, err)
	}

	o.c.Signer.Fail(errors.New("the signer is down"))
	_, err := o.tryAdmit(t, a)
	assert.Equal(t, http.StatusBadGateway, status(err))
	assert.Empty(t, o.meshAllocations(t), "the failed admission holds no mesh address")
	o.c.Signer.Fail(nil)

	for _, h := range []host{a, b} {
		_, err := o.tryAdmit(t, h)
		require.NoError(t, err, "the control: the pool has room for %s", h.id)
	}
	calls := o.c.Signer.Calls()
	_, err = o.tryAdmit(t, c)
	assert.Equal(t, http.StatusConflict, status(err), "the pool is full")
	assert.Contains(t, err.Error(), "mesh pool")
	assert.Equal(t, calls, o.c.Signer.Calls(), "no certificate is issued for a host that holds no mesh address")
}

func TestTheRecorderGivesAHostAdmittedBeforeTheMeshPoolItsMeshAddress(t *testing.T) {
	reported := &reportedPrefixes{by: map[string][]string{}}
	o := startOne(t, orchtest.Options{NewPrefixHosts: func(inventory.Service) inventory.PrefixHosts { return reported }})
	h := newHost(t, "node-1")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	before := o.admit(t, h)
	require.Empty(t, before.MeshAddress, "no profile, so no mesh pool, at its admission")
	time.Sleep(100 * time.Millisecond)
	require.Empty(t, o.meshAllocations(t), "the control: with no pool the recorder gives nothing")

	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.99.0.0/24")))
	require.Eventually(t, func() bool {
		hosts, err := o.op.Hosts(t.Context())
		require.NoError(t, err)
		return len(hosts) == 1 && hosts[0].MeshAddress == "10.99.0.1"
	}, 10*time.Second, 50*time.Millisecond, "the leader's recorder gives the host one")
	assert.Equal(t, "node-1", o.meshAllocations(t)["10.99.0.1/32"].HostID, "and records it")
}

func TestAHostAFullMeshPoolHasNoAddressForIsAnEventAndHoldsUpNoPrefix(t *testing.T) {
	reported := &reportedPrefixes{by: map[string][]string{}}
	o := startOne(t, orchtest.Options{NewPrefixHosts: func(inventory.Service) inventory.PrefixHosts { return reported }})
	for _, id := range []string{"node-1", "node-2"} {
		h := newHost(t, id)
		_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
		require.NoError(t, err)
		o.admit(t, h)
	}
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.99.0.7/32")), "a pool of one address, for two hosts")
	require.Eventually(t, func() bool { return len(o.meshAllocations(t)) == 1 }, 10*time.Second, 50*time.Millisecond, "one host takes the address")
	given := o.meshAllocations(t)["10.99.0.7/32"].HostID
	other := map[string]string{"node-1": "node-2", "node-2": "node-1"}[given]

	require.Eventually(t, func() bool {
		events, err := o.op.Events(t.Context())
		require.NoError(t, err)
		for _, e := range events {
			if e.Type == inventory.EventMeshAddressUnavailable && e.Subject == other {
				return true
			}
		}
		return false
	}, 10*time.Second, 50*time.Millisecond, "the host the pool has no address for is an event")

	reported.set(other, "10.200.7.0/24")
	require.Eventually(t, func() bool { return o.allocations(t)["10.200.7.0/24"].HostID == other }, 10*time.Second, 50*time.Millisecond,
		"the prefix it reports is still recorded")
	time.Sleep(300 * time.Millisecond)
	events, err := o.op.Events(t.Context())
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Type == inventory.EventMeshAddressUnavailable {
			n++
		}
	}
	assert.Equal(t, 1, n, "told once, not every pass")
}

func TestTheMeshPoolIsKeptApartFromEveryNetworkAndAddressPool(t *testing.T) {
	eh := &exposureHosts{leaseds: map[string]exposures.Lease{}}
	o := startOne(t, orchtest.Options{
		NewHosts:         func(inventory.Service) groups.Hosts { return orchtest.NewFakeHosts("h1") },
		NewExposureHosts: func(inventory.Service) orchd.ExposureHosts { return eh },
	})
	ctx := t.Context()
	_, err := o.op.ApplyNetwork(ctx, "a", orchapi.NetworkSpec{Prefix: "10.1.0.0/24"})
	require.NoError(t, err)
	_, err = o.op.ApplyAddressPool(ctx, "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)

	for name, pool := range map[string]string{"a network": "10.1.0.128/25", "an address pool": "192.168.0.0/24"} {
		err := o.op.SetProfile(ctx, meshProfileYAML(t, "10.200.0.0/16", pool))
		assert.Equal(t, http.StatusConflict, status(err), "a mesh pool overlapping %s: %v", name, err)
	}
	require.NoError(t, o.op.SetProfile(ctx, meshProfileYAML(t, "10.200.0.0/16", "10.99.0.0/24")), "the control: a pool of its own")

	_, err = o.op.ApplyNetwork(ctx, "b", orchapi.NetworkSpec{Prefix: "10.99.0.0/25"})
	assert.Equal(t, http.StatusConflict, status(err), "a network overlapping the mesh pool")
	_, err = o.op.ApplyAddressPool(ctx, "mesh", orchapi.AddressPoolSpec{CIDRs: []string{"10.99.0.8/29"}, Networks: []string{"a"}})
	assert.Equal(t, http.StatusBadRequest, status(err), "an address pool overlapping the mesh pool")
	_, err = o.op.ApplyNetwork(ctx, "b", orchapi.NetworkSpec{Prefix: "10.2.0.0/24"})
	require.NoError(t, err, "the control: a network outside it")
}

func TestAMeshPoolChangeThatLeavesAHeldAddressOutIsRefused(t *testing.T) {
	o := startOne(t, orchtest.Options{})
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.99.0.0/24")))
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.98.0.0/24")), "the control: no host holds one yet, so it may move")
	h := newHost(t, "node-1")
	_, err := o.anon.Enroll(t.Context(), h.request(o.secret))
	require.NoError(t, err)
	require.Equal(t, "10.98.0.1", o.admit(t, h).MeshAddress)

	for _, c := range []struct{ name, mesh string }{
		{"narrowed past it", "10.98.0.128/25"},
		{"moved", "10.97.0.0/24"},
		{"dropped", ""},
	} {
		err := o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", c.mesh))
		assert.Equal(t, http.StatusConflict, status(err), "%s: %v", c.name, err)
	}
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.98.0.0/25")), "the control: narrowed, still holding it")
	require.NoError(t, o.op.SetProfile(t.Context(), meshProfileYAML(t, "10.200.0.0/16", "10.98.0.0/23")), "and widened")
}
