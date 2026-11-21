// Package etcd keeps the address pools and the exposures in orch's etcd. A pool is under
// /naru/orch/address-pools/<name>, an exposure under /naru/orch/exposures/<network>/<id>, and each
// exposure's address is claimed under /naru/orch/exposure-claims/<pool>/<address> in the same
// transaction that writes the exposure, so one address never belongs to two exposures. A count of
// a network's exposures is kept under /naru/orch/exposure-counts/<network> in those transactions.
package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/exposures"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// The keys' prefixes.
const (
	PoolPrefix     = "/naru/orch/address-pools/"
	ExposurePrefix = "/naru/orch/exposures/"
	ClaimPrefix    = "/naru/orch/exposure-claims/"
	CountPrefix    = "/naru/orch/exposure-counts/"
)

// retries bounds how often a write re-reads what a concurrent write changed.
const retries = 5

// Store is the pools and exposures over an etcd client.
type Store struct {
	kv clientv3.KV
}

var _ exposures.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("a record does not marshal: %v", err))
	}
	return string(b)
}

func claimKey(r exposures.Record) string    { return ClaimPrefix + r.Pool + "/" + r.Address }
func claimValue(r exposures.Record) string  { return r.Network + "/" + r.ID }
func exposureKey(network, id string) string { return ExposurePrefix + network + "/" + id }

// PutPool writes p only while the stored pool is still prev, or absent when prev is nil, and
// while no address of the pool has been claimed since check saw the claims.
func (s *Store) PutPool(ctx context.Context, p orchapi.AddressPool, prev *orchapi.AddressPool, check func([]exposures.Claim) error) error {
	key := PoolPrefix + p.Name
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
		if string(resp.Kvs[0].Value) != marshal(*prev) {
			return inventory.ErrConflict
		}
		cond = clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision)
	}
	claims, err := s.kv.Get(ctx, ClaimPrefix+p.Name+"/", clientv3.WithPrefix())
	if err != nil {
		return err
	}
	held := make([]exposures.Claim, 0, len(claims.Kvs))
	for _, kv := range claims.Kvs {
		c, err := decodeClaim(kv.Key, kv.Value)
		if err != nil {
			return err
		}
		held = append(held, c)
	}
	if err := check(held); err != nil {
		return err
	}
	unclaimed := clientv3.Compare(clientv3.CreateRevision(ClaimPrefix+p.Name+"/"), "<", claims.Header.Revision+1).WithPrefix()
	t, err := s.kv.Txn(ctx).If(cond, unclaimed).Then(clientv3.OpPut(key, marshal(p))).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
}

func decodeClaim(key, value []byte) (exposures.Claim, error) {
	network, id, ok := strings.Cut(string(value), "/")
	if !ok {
		return exposures.Claim{}, fmt.Errorf("claim %s: value %q is not network/id", key, value)
	}
	return exposures.Claim{Address: path.Base(string(key)), Network: network, ID: id}, nil
}

// GetPool returns the pool, or inventory.ErrNotFound.
func (s *Store) GetPool(ctx context.Context, name string) (orchapi.AddressPool, error) {
	resp, err := s.kv.Get(ctx, PoolPrefix+name)
	if err != nil {
		return orchapi.AddressPool{}, err
	}
	if len(resp.Kvs) == 0 {
		return orchapi.AddressPool{}, inventory.ErrNotFound
	}
	return decodePool(resp.Kvs[0].Key, resp.Kvs[0].Value)
}

func decodePool(key, value []byte) (orchapi.AddressPool, error) {
	var p orchapi.AddressPool
	if err := json.Unmarshal(value, &p); err != nil {
		return p, fmt.Errorf("record %s: %w", key, err)
	}
	return p, nil
}

// DeletePool removes the pool while none of its addresses is claimed, or returns
// inventory.ErrNotFound for a pool that is not there and inventory.ErrConflict for one in use.
func (s *Store) DeletePool(ctx context.Context, name string) error {
	key := PoolPrefix + name
	t, err := s.kv.Txn(ctx).If(
		clientv3.Compare(clientv3.CreateRevision(key), ">", 0),
		clientv3.Compare(clientv3.CreateRevision(ClaimPrefix+name+"/"), "=", 0).WithPrefix(),
	).Then(clientv3.OpDelete(key)).Else(clientv3.OpGet(key)).Commit()
	if err != nil {
		return err
	}
	switch {
	case t.Succeeded:
		return nil
	case len(t.Responses[0].GetResponseRange().Kvs) == 0:
		return inventory.ErrNotFound
	}
	return fmt.Errorf("%w: pool %s has exposures", inventory.ErrConflict, name)
}

