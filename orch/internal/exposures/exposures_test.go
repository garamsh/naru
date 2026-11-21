package exposures_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/exposures/exposurestest"
	"github.com/garamsh/naru/orch/internal/inventory"
)

func fixture(t *testing.T) (*exposures.Service, *exposurestest.Store) {
	t.Helper()
	st := exposurestest.NewStore()
	nets := exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}, {Name: "b", Prefix: "10.2.0.0/24"}}
	members := exposurestest.Members{"a": {"10.1.0.5", "10.1.0.6"}, "b": {"10.2.0.5"}}
	s := exposures.New(st, nets, members, &exposurestest.Log{}, &exposurestest.Clock{T: time.Unix(1000, 0)})
	_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29", "192.168.0.88/31"}, Networks: []string{"a"}})
	require.NoError(t, err)
	return s, st
}

func spec(network string, backends ...string) orchapi.ExposureSpec {
	return orchapi.ExposureSpec{
		Network: network, Pool: "lan", Backends: backends,
		Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, NodePort: 30443}},
	}
}

func TestTheLowestFreeAddressIsAssignedAndReleasedOnDelete(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e1, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e1.Address, "the pool starts at the first CIDR's first address, network address included")
	require.Equal(t, orchapi.ExposurePending, e1.Phase)
	e2, err := s.ApplyExposure(ctx, "ccm", "a", "db", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.81", e2.Address)

	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "web"))
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "web"), "a delete is idempotent")
	e3, err := s.ApplyExposure(ctx, "ccm", "a", "again", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e3.Address, "a deleted Service's address is free again")
}

func TestARequestedAddressIsGrantedOnlyInThePoolAndFree(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = "192.168.0.89"
	e, err := s.ApplyExposure(ctx, "ccm", "a", "x", sp)
	require.NoError(t, err)
	require.Equal(t, "192.168.0.89", e.Address)
	_, err = s.ApplyExposure(ctx, "ccm", "a", "y", sp)
	require.ErrorIs(t, err, inventory.ErrConflict, "held")
	sp.RequestedAddress = "192.168.0.90"
	_, err = s.ApplyExposure(ctx, "ccm", "a", "z", sp)
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "outside the pool")
}

func TestAFullPoolIsAConflict(t *testing.T) {
	s, _ := fixture(t)
	for i := range 10 {
		_, err := s.ApplyExposure(t.Context(), "ccm", "a", string(rune('a'+i))+"x", spec("a", "10.1.0.5"))
		require.NoError(t, err)
	}
	_, err := s.ApplyExposure(t.Context(), "ccm", "a", "one-too-many", spec("a", "10.1.0.5"))
	require.ErrorIs(t, err, inventory.ErrConflict)
}

func TestACallerOutsideItsNetworkOrPoolIsForbidden(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyPool(ctx, "op", "both", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.5.0/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)
	both := spec("a", "10.1.0.5")
	both.Pool = "both"
	_, err = s.ApplyExposure(ctx, "ccm", "b", "web", both)
	require.ErrorIs(t, err, exposures.ErrForbidden, "a spec that names another network, in a pool open to both")

	_, err = s.ApplyExposure(ctx, "ccm", "b", "web", spec("b", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not list the network")
	sp := spec("a", "10.1.0.5")
	sp.Pool = "absent"
	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", sp)
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not exist looks the same")

	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.Exposure(ctx, "b", "web")
	require.ErrorIs(t, err, exposures.ErrForbidden, "another network's exposure: read")
	require.ErrorIs(t, s.DeleteExposure(ctx, "ccm", "b", "web"), exposures.ErrForbidden, "delete")
	got, err := s.Exposures(ctx, "b")
	require.NoError(t, err)
	require.Empty(t, got, "a list shows the caller's own only")
	all, err := s.Exposures(ctx, "")
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestABackendMustBeALiveMemberOfTheCallersNetwork(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "a member of another network")
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "web", spec("a", "10.1.0.99"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "no member")
}

func TestProxyProtocolIsTCPOnly(t *testing.T) {
	s, _ := fixture(t)
	sp := spec("a", "10.1.0.5")
	sp.ProxyProtocol = "v2"
	e, err := s.ApplyExposure(t.Context(), "ccm", "a", "web", sp)
	require.NoError(t, err)
	require.Equal(t, "v2", e.ProxyProtocol)
	sp.Ports = append(sp.Ports, orchapi.ExposurePort{Protocol: "UDP", Port: 53, NodePort: 30053})
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "dns", sp)
	require.ErrorIs(t, err, exposures.ErrUnprocessable)
	sp.ProxyProtocol = "v1"
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "old", sp)
	require.ErrorIs(t, err, inventory.ErrInvalid)
}

func TestAnUpdateKeepsTheAddressAndRefusesAChangeOfPool(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	up, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5", "10.1.0.6"))
	require.NoError(t, err)
	require.Equal(t, e.Address, up.Address)
	require.Len(t, up.Backends, 2)
	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = "192.168.0.85"
	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", sp)
	require.ErrorIs(t, err, inventory.ErrConflict)
}

func TestANetworkHoldsAtMostSixtyFourExposures(t *testing.T) {
	st := exposurestest.NewStore()
	s := exposures.New(st, exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}}, exposurestest.Members{"a": {"10.1.0.5"}}, &exposurestest.Log{}, &exposurestest.Clock{})
	_, err := s.ApplyPool(t.Context(), "op", "big", orchapi.AddressPoolSpec{CIDRs: []string{"172.16.0.0/24"}, Networks: []string{"a"}})
	require.NoError(t, err)
	sp := spec("a", "10.1.0.5")
	sp.Pool = "big"
	for i := range exposures.MaxExposuresPerNetwork {
		_, err := s.ApplyExposure(t.Context(), "ccm", "a", "s"+string(rune('a'+i/26))+string(rune('a'+i%26)), sp)
		require.NoError(t, err)
	}
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "over", sp)
	require.ErrorIs(t, err, inventory.ErrConflict)
}

func TestPoolsAreValidatedAndHeldWhileUsed(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	bad := func(name string, c ...string) error {
		_, err := s.ApplyPool(ctx, "op", name, orchapi.AddressPoolSpec{CIDRs: c, Networks: []string{"a"}})
		return err
	}
	require.ErrorIs(t, bad("p", "192.168.0.84/30"), inventory.ErrInvalid, "overlaps pool lan")
	require.ErrorIs(t, bad("p", "10.1.0.0/28"), inventory.ErrInvalid, "inside a network's prefix")
	require.ErrorIs(t, bad("p", "192.168.1.5/24"), inventory.ErrInvalid, "host bits set")
	require.ErrorIs(t, bad("p"), inventory.ErrInvalid, "no CIDR")
	require.NoError(t, bad("other", "192.168.1.0/28"))

	_, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.ApplyPool(ctx, "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.88/31"}, Networks: []string{"a"}})
	require.ErrorIs(t, err, inventory.ErrConflict, "the address 192.168.0.80 would leave the pool")
	_, err = s.ApplyPool(ctx, "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"b"}})
	require.ErrorIs(t, err, inventory.ErrConflict, "the network would leave the pool")
	require.ErrorIs(t, s.DeletePool(ctx, "op", "lan"), inventory.ErrConflict)
	require.NoError(t, s.DeletePool(ctx, "op", "other"))
}
