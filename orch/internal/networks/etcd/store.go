// Package etcd keeps the networks in orch's etcd under /naru/orch/networks/.
package etcd

import (
	"context"
	"encoding/json"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/networks"
)

// Prefix is where a network is kept, under its name.
const Prefix = "/naru/orch/networks/"

// Store is the networks over an etcd client.
type Store struct {
	kv clientv3.KV
}

var _ networks.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

// Put creates the network or replaces it.
func (s *Store) Put(ctx context.Context, n orchapi.Network) error {
	b, err := json.Marshal(n)
	if err != nil {
		panic(fmt.Sprintf("a network does not marshal: %v", err))
	}
	_, err = s.kv.Put(ctx, Prefix+n.Name, string(b))
	return err
}

// Get returns the network, or inventory.ErrNotFound.
func (s *Store) Get(ctx context.Context, name string) (orchapi.Network, error) {
	resp, err := s.kv.Get(ctx, Prefix+name)
	if err != nil {
		return orchapi.Network{}, err
	}
	if len(resp.Kvs) == 0 {
		return orchapi.Network{}, inventory.ErrNotFound
	}
	return decode(resp.Kvs[0].Key, resp.Kvs[0].Value)
}

func decode(key, value []byte) (orchapi.Network, error) {
	var n orchapi.Network
	if err := json.Unmarshal(value, &n); err != nil {
		return orchapi.Network{}, fmt.Errorf("record %s: %w", key, err)
	}
	return n, nil
}

// Delete removes the network, or returns inventory.ErrNotFound.
func (s *Store) Delete(ctx context.Context, name string) error {
	resp, err := s.kv.Delete(ctx, Prefix+name)
	if err != nil {
		return err
	}
	if resp.Deleted == 0 {
		return inventory.ErrNotFound
	}
	return nil
}

// List returns every network, in name order.
func (s *Store) List(ctx context.Context) ([]orchapi.Network, error) {
	resp, err := s.kv.Get(ctx, Prefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Network, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		n, err := decode(kv.Key, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}
