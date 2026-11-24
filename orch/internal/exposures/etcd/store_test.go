package etcd_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	exposuresetcd "github.com/garamsh/naru/orch/internal/exposures/etcd"
	"github.com/garamsh/naru/orch/internal/inventory"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func newStore(t *testing.T) *exposuresetcd.Store {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	return exposuresetcd.New(c.Members[0].Etcd())
}

var lan = orchapi.AddressPool{Name: "lan", CIDRs: []string{"192.168.0.0/24"}, Networks: []string{"a", "b"}}

func rec(id, address string) exposures.Record { return recOn("a", id, address) }

func recOn(network, id, address string) exposures.Record {
	return exposures.Record{Exposure: orchapi.Exposure{ID: id, Network: network, Pool: "lan", Address: address}}
}

func noCheck([]exposures.Claim) error { return nil }

// withPool returns a store holding the pool lan.
func withPool(t *testing.T) *exposuresetcd.Store {
	t.Helper()
	s := newStore(t)
	require.NoError(t, s.PutPool(t.Context(), lan, nil, noCheck))
	return s
}

const limit = 64

func TestPoolsRoundTripAndAreWrittenOnlyOverTheRecordRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	_, err := s.GetPool(ctx, "lan")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	p := orchapi.AddressPool{Name: "lan", CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}}
	require.NoError(t, s.PutPool(ctx, p, nil, noCheck))
	require.ErrorIs(t, s.PutPool(ctx, p, nil, noCheck), inventory.ErrConflict, "a create over an existing pool")
	got, err := s.GetPool(ctx, "lan")
	require.NoError(t, err)
	require.Equal(t, p, got)
	stale := p
	stale.Networks = []string{"x"}
	require.ErrorIs(t, s.PutPool(ctx, p, &stale, noCheck), inventory.ErrConflict, "an update over a record that is not the stored one")
	require.NoError(t, s.PutPool(ctx, p, &got, noCheck))
	all, err := s.ListPools(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.NoError(t, s.DeletePool(ctx, "lan"))
	require.ErrorIs(t, s.DeletePool(ctx, "lan"), inventory.ErrNotFound)
}

func TestOneAddressNeverBelongsToTwoExposures(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, rec("web", "192.168.0.80"), lan, limit))
	require.ErrorIs(t, s.CreateExposure(ctx, rec("db", "192.168.0.80"), lan, limit), inventory.ErrConflict, "the address is claimed")
	require.ErrorIs(t, s.CreateExposure(ctx, rec("web", "192.168.0.81"), lan, limit), inventory.ErrConflict, "the id exists")
	_, err := s.GetExposure(ctx, "a", "db")
	require.ErrorIs(t, err, inventory.ErrNotFound, "the refused create wrote nothing")

	require.NoError(t, s.DeleteExposure(ctx, rec("web", "192.168.0.80")))
	require.NoError(t, s.DeleteExposure(ctx, rec("web", "192.168.0.80")), "idempotent")
	require.NoError(t, s.CreateExposure(ctx, rec("db", "192.168.0.80"), lan, limit), "the claim went with the record")
}

func TestAnIdIsKeptUnderItsNetwork(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, recOn("a", "web", "192.168.0.80"), lan, limit))
	require.NoError(t, s.CreateExposure(ctx, recOn("b", "web", "192.168.0.81"), lan, limit), "the same id on another network is another exposure")
	a, err := s.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", a.Address)
	_, err = s.GetExposure(ctx, "c", "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	// A delete that names the other network's record cannot remove this one.
	require.NoError(t, s.DeleteExposure(ctx, recOn("c", "web", "192.168.0.80")))
	_, err = s.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
}

