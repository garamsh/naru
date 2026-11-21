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

// GuardKey is the key a network and an address pool both move when they are written with an overlap
// check, and both compare when they commit: a pool and a network that overlap, written from the same
// reading, cannot both be kept.
const GuardKey = "/naru/orch/address-space"

// Guard reads GuardKey and returns the comparison that holds while nothing has written it since.
func Guard(ctx context.Context, kv clientv3.KV) (clientv3.Cmp, error) {
	resp, err := kv.Get(ctx, GuardKey)
	if err != nil {
		return clientv3.Cmp{}, err
	}
	if len(resp.Kvs) == 0 {
		return clientv3.Compare(clientv3.CreateRevision(GuardKey), "=", 0), nil
	}
	return clientv3.Compare(clientv3.ModRevision(GuardKey), "=", resp.Kvs[0].ModRevision), nil
}

// MoveGuard is the operation that changes GuardKey.
func MoveGuard() clientv3.Op { return clientv3.OpPut(GuardKey, "1") }

// Store is the networks over an etcd client.
type Store struct {
	kv clientv3.KV
}

var _ networks.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

// Put writes n as one transaction that holds only while the stored network is still prev, or is
// still absent when prev is nil. It returns inventory.ErrConflict when another writer got there first.
// A non-nil check runs after the guard was read, and the guard is compared and moved in the transaction.
func (s *Store) Put(ctx context.Context, n orchapi.Network, prev *orchapi.Network, check func() error) error {
	b, err := json.Marshal(n)
	if err != nil {
		panic(fmt.Sprintf("a network does not marshal: %v", err))
	}
	key := Prefix + n.Name
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var cond clientv3.Cmp
	switch {
	case len(resp.Kvs) == 0 && prev == nil:
		cond = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	case len(resp.Kvs) == 0 || prev == nil:
		return inventory.ErrConflict
	default:
		cur, err := decode(resp.Kvs[0].Key, resp.Kvs[0].Value)
		if err != nil {
			return err
		}
		if cur != *prev {
			return inventory.ErrConflict
		}
		cond = clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision)
	}
	conds, ops := []clientv3.Cmp{cond}, []clientv3.Op{clientv3.OpPut(key, string(b))}
	if check != nil {
		guard, err := Guard(ctx, s.kv)
		if err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		conds, ops = append(conds, guard), append(ops, MoveGuard())
	}
	t, err := s.kv.Txn(ctx).If(conds...).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
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
