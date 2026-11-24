package exposures_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/lib/wire"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/exposures/exposurestest"
	"github.com/garamsh/naru/orch/internal/inventory"
)

func fixture(t *testing.T) (*exposures.Service, *exposurestest.Store) {
	t.Helper()
	st := exposurestest.NewStore()
	nets := exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}, {Name: "b", Prefix: "10.2.0.0/24"}}
	members := exposurestest.Members{"a": {"10.1.0.5", "10.1.0.6"}, "b": {"10.2.0.5"}}
	s := exposures.New(st, nets, members, exposurestest.Profile{}, &exposurestest.Log{}, &exposurestest.Clock{T: time.Unix(1000, 0)})
	_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29", "192.168.0.88/31"}, Networks: []string{"a"}})
	require.NoError(t, err)
	return s, st
}

func spec(network string, backends ...string) orchapi.ExposureSpec {
	return orchapi.ExposureSpec{
		Network: network, Pool: "lan", Backends: backends,
		Ports: []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, BackendPort: 30443}},
	}
}

func TestTheLowestFreeAddressIsAssigned(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e1, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", e1.Address, "the pool starts at the first CIDR's first address, network address included")
	require.Equal(t, orchapi.ExposurePending, e1.Phase)
	e2, err := s.ApplyExposure(ctx, "op", "a", "db", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.81", e2.Address)
}

func TestADeletedExposuresAddressIsHeldUntilTheReconcilerReleasesIt(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	e1, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.NoError(t, s.DeleteExposure(ctx, "op", "a", "web"))
	require.NoError(t, s.DeleteExposure(ctx, "op", "a", "web"), "a delete is idempotent")
	rec, err := st.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, exposures.PhaseReleasing, rec.Phase, "the record stays: its host may still announce the address")

	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = e1.Address
	_, err = s.ApplyExposure(ctx, "op", "a", "again", sp)
	require.ErrorIs(t, err, inventory.ErrConflict, "the same address, asked for at once")
	e3, err := s.ApplyExposure(ctx, "op", "a", "other", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.NotEqual(t, e1.Address, e3.Address, "and the lowest free address is another one")
	_, err = s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.ErrorIs(t, err, inventory.ErrConflict, "the same id while it is released")
	_, err = s.Exposure(ctx, "a", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound, "one being released is not shown by id")
	all, err := s.AllExposures(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2, "the list of every exposure still shows the one being released")

	// Control: once the reconciler has deleted the record, the address is free again.
	require.NoError(t, st.DeleteExposure(ctx, rec))
	again, err := s.ApplyExposure(ctx, "op", "a", "again", sp)
	require.NoError(t, err)
	require.Equal(t, e1.Address, again.Address)
}

func TestARequestedAddressIsGrantedOnlyInThePoolAndFree(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = "192.168.0.89"
	e, err := s.ApplyExposure(ctx, "op", "a", "x", sp)
	require.NoError(t, err)
	require.Equal(t, "192.168.0.89", e.Address)
	_, err = s.ApplyExposure(ctx, "op", "a", "y", sp)
	require.ErrorIs(t, err, inventory.ErrConflict, "held")
	sp.RequestedAddress = "192.168.0.90"
	_, err = s.ApplyExposure(ctx, "op", "a", "z", sp)
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "outside the pool")
}

func TestAFullPoolIsAConflict(t *testing.T) {
	s, _ := fixture(t)
	for i := range 10 {
		_, err := s.ApplyExposure(t.Context(), "op", "a", string(rune('a'+i))+"x", spec("a", "10.1.0.5"))
		require.NoError(t, err)
	}
	_, err := s.ApplyExposure(t.Context(), "op", "a", "one-too-many", spec("a", "10.1.0.5"))
	require.ErrorIs(t, err, inventory.ErrConflict)
}