// ListPools returns every pool in name order.
func (s *Store) ListPools(ctx context.Context) ([]orchapi.AddressPool, error) {
	resp, err := s.kv.Get(ctx, PoolPrefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.AddressPool, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		p, err := decodePool(kv.Key, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// CreateExposure writes r, its claim and the network's count in one transaction that holds only
// while neither the exposure nor the claim exists, the pool is the one given and the count is the
// one read.
func (s *Store) CreateExposure(ctx context.Context, r exposures.Record, pool orchapi.AddressPool, limit int) error {
	key, count := exposureKey(r.Network, r.ID), CountPrefix+r.Network
	for range retries {
		n, cond, err := s.count(ctx, count)
		if err != nil {
			return err
		}
		if n >= limit {
			return exposures.ErrFull
		}
		t, err := s.kv.Txn(ctx).If(
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(claimKey(r)), "=", 0),
			clientv3.Compare(clientv3.Value(PoolPrefix+pool.Name), "=", marshal(pool)),
			cond,
		).Then(clientv3.OpPut(key, marshal(r)), clientv3.OpPut(claimKey(r), claimValue(r)), clientv3.OpPut(count, strconv.Itoa(n+1))).
			Else(clientv3.OpGet(key), clientv3.OpGet(claimKey(r)), clientv3.OpGet(PoolPrefix+pool.Name)).Commit()
		if err != nil {
			return err
		}
		if t.Succeeded {
			return nil
		}
		// Only a changed count is worth another read; the rest is a conflict for the caller.
		if len(t.Responses[0].GetResponseRange().Kvs) > 0 || len(t.Responses[1].GetResponseRange().Kvs) > 0 || len(t.Responses[2].GetResponseRange().Kvs) == 0 {
			break
		}
		if string(t.Responses[2].GetResponseRange().Kvs[0].Value) != marshal(pool) {
			break
		}
	}
	return inventory.ErrConflict
}

// count reads a network's count and the condition that it is still what was read.
func (s *Store) count(ctx context.Context, key string) (int, clientv3.Cmp, error) {
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return 0, clientv3.Cmp{}, err
	}
	if len(resp.Kvs) == 0 {
		return 0, clientv3.Compare(clientv3.CreateRevision(key), "=", 0), nil
	}
	n, err := strconv.Atoi(string(resp.Kvs[0].Value))
	if err != nil {
		return 0, clientv3.Cmp{}, fmt.Errorf("count %s: %w", key, err)
	}
	return n, clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision), nil
}

// UpdateExposure writes r only while the stored record is still prev.
func (s *Store) UpdateExposure(ctx context.Context, r, prev exposures.Record) error {
	key := exposureKey(r.Network, r.ID)
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	if len(resp.Kvs) == 0 || !bytes.Equal(resp.Kvs[0].Value, []byte(marshal(prev))) {
		return inventory.ErrConflict
	}
	t, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision)).
		Then(clientv3.OpPut(key, marshal(r))).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
}

// GetExposure returns the network's exposure, or inventory.ErrNotFound.
func (s *Store) GetExposure(ctx context.Context, network, id string) (exposures.Record, error) {
	resp, err := s.kv.Get(ctx, exposureKey(network, id))
	if err != nil {
		return exposures.Record{}, err
	}
	if len(resp.Kvs) == 0 {
		return exposures.Record{}, inventory.ErrNotFound
	}
	return decodeRecord(resp.Kvs[0].Key, resp.Kvs[0].Value)
}

func decodeRecord(key, value []byte) (exposures.Record, error) {
	var r exposures.Record
	if err := json.Unmarshal(value, &r); err != nil {
		return r, fmt.Errorf("record %s: %w", key, err)
	}
	return r, nil
}

// DeleteExposure removes the record, its claim and one from the network's count, in one
// transaction that holds only while r is still the stored record. An exposure that is gone is not
// an error; one that changed is inventory.ErrConflict.
func (s *Store) DeleteExposure(ctx context.Context, r exposures.Record) error {
	key, count := exposureKey(r.Network, r.ID), CountPrefix+r.Network
	for range retries {
		n, cond, err := s.count(ctx, count)
		if err != nil {
			return err
		}
		t, err := s.kv.Txn(ctx).If(
			clientv3.Compare(clientv3.Value(key), "=", marshal(r)),
			clientv3.Compare(clientv3.Value(claimKey(r)), "=", claimValue(r)),
			cond,
		).Then(clientv3.OpDelete(key), clientv3.OpDelete(claimKey(r)), clientv3.OpPut(count, strconv.Itoa(max(n-1, 0)))).
			Else(clientv3.OpGet(key)).Commit()
		if err != nil {
			return err
		}
		if t.Succeeded {
			return nil
		}
		got := t.Responses[0].GetResponseRange().Kvs
		if len(got) == 0 {
			return nil
		}
		if string(got[0].Value) != marshal(r) {
			return inventory.ErrConflict
		}
	}
	return inventory.ErrConflict
}

// ListExposures returns every exposure in network and id order.
func (s *Store) ListExposures(ctx context.Context) ([]exposures.Record, error) {
	resp, err := s.kv.Get(ctx, ExposurePrefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, err
	}
	out := make([]exposures.Record, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		r, err := decodeRecord(kv.Key, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}
