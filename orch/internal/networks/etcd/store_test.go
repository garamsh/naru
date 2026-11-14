package etcd_test

import (
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
	require.NoError(t, s.Put(t.Context(), lab))
	require.NoError(t, s.Put(t.Context(), orchapi.Network{Name: "edge", Prefix: "10.8.0.0/24"}))
	got, err := s.Get(t.Context(), "lab")
	require.NoError(t, err)
	require.Equal(t, lab, got)

	all, err := s.List(t.Context())
	require.NoError(t, err)
	require.Equal(t, []orchapi.Network{{Name: "edge", Prefix: "10.8.0.0/24"}, lab}, all, "in name order")

	lab.Prefix = "10.7.0.0/24"
	require.NoError(t, s.Put(t.Context(), lab))
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