func TestACallerOutsideItsNetworkOrPoolIsForbidden(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyPool(ctx, "op", "both", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.5.0/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)
	both := spec("a", "10.1.0.5")
	both.Pool = "both"
	_, err = s.ApplyExposure(ctx, "op", "b", "web", both)
	require.ErrorIs(t, err, exposures.ErrForbidden, "a spec that names another network, in a pool open to both")
	both.Network = "b"
	both.Backends = []string{"10.2.0.5"}
	_, err = s.ApplyExposure(ctx, "op", "b", "web", both)
	require.NoError(t, err, "control: the same pool, the caller's own network")

	_, err = s.ApplyExposure(ctx, "op", "b", "other", spec("b", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not list the network")
	sp := spec("a", "10.1.0.5")
	sp.Pool = "absent"
	_, err = s.ApplyExposure(ctx, "op", "a", "web", sp)
	require.ErrorIs(t, err, exposures.ErrForbidden, "a pool that does not exist looks the same")
	_, err = s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err, "control: the pool lan lists a")
}

func TestAnIdBelongsToItsNetwork(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyPool(ctx, "op", "both", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.5.0/29"}, Networks: []string{"a", "b"}})
	require.NoError(t, err)
	sa := spec("a", "10.1.0.5")
	sa.Pool = "both"
	ea, err := s.ApplyExposure(ctx, "op", "a", "web", sa)
	require.NoError(t, err)

	// B names A's id: it makes its own exposure, and A's is neither changed nor taken over.
	sb := spec("b", "10.2.0.5")
	sb.Pool = "both"
	eb, err := s.ApplyExposure(ctx, "op", "b", "web", sb)
	require.NoError(t, err)
	require.NotEqual(t, ea.Address, eb.Address)
	got, err := st.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, ea, got.Exposure, "A's exposure is as A left it")
	require.Equal(t, []string{"10.1.0.5"}, got.Backends)
	// Control: A updates its own exposure of that id.
	sa.Backends = []string{"10.1.0.5", "10.1.0.6"}
	up, err := s.ApplyExposure(ctx, "op", "a", "web", sa)
	require.NoError(t, err)
	require.Equal(t, ea.Address, up.Address)
	require.Len(t, up.Backends, 2)

	// A read or delete of another network's id is as if it were absent, and the same answer as
	// for an id nobody holds, so the status does not tell a network which ids exist elsewhere.
	_, err = s.ApplyExposure(ctx, "op", "a", "private", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.Exposure(ctx, "b", "private")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	_, err = s.Exposure(ctx, "b", "nobody-holds-this")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	_, err = s.Exposure(ctx, "a", "private")
	require.NoError(t, err, "control: its own network reads it")
	require.NoError(t, s.DeleteExposure(ctx, "op", "b", "private"))
	cur, err := st.GetExposure(ctx, "a", "private")
	require.NoError(t, err)
	require.NotEqual(t, exposures.PhaseReleasing, cur.Phase, "B's delete left A's exposure alone")
	require.NoError(t, s.DeleteExposure(ctx, "op", "a", "private"))
	cur, err = st.GetExposure(ctx, "a", "private")
	require.NoError(t, err)
	require.Equal(t, exposures.PhaseReleasing, cur.Phase, "control: A's own delete takes effect")
}

func TestACallNamingNoNetworkIsForbiddenAndTouchesNothing(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	_, err = s.Exposure(ctx, "", "web")
	require.ErrorIs(t, err, exposures.ErrForbidden)
	require.ErrorIs(t, s.DeleteExposure(ctx, "op", "", "web"), exposures.ErrForbidden)
	_, err = s.ApplyExposure(ctx, "op", "", "web", spec("", "10.1.0.5"))
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
	require.ErrorIs(t, s.DeleteExposure(ctx, "op", "a", "../b/web"), inventory.ErrInvalid)
	_, err = s.Exposure(ctx, "a", "valid-id")
	require.ErrorIs(t, err, inventory.ErrNotFound, "control: a valid absent id")
}

func TestABackendMustBeALiveMemberOfTheCallersNetwork(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.ApplyExposure(t.Context(), "op", "a", "web", spec("a", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "a member of another network")
	_, err = s.ApplyExposure(t.Context(), "op", "a", "web", spec("a", "10.1.0.99"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "no member")
}

// Orch takes every protocol in the spelling the host API takes it in, and no other (ADR 0107).
func TestAnExposureTakesEveryProtocolInTheSharedSpellingAndNoOther(t *testing.T) {
	s, _ := fixture(t)
	with := func(p wire.Protocol) orchapi.ExposureSpec {
		sp := spec("a", "10.1.0.5")
		sp.Ports[0].Protocol = p
		return sp
	}
	for _, p := range wire.Protocols() {
		e, err := s.ApplyExposure(t.Context(), "op", "a", "ok-"+strings.ToLower(string(p)), with(p))
		require.NoError(t, err, "control: %s is taken", p)
		require.Equal(t, p, e.Ports[0].Protocol)
	}
	for _, p := range []wire.Protocol{"tcp", "udp", "Tcp", "SCTP", ""} {
		_, err := s.ApplyExposure(t.Context(), "op", "a", "bad", with(p))
		require.ErrorIs(t, err, inventory.ErrInvalid, "protocol %q", p)
	}
}

func TestProxyProtocolIsTCPOnly(t *testing.T) {
	s, _ := fixture(t)
	sp := spec("a", "10.1.0.5")
	sp.ProxyProtocol = "v2"
	e, err := s.ApplyExposure(t.Context(), "op", "a", "web", sp)
	require.NoError(t, err)
	require.Equal(t, "v2", e.ProxyProtocol)
	sp.Ports = append(sp.Ports, orchapi.ExposurePort{Protocol: "UDP", Port: 53, BackendPort: 30053})
	_, err = s.ApplyExposure(t.Context(), "op", "a", "dns", sp)
	require.ErrorIs(t, err, exposures.ErrUnprocessable)
	sp.ProxyProtocol = "v1"
	_, err = s.ApplyExposure(t.Context(), "op", "a", "old", sp)
	require.ErrorIs(t, err, inventory.ErrInvalid)
}

func TestAnUpdateKeepsTheAddressAndRefusesAChangeOfPool(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	up, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5", "10.1.0.6"))
	require.NoError(t, err)
	require.Equal(t, e.Address, up.Address)
	require.Len(t, up.Backends, 2)
	sp := spec("a", "10.1.0.5")
	sp.RequestedAddress = "192.168.0.85"
	_, err = s.ApplyExposure(ctx, "op", "a", "web", sp)
	require.ErrorIs(t, err, inventory.ErrConflict)
}

func TestANetworkHoldsAtMostSixtyFourExposures(t *testing.T) {
	st := exposurestest.NewStore()
	s := exposures.New(st, exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}}, exposurestest.Members{"a": {"10.1.0.5"}}, exposurestest.Profile{}, &exposurestest.Log{}, &exposurestest.Clock{})
	_, err := s.ApplyPool(t.Context(), "op", "big", orchapi.AddressPoolSpec{CIDRs: []string{"172.16.0.0/24"}, Networks: []string{"a"}})
	require.NoError(t, err)
	sp := spec("a", "10.1.0.5")
	sp.Pool = "big"
	for i := range exposures.MaxExposuresPerNetwork {
		_, err := s.ApplyExposure(t.Context(), "op", "a", "s"+string(rune('a'+i/26))+string(rune('a'+i%26)), sp)
		require.NoError(t, err)
	}
	_, err = s.ApplyExposure(t.Context(), "op", "a", "over", sp)
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

	_, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
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
	s := exposures.New(st, exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}}, exposurestest.Members{"a": members}, exposurestest.Profile{}, &exposurestest.Log{}, &exposurestest.Clock{})
	_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
	require.NoError(t, err)
	ports := func(n int) []orchapi.ExposurePort {
		var out []orchapi.ExposurePort
		for i := range n {
			out = append(out, orchapi.ExposurePort{Protocol: "TCP", Port: 1000 + i, BackendPort: 31000 + i})
		}
		return out
	}
	sp := spec("a", members[:exposures.MaxBackends]...)
	sp.Ports = ports(exposures.MaxPortsPerExposure)
	_, err = s.ApplyExposure(t.Context(), "op", "a", "web", sp)
	require.NoError(t, err, "control: the limits themselves")

	over := sp
	over.Ports = ports(exposures.MaxPortsPerExposure + 1)
	_, err = s.ApplyExposure(t.Context(), "op", "a", "ports", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "17 ports")
	over = sp
	over.Backends = members
	_, err = s.ApplyExposure(t.Context(), "op", "a", "backends", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "65 backends")
	over = sp
	over.Ports = []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, BackendPort: 30080}, {Protocol: "TCP", Port: 80, BackendPort: 30081}}
	_, err = s.ApplyExposure(t.Context(), "op", "a", "dup", over)
	require.ErrorIs(t, err, inventory.ErrInvalid, "port 80/TCP twice")
	over.Ports = []orchapi.ExposurePort{{Protocol: "TCP", Port: 80, BackendPort: 30080}, {Protocol: "UDP", Port: 80, BackendPort: 30081}}
	over.ProxyProtocol = ""
	_, err = s.ApplyExposure(t.Context(), "op", "a", "both-protocols", over)
	require.NoError(t, err, "control: the same number over TCP and UDP is two ports")
}

