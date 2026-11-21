// Package etcd keeps the address pools and the exposures in orch's etcd. A pool is under
// /naru/orch/address-pools/<name>, an exposure under /naru/orch/exposures/<id>, and each
// exposure's address is claimed under /naru/orch/exposure-claims/<pool>/<address> in the same
// transaction that writes the exposure, so one address never belongs to two exposures.
package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
)

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

func claimKey(r exposures.Record) string { return ClaimPrefix + r.Pool + "/" + r.Address }

// PutPool writes p only while the stored pool is still prev, or absent when prev is nil.
func (s *Store) PutPool(ctx context.Context, p orchapi.AddressPool, prev *orchapi.AddressPool) error {
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
	t, err := s.kv.Txn(ctx).If(cond).Then(clientv3.OpPut(key, marshal(p))).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
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

// DeletePool removes the pool, or returns inventory.ErrNotFound.
func (s *Store) DeletePool(ctx context.Context, name string) error {
	resp, err := s.kv.Delete(ctx, PoolPrefix+name)
	if err != nil {
		return err
	}
	if resp.Deleted == 0 {
		return inventory.ErrNotFound
	}
	return nil
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

// CreateExposure writes r and its claim in one transaction that holds only while neither exists.
func (s *Store) CreateExposure(ctx context.Context, r exposures.Record) error {
	key := ExposurePrefix + r.ID
	t, err := s.kv.Txn(ctx).If(
		clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		clientv3.Compare(clientv3.CreateRevision(claimKey(r)), "=", 0),
	).Then(clientv3.OpPut(key, marshal(r)), clientv3.OpPut(claimKey(r), r.ID)).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
}

// UpdateExposure writes r only while the stored record is still prev.
func (s *Store) UpdateExposure(ctx context.Context, r, prev exposures.Record) error {
	key := ExposurePrefix + r.ID
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

// GetExposure returns the exposure, or inventory.ErrNotFound.
func (s *Store) GetExposure(ctx context.Context, id string) (exposures.Record, error) {
	resp, err := s.kv.Get(ctx, ExposurePrefix+id)
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

// DeleteExposure removes the record and releases its address, in one transaction. The claim is
// deleted only while it still names this exposure.
func (s *Store) DeleteExposure(ctx context.Context, id string) error {
	r, err := s.GetExposure(ctx, id)
	if errors.Is(err, inventory.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	key := ExposurePrefix + id
	_, err = s.kv.Txn(ctx).If(clientv3.Compare(clientv3.Value(claimKey(r)), "=", id)).
		Then(clientv3.OpDelete(key), clientv3.OpDelete(claimKey(r))).
		Else(clientv3.OpDelete(key)).Commit()
	return err
}

// ListExposures returns every exposure in id order.
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
