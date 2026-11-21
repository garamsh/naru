package exposures_test

import (
	"net/netip"
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

func TestTheLowestFreeAddressIsAssigned(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e1, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e1.Address, "the pool starts at the first CIDR's first address, network address included")
	require.Equal(t, orchapi.ExposurePending, e1.Phase)
	e2, err := s.ApplyExposure(ctx, "ccm", "a", "db", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.81", e2.Address)
}

func TestADeletedExposuresAddressIsHeldUntilTheReconcilerReleasesIt(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	e1, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "web"))
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "web"), "a delete is idempotent")
	rec, err := st.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, exposures.PhaseReleasing, rec.Phase, "the record stays: its host may still announce the address")

	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = e1.Address
	_, err = s.ApplyExposure(ctx, "ccm", "a", "again", sp)
	require.ErrorIs(t, err, inventory.ErrConflict, "the same address, asked for at once")
	e3, err := s.ApplyExposure(ctx, "ccm", "a", "other", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.NotEqual(t, e1.Address, e3.Address, "and the lowest free address is another one")
	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.ErrorIs(t, err, inventory.ErrConflict, "the same id while it is released")
	_, err = s.Exposure(ctx, "a", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound, "an exposer no longer sees it")
	list, err := s.Exposures(ctx, "a")
	require.NoError(t, err)
	require.Len(t, list, 1, "only the other one")
	all, err := s.AllExposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "an operator still sees the one being released")

	// Control: once the reconciler has deleted the record, the address is free again.
	require.NoError(t, st.DeleteExposure(ctx, rec))
	again, err := s.ApplyExposure(ctx, "ccm", "a", "again", sp)
	require.NoError(t, err)
	require.Equal(t, e1.Address, again.Address)
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
	both.Network = "b"
	both.Backends = []string{"10.2.0.5"}
	_, err = s.ApplyExposure(ctx, "ccm", "b", "web", both)
	require.NoError(t, err, "control: the same pool, the caller's own network")

	_, err = s.ApplyExposure(ctx, "ccm", "b", "other", spec("b", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not list the network")
	sp := spec("a", "10.1.0.5")
	sp.Pool = "absent"
	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", sp)
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not exist looks the same")
	_, err = s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err, "control: the pool lan lists a")
}

func TestAnIdBelongsToItsNetwork(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyPool(ctx, "op", "both", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.5.0/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)
	sa := spec("a", "10.1.0.5")
	sa.Pool = "both"
	ea, err := s.ApplyExposure(ctx, "ccm", "a", "web", sa)
	require.NoError(t, err)

	// B names A's id: it makes its own exposure, and A's is neither changed nor taken over.
	sb := spec("b", "10.2.0.5")
	sb.Pool = "both"
	eb, err := s.ApplyExposure(ctx, "ccm", "b", "web", sb)
	require.NoError(t, err)
	require.NotEqual(t, ea.Address, eb.Address)
	got, err := st.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, ea, got.Exposure, "A's exposure is as A left it")
	require.Equal(t, []string{"10.1.0.5"}, got.Backends)
	// Control: A updates its own exposure of that id.
	sa.Backends = []string{"10.1.0.5", "10.1.0.6"}
	up, err := s.ApplyExposure(ctx, "ccm", "a", "web", sa)
	require.NoError(t, err)
	require.Equal(t, ea.Address, up.Address)
	require.Len(t, up.Backends, 2)

	// A read or delete of another network's id is as if it were absent, and the same answer as
	// for an id nobody holds, so the status does not tell a network which ids exist elsewhere.
	_, err = s.ApplyExposure(ctx, "ccm", "a", "private", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.Exposure(ctx, "b", "private")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	_, err = s.Exposure(ctx, "b", "nobody-holds-this")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	_, err = s.Exposure(ctx, "a", "private")
	require.NoError(t, err, "control: its own network reads it")
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "b", "private"))
	cur, err := st.GetExposure(ctx, "a", "private")
	require.NoError(t, err)
	require.NotEqual(t, exposures.PhaseReleasing, cur.Phase, "B's delete left A's exposure alone")
	require.NoError(t, s.DeleteExposure(ctx, "ccm", "a", "private"))
	cur, err = st.GetExposure(ctx, "a", "private")
	require.NoError(t, err)
	require.Equal(t, exposures.PhaseReleasing, cur.Phase, "control: A's own delete takes effect")

	got2, err := s.Exposures(ctx, "b")
	require.NoError(t, err)
	require.Len(t, got2, 1, "a list shows the caller's own only")
	require.Equal(t, "b", got2[0].Network)
}