func TestAPoolRefusesAnInvalidNetworkName(t *testing.T) {
	s, _ := fixture(t)
	_, err := s.ApplyPool(t.Context(), "op", "p", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.0/28"}, Networks: []string{"A_b"}})
	require.ErrorIs(t, err, inventory.ErrInvalid)
	_, err = s.ApplyPool(t.Context(), "op", "p", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.1.0/28"}, Networks: []string{"a"}})
	require.NoError(t, err, "control")
}

// renewing renews the record, as the reconciler does, just before the first n updates reach the store.
type renewing struct {
	*exposurestest.Store
	n int
}

func (r *renewing) UpdateExposure(ctx context.Context, next, prev exposures.Record) error {
	if r.n > 0 {
		r.n--
		cur, _ := r.Store.GetExposure(ctx, prev.Network, prev.ID)
		renewed := cur
		renewed.Host, renewed.LeaseUntil = "h1", cur.LeaseUntil.Add(time.Second)
		if err := r.Store.UpdateExposure(ctx, renewed, cur); err != nil {
			panic(err)
		}
	}
	return r.Store.UpdateExposure(ctx, next, prev)
}

func TestAnApplyOverARenewalIsReadAgainAndKeepsTheRenewal(t *testing.T) {
	for _, tc := range []struct {
		renewals int
		ok       bool
	}{{0, true}, {1, true}, {2, true}, {3, false}} {
		st := &renewing{Store: exposurestest.NewStore(), n: 0}
		nets := exposurestest.Networks{{Name: "a", Prefix: "10.1.0.0/24"}}
		members := exposurestest.Members{"a": {"10.1.0.5", "10.1.0.6"}}
		s := exposures.New(st, nets, members, exposurestest.Profile{}, &exposurestest.Log{}, &exposurestest.Clock{T: time.Unix(1000, 0)})
		_, err := s.ApplyPool(t.Context(), "op", "lan", orchapi.AddressPoolSpec{CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}})
		require.NoError(t, err)
		_, err = s.ApplyExposure(t.Context(), "op", "a", "web", spec("a", "10.1.0.5"))
		require.NoError(t, err)
		st.n = tc.renewals
		got, err := s.ApplyExposure(t.Context(), "op", "a", "web", spec("a", "10.1.0.5", "10.1.0.6"))
		if !tc.ok {
			require.ErrorIs(t, err, inventory.ErrConflict, "%d renewals in a row exceed the tries", tc.renewals)
			continue
		}
		require.NoError(t, err, "%d renewals", tc.renewals)
		require.Len(t, got.Backends, 2)
		rec, err := st.GetExposure(t.Context(), "a", "web")
		require.NoError(t, err)
		require.Len(t, rec.Backends, 2, "the change was written")
		require.Equal(t, tc.renewals > 0, rec.Host == "h1", "over the renewal, which it kept")
	}
}

func TestANetworkNameThatIsNoDNSLabelNamesNoNetwork(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyExposure(ctx, "op", "a", "web", spec("a", "10.1.0.5"))
	require.NoError(t, err)
	for _, bad := range []string{"a/b", "A", "-a", "a b"} {
		require.ErrorIs(t, s.DeleteExposure(ctx, "op", bad, "web"), inventory.ErrInvalid, bad)
		_, err := s.Exposure(ctx, bad, "web")
		require.ErrorIs(t, err, inventory.ErrInvalid, bad)
	}
	require.NoError(t, s.DeleteExposure(ctx, "op", "a", "web"), "control: the network's own name")
}

func apiSpec(network string, backends ...string) orchapi.ExposureSpec {
	sp := spec(network, backends...)
	sp.Ports = []orchapi.ExposurePort{{Protocol: "TCP", Port: 443, BackendPort: 6443}}
	sp.RequestedAddress = "192.168.0.84"
	return sp
}

func TestTheOperatorMayNameAnyBackendPortOfAMemberOfTheNetwork(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	e, err := s.ApplyExposure(ctx, "op", "a", "api", apiSpec("a", "10.1.0.5"))
	require.NoError(t, err)
	require.Equal(t, "192.168.0.84", e.Address, "the address the operator declared, before any certificate names it")
	require.Equal(t, orchapi.OwnerOperator, e.Owner)
	require.Equal(t, 6443, e.Ports[0].BackendPort)

	sp := apiSpec("a", "10.1.0.5")
	sp.Ports[0].BackendPort = 65536
	_, err = s.ApplyExposure(ctx, "op", "a", "api2", sp)
	require.ErrorIs(t, err, inventory.ErrInvalid, "control: a backend port ends at 65535")
	sp.Ports[0].BackendPort = 0
	_, err = s.ApplyExposure(ctx, "op", "a", "api2", sp)
	require.ErrorIs(t, err, inventory.ErrInvalid, "and starts at 1")
}

func TestABackendOfTheOperatorsExposureIsALiveMemberOfThatNetwork(t *testing.T) {
	s, _ := fixture(t)
	ctx := t.Context()
	_, err := s.ApplyExposure(ctx, "op", "a", "api", apiSpec("a", "10.2.0.5"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "a member of another network")
	_, err = s.ApplyExposure(ctx, "op", "a", "api", apiSpec("a", "10.1.0.99"))
	require.ErrorIs(t, err, exposures.ErrUnprocessable, "an address no member has")
	_, err = s.ApplyExposure(ctx, "op", "a", "api", apiSpec("a", "10.1.0.5"))
	require.NoError(t, err, "control: a live member of the network")
}

// An exposure an exposer stored before ADR 0114 stays the exposer's: the operator reads and releases
// it, and does not write over it.
func TestAnExposureAnExposerStoredIsReadAndReleasedButNotWrittenOverByTheOperator(t *testing.T) {
	s, st := fixture(t)
	ctx := t.Context()
	pool, err := st.GetPool(ctx, "lan")
	require.NoError(t, err)
	old := exposures.Record{Exposure: orchapi.Exposure{
		ID: "web", Network: "a", Owner: "exposer", Pool: "lan", Address: "192.168.0.85",
		Ports: spec("a").Ports, Backends: []string{"10.1.0.5"}, Phase: orchapi.ExposurePending,
	}}
	require.NoError(t, st.CreateExposure(ctx, old, pool, exposures.MaxExposuresPerNetwork))

	got, err := s.Exposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, "exposer", got.Owner)
	// The spec is the stored one but for its backends, so only the owner stands in the way.
	sp := spec("a", "10.1.0.5", "10.1.0.6")
	_, err = s.ApplyExposure(ctx, "op", "a", "web", sp)
	require.ErrorIs(t, err, inventory.ErrConflict, "the operator does not write over the exposer's")
	_, err = s.ApplyExposure(ctx, "op", "a", "api", apiSpec("a", "10.1.0.5"))
	require.NoError(t, err, "control: the operator writes an id of its own")

	require.NoError(t, s.DeleteExposure(ctx, "op", "a", "web"))
	rec, err := st.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, exposures.PhaseReleasing, rec.Phase, "the operator releases the exposer's")
}
