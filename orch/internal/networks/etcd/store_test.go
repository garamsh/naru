package etcd_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func newStore(t *testing.T) *netetcd.Store {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	return netetcd.New(c.Members[0].Etcd())
}

func TestNetworksRoundTripThroughTheStore(t *testing.T) {
	s := newStore(t)
	_, err := s.Get(t.Context(), "lab")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	none, err := s.List(t.Context())
	require.NoError(t, err)
	require.Empty(t, none)

	lab := orchapi.Network{Name: "lab", Prefix: "10.9.0.0/24", APIEndpoint: "10.9.0.2"}
	require.NoError(t, s.Put(t.Context(), lab, nil, nil))
	require.NoError(t, s.Put(t.Context(), orchapi.Network{Name: "edge", Prefix: "10.8.0.0/24"}, nil, nil))
	got, err := s.Get(t.Context(), "lab")
	require.NoError(t, err)
	require.Equal(t, lab, got)

	all, err := s.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []orchapi.Network{{Name: "edge", Prefix: "10.8.0.0/24"}, lab}, all, "in name order")

	old := lab
	lab.Prefix = "10.7.0.0/24"
	require.NoError(t, s.Put(t.Context(), lab, &old, nil))
	got, err = s.Get(t.Context(), "lab")
	require.NoError(t, err)
	require.Equal(t, "10.7.0.0/24", got.Prefix, "a put replaces")

	require.NoError(t, s.Delete(t.Context(), "lab"))
	require.ErrorIs(t, s.Delete(t.Context(), "lab"), inventory.ErrNotFound)
	_, err = s.Get(t.Context(), "lab")
	require.ErrorIs(t, err, inventory.ErrNotFound)
	all, err = s.List(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 1)
}

func TestOnlyOneOfManyCreatesOfANetworkWins(t *testing.T) {
	s := newStore(t)
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Put(t.Context(), orchapi.Network{Name: "lab", Prefix: fmt.Sprintf("10.%d.0.0/24", i)}, nil, nil)
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, inventory.ErrConflict):
				conflicts.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
	require.Equal(t, int32(7), conflicts.Load())
}

func TestAReplaceOfANetworkThatChangedMeanwhileIsRefused(t *testing.T) {
	s := newStore(t)
	a := orchapi.Network{Name: "lab", Prefix: "10.9.0.0/24"}
	require.NoError(t, s.Put(t.Context(), a, nil, nil))
	b := a
	b.Prefix = "10.8.0.0/24"
	require.NoError(t, s.Put(t.Context(), b, &a, nil))
	c := a
	c.Prefix = "10.7.0.0/24"
	require.ErrorIs(t, s.Put(t.Context(), c, &a, nil), inventory.ErrConflict, "a was read before b replaced it")
	require.ErrorIs(t, s.Put(t.Context(), c, nil, nil), inventory.ErrConflict, "the network exists")
}