func TestACallNamingNoNetworkIsForbiddenAndTouchesNothing(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyExposure(ctx, "ccm", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.Exposure(ctx, "", "web")
	require.ErrorIs(t, err, exposures.ErrForbidden)
	_, err = s.Exposures(ctx, "")
	require.ErrorIs(t, err, exposures.ErrForbidden)
	require.ErrorIs(t, s.DeleteExposure(ctx, "ccm", "", "web"), exposures.ErrForbidden)
	_, err = s.ApplyExposure(ctx, "ccm", "", "web", spec("", "10.1.0.5"))
	require.ErrorIs(t, err, exposures.ErrForbidden)

	_, err = s.Exposure(ctx, "a", "web")
	require.NoError(t, err, "control: a named network reads it, and the delete above left it")
	all, err := s.AllExposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1, "the operators' reader is the one way to see every network")
}

func TestAnIdIsValidatedOnEveryCall(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.Exposure(ctx, "a", "Not Valid")
	require.ErrorIs(t, err, inventory.ErrInvalid)
	require.ErrorIs(t, s.DeleteExposure(ctx, "ccm", "a", "../b/web"), inventory.ErrInvalid)
	_, err = s.Exposure(ctx, "a", "valid-id")
	require.ErrorIs(t, err, inventory.ErrNotFound, "control: a valid absent id")
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

func TestAnExposureHoldsAtMostSixteenPortsAndSixtyFourBackendsWithoutDuplicates(t *testing.T) {
	st := exposurestest.NewStore()
	var members []string
	for i := range exposures.MaxBackends + 1 {
		members = append(members, netip.AddrFrom4([4]byte{10, 1, 0, byte(i + 1)}).String())
	}
	s := exposures.New(st, exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}}, exposurestest.Members{"a": members}, &exposurestest.Log{}, &exposurestest.Clock{})
	_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	ports := func(n int) []orchapi.ExposurePort {
		var out []orchapi.ExposurePort
		for i := range n {
			out = append(out, orchapi.ExposurePort{Protocol: "TCP", Port: 1000 + i, NodePort: 31000 + i})
		}
		return out
	}
	sp := spec("a", members[:exposures.MaxBackends]...)
	sp.Ports = ports(exposures.MaxPortsPerExposure)
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "web", sp)
	require.NoError(t, err, "control: the limits themselves")

	over := sp
	over.Ports = ports(exposures.MaxPortsPerExposure + 1)
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "ports", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "17 ports")
	over = sp
	over.Backends = members
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "backends", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "65 backends")
	over = sp
	over.Ports = []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, NodePort: 30080}, {Protocol: "TCP", Port: 80, NodePort: 30081}}
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "dup", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "port 80/TCP twice")
	over.Ports = []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, NodePort: 30080}, {Protocol: "UDP", Port: 80, NodePort: 30081}}
	over.ProxyProtocol = ""
	_, err = s.ApplyExposure(t.Context(), "ccm", "a", "both-protocols", over)
	require.NoError(t, err, "control: the same number over TCP and UDP is two ports")
}

func TestAPoolRefusesAnInvalidNetworkName(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.ApplyPool(t.Context(), "op", "p", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.0/28"}, Networks: []string{"A_b"}})
	require.ErrorIs(t, err, inventory.ErrInvalid)
	_, err = s.ApplyPool(t.Context(), "op", "p", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.0/28"}, Networks: []string{"a"}})
	require.NoError(t, err, "control")
}
