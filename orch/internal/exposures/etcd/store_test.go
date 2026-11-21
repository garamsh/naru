package etcd_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	exposuresetcd "github.com/garamsh/naru/orch/internal/exposures/etcd"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func newStore(t *testing.T) *exposuresetcd.Store {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	return exposuresetcd.New(c.Members[0].Etcd())
}

func rec(id, address string) exposures.Record {
	return exposures.Record{Exposure: orchapi.Exposure{ID: id, Network: "a", Pool: "lan", Address: address}}
}

func TestPoolsRoundTripAndAreWrittenOnlyOverTheRecordRead(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	_, err := s.GetPool(ctx, "lan")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	p := orchapi.AddressPool{Name: "lan", CIDRs: []string{"192.168.0.80/29"}, Networks: []string{"a"}}
	require.NoError(t, s.PutPool(ctx, p, nil))
	require.ErrorIs(t, s.PutPool(ctx, p, nil), inventory.ErrConflict, "a create over an existing pool")
	got, err := s.GetPool(ctx, "lan")
	require.NoError(t, err)
	require.Equal(t, p, got)
	stale := p
	stale.Networks = []string{"x"}
	require.ErrorIs(t, s.PutPool(ctx, p, &stale), inventory.ErrConflict, "an update over a record that is not the stored one")
	require.NoError(t, s.PutPool(ctx, p, &got))
	all, err := s.ListPools(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.NoError(t, s.DeletePool(ctx, "lan"))
	require.ErrorIs(t, s.DeletePool(ctx, "lan"), inventory.ErrNotFound)
}

func TestOneAddressNeverBelongsToTwoExposures(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	require.NoError(t, s.CreateExposure(ctx, rec("web", "192.168.0.80")))
	require.ErrorIs(t, s.CreateExposure(ctx, rec("db", "192.168.0.80")), inventory.ErrConflict, "the address is claimed")
	require.ErrorIs(t, s.CreateExposure(ctx, rec("web", "192.168.0.81")), inventory.ErrConflict, "the id exists")
	_, err := s.GetExposure(ctx, "db")
	require.ErrorIs(t, err, inventory.ErrNotFound, "the refused create wrote nothing")

	require.NoError(t, s.DeleteExposure(ctx, "web"))
	require.NoError(t, s.DeleteExposure(ctx, "web"), "idempotent")
	require.NoError(t, s.CreateExposure(ctx, rec("db", "192.168.0.80")), "the claim went with the record")
}

func TestConcurrentCreatesOfOneAddressHaveOneWinner(t *testing.T) {
	s := newStore(t)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.CreateExposure(t.Context(), rec(fmt.Sprintf("s%d", i), "192.168.0.80")) == nil {
				won.Add(1)
			}
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, won.Load())
}

func TestAnUpdateOverAStaleRecordIsAConflict(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	r := rec("web", "192.168.0.80")
	require.NoError(t, s.CreateExposure(ctx, r))
	next := r
	next.Exposure.Host = "h1"
	require.NoError(t, s.UpdateExposure(ctx, next, r))
	require.ErrorIs(t, s.UpdateExposure(ctx, next, r), inventory.ErrConflict, "r is no longer the stored record")
	got, err := s.GetExposure(ctx, "web")
	require.NoError(t, err)
	require.True(t, got.Equal(next))
	list, err := s.ListExposures(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)
}