func TestAProxyExposureIsKeptApartFromEveryNetworkAndCountedOnItsOwn(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, recOn("", "web", "192.168.0.80"), lan, 1))
	require.NoError(t, s.CreateExposure(ctx, recOn("a", "web", "192.168.0.81"), lan, 1), "a network's id and count are its own")
	require.ErrorIs(t, s.CreateExposure(ctx, recOn("", "db", "192.168.0.82"), lan, 1), exposures.ErrFull, "the proxy exposures' own count")
	require.ErrorIs(t, s.CreateExposure(ctx, recOn("b", "db", "192.168.0.80"), lan, limit), inventory.ErrConflict, "one claim space")
	p, err := s.GetExposure(ctx, "", "web")
	require.NoError(t, err)
	require.Equal(t, "192.168.0.80", p.Address)
	require.Empty(t, p.Network)

	// The claim reads back as the proxy exposure's, with no network, so a pool's check tells it apart.
	var claims []exposures.Claim
	require.ErrorIs(t, s.PutPool(ctx, lan, &lan, func(c []exposures.Claim) error { claims = c; return inventory.ErrConflict }), inventory.ErrConflict)
	require.ElementsMatch(t, []exposures.Claim{{Address: "192.168.0.80", ID: "web"}, {Address: "192.168.0.81", Network: "a", ID: "web"}}, claims)

	require.NoError(t, s.DeleteExposure(ctx, p))
	_, err = s.GetExposure(ctx, "a", "web")
	require.NoError(t, err, "control: the network's web stays")
	require.NoError(t, s.CreateExposure(ctx, recOn("", "db", "192.168.0.80"), lan, 1), "the count and the claim went with the record")
}

func TestConcurrentCreatesOfOneAddressHaveOneWinner(t *testing.T) {
	s := withPool(t)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.CreateExposure(t.Context(), rec(fmt.Sprintf("s%d", i), "192.168.0.80"), lan, limit) == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, won.Load())
}

func TestANetworkNeverHoldsMoreThanItsLimitWhenCreatesRace(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	const room = 2
	for i := range limit - room {
		require.NoError(t, s.CreateExposure(ctx, rec(fmt.Sprintf("pre%d", i), fmt.Sprintf("192.168.0.%d", i)), lan, limit))
	}
	var won, full atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			switch err := s.CreateExposure(ctx, rec(fmt.Sprintf("race%d", i), fmt.Sprintf("192.168.1.%d", i)), lan, limit); {
			case err == nil:
				won.Add(1)
			case errors.Is(err, exposures.ErrFull):
				full.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, room, won.Load(), "exactly the room that was left")
	require.EqualValues(t, 8-room, full.Load())
	list, err := s.ListExposures(ctx)
	require.NoError(t, err)
	require.Len(t, list, limit)
	// Control: another network has its own count, and a delete gives the room back.
	require.NoError(t, s.CreateExposure(ctx, recOn("b", "web", "192.168.2.1"), lan, limit))
	require.NoError(t, s.DeleteExposure(ctx, rec("pre0", "192.168.0.0")))
	require.NoError(t, s.CreateExposure(ctx, rec("after", "192.168.3.1"), lan, limit))
	require.ErrorIs(t, s.CreateExposure(ctx, rec("over", "192.168.3.2"), lan, limit), exposures.ErrFull)
}

func TestACreateIntoAPoolThatChangedIsAConflict(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	changed := lan
	changed.Networks = []string{"b"}
	require.NoError(t, s.PutPool(ctx, changed, &lan, noCheck))
	require.ErrorIs(t, s.CreateExposure(ctx, rec("web", "192.168.0.80"), lan, limit), inventory.ErrConflict, "planned from the pool as it was")
	require.NoError(t, s.CreateExposure(ctx, recOn("b", "web", "192.168.0.80"), changed, limit), "control: from the pool as it is")

	require.NoError(t, s.DeleteExposure(ctx, recOn("b", "web", "192.168.0.80")))
}

func TestAPoolInUseCannotBeDeletedOrReplacedPastItsClaims(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, rec("web", "192.168.0.80"), lan, limit))
	require.ErrorIs(t, s.DeletePool(ctx, "lan"), inventory.ErrConflict)
	narrower := lan
	narrower.CIDRs = []string{"192.168.1.0/24"}
	refuse := func(held []exposures.Claim) error {
		if len(held) > 0 {
			return inventory.ErrConflict
		}
		return nil
	}
	require.ErrorIs(t, s.PutPool(ctx, narrower, &lan, refuse), inventory.ErrConflict, "the check sees the claim")
	require.NoError(t, s.DeleteExposure(ctx, rec("web", "192.168.0.80")))
	require.NoError(t, s.PutPool(ctx, narrower, &lan, refuse), "control: no claim left")
	require.NoError(t, s.DeletePool(ctx, "lan"))
}

