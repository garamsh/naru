package etcd_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// raceKV runs race once, right after the first read of a host, as a writer that got in between.
type raceKV struct {
	clientv3.KV
	race func()
}

func (r *raceKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	resp, err := r.KV.Get(ctx, key, opts...)
	if race := r.race; race != nil {
		r.race = nil
		race()
	}
	return resp, err
}

func TestARetriedHostUpdateAppliesItsChangeToTheValueItReRead(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	kv := &raceKV{KV: c.Members[0].Etcd()}
	s := invetcd.New(kv)

	const key = "/naru/orch/hosts/h1"
	draining, err := json.Marshal(inventory.Host{ID: "h1", Region: "kr-seoul", Draining: true, Labels: map[string]string{"rack": "a"}})
	require.NoError(t, err)
	// An undrain stores a document with no draining field and no labels at all.
	undrained := `{"id":"h1","region":"kr-seoul"}`
	_, err = kv.KV.Put(t.Context(), key, string(draining))
	require.NoError(t, err)

	attempts := 0
	kv.race = func() {
		_, err := kv.KV.Put(t.Context(), key, undrained)
		require.NoError(t, err)
	}
	seen := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	got, err := s.UpdateHost(t.Context(), "h1", func(h *inventory.Host) { attempts++; h.LastSeen = seen }, nil)
	require.NoError(t, err)

	require.Equal(t, 2, attempts, "the race must have forced exactly one retry")
	require.False(t, got.Draining)
	require.Nil(t, got.Labels)
	require.True(t, got.LastSeen.Equal(seen))

	stored, err := s.GetHost(t.Context(), "h1")
	require.NoError(t, err)
	require.False(t, stored.Draining, "the stored host kept the draining flag of the first attempt")
	require.Nil(t, stored.Labels, "the stored host kept the labels of the first attempt")
}