func TestAnUpdateOverAStaleRecordIsAConflict(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	r := rec("web", "192.168.0.80")
	require.NoError(t, s.CreateExposure(ctx, r, lan, limit))
	next := r
	next.Exposure.Host = "h1"
	require.NoError(t, s.UpdateExposure(ctx, next, r))
	require.ErrorIs(t, s.UpdateExposure(ctx, next, r), inventory.ErrConflict, "r is no longer the stored record")
	got, err := s.GetExposure(ctx, "a", "web")
	require.NoError(t, err)
	require.True(t, got.Equal(next))
	list, err := s.ListExposures(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.ErrorIs(t, s.DeleteExposure(ctx, r), inventory.ErrConflict, "a delete over a record that changed")
	require.NoError(t, s.DeleteExposure(ctx, next))
}

func TestAPoolIsNotReplacedOverAClaimMadeAfterItsCheck(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	narrower := lan
	narrower.CIDRs = []string{"192.168.1.0/24"}
	// The exposure is created after the check read the claims and before the pool is written.
	raced := func([]exposures.Claim) error {
		return s.CreateExposure(ctx, rec("web", "192.168.0.80"), lan, limit)
	}
	require.ErrorIs(t, s.PutPool(ctx, narrower, &lan, raced), inventory.ErrConflict)
	got, err := s.GetPool(ctx, "lan")
	require.NoError(t, err)
	require.Equal(t, lan, got, "the pool is as it was")

	// Control: with no claim in between the same replacement is written.
	require.NoError(t, s.DeleteExposure(ctx, rec("web", "192.168.0.80")))
	require.NoError(t, s.PutPool(ctx, narrower, &lan, noCheck))
}

func TestAPoolIsNotDeletedUnderAClaimMadeJustBefore(t *testing.T) {
	s := withPool(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, rec("web", "192.168.0.80"), lan, limit))
	require.ErrorIs(t, s.DeletePool(ctx, "lan"), inventory.ErrConflict)
	_, err := s.GetPool(ctx, "lan")
	require.NoError(t, err)
	require.NoError(t, s.DeleteExposure(ctx, rec("web", "192.168.0.80")))
	require.NoError(t, s.DeletePool(ctx, "lan"), "control: no claim")
}

// A network and a pool that overlap, each checked against the other's absence, are never both kept:
// the one that commits second finds the address-space guard moved.
func TestAPoolAndANetworkWrittenFromTheSameReadingAreNotBothKept(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := c.Members[0].Etcd()
	ctx := t.Context()
	ps, ns := exposuresetcd.New(kv), netetcd.New(kv)
	net := orchapi.Network{Name: "lab", Prefix: "192.168.0.0/24"}
	other := orchapi.Network{Name: "far", Prefix: "10.9.0.0/24"}
	pass := func() error { return nil }

	// A network is written while the pool is between its check and its commit.
	err := ps.PutPool(ctx, lan, nil, func([]exposures.Claim) error { return ns.Put(ctx, net, nil, pass) })
	require.ErrorIs(t, err, inventory.ErrConflict, "the network moved the guard after the pool read it")
	_, err = ps.GetPool(ctx, "lan")
	require.ErrorIs(t, err, inventory.ErrNotFound, "so the pool was not kept")
	_, err = ns.Get(ctx, "lab")
	require.NoError(t, err, "and the network was")

	// A pool is written while the network is between its check and its commit.
	err = ns.Put(ctx, other, nil, func() error { return ps.PutPool(ctx, lan, nil, noCheck) })
	require.ErrorIs(t, err, inventory.ErrConflict, "the pool moved the guard after the network read it")
	_, err = ns.Get(ctx, "far")
	require.ErrorIs(t, err, inventory.ErrNotFound)

	// Control: with nothing between a check and its commit both are kept.
	require.NoError(t, ns.Put(ctx, other, nil, pass))
	require.NoError(t, ps.PutPool(ctx, orchapi.AddressPool{Name: "wan", CIDRs: []string{"172.16.0.0/24"}, Networks: []string{"a"}}, nil, noCheck))
}
