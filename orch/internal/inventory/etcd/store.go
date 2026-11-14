// Package etcd keeps the inventory in orch's etcd under /naru/orch/.
package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/inventory"
)

// Prefixes of the records. Events sort by the time in their key, so a prefix read is the log in
// order.
const (
	keyPrefix     = "/naru/orch/enrollment-keys/"
	pendingPrefix = "/naru/orch/pending/"
	hostPrefix    = "/naru/orch/hosts/"
	eventPrefix   = "/naru/orch/events/"
)

// casRetries bounds the retries of a read-modify-write that lost a race.
const casRetries = 8

// Store is the inventory over an etcd client.
type Store struct {
	kv clientv3.KV
}

var _ inventory.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("a stored record does not marshal: %v", err))
	}
	return string(b)
}

func eventOp(ev orchapi.Event) clientv3.Op {
	var salt [4]byte
	_, _ = rand.Read(salt[:])
	return clientv3.OpPut(fmt.Sprintf("%s%020d-%s", eventPrefix, ev.Time.UnixNano(), hex.EncodeToString(salt[:])), encode(ev))
}

// get reads one record into out and returns its modification revision.
func (s *Store) get(ctx context.Context, key string, out any) (int64, error) {
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return 0, err
	}
	if len(resp.Kvs) == 0 {
		return 0, inventory.ErrNotFound
	}
	return resp.Kvs[0].ModRevision, json.Unmarshal(resp.Kvs[0].Value, out)
}

func list[T any](ctx context.Context, s *Store, prefix string) ([]T, error) {
	resp, err := s.kv.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	out := make([]T, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var v T
		if err := json.Unmarshal(kv.Value, &v); err != nil {
			return nil, fmt.Errorf("record %s: %w", kv.Key, err)
		}
		out = append(out, v)
	}
	return out, nil
}

// update reads the record at key, applies change and writes it back with ops, retrying when
// another writer got in between.
func update[T any](ctx context.Context, s *Store, key string, change func(*T) error, ops ...clientv3.Op) (T, error) {
	var v T
	for range casRetries {
		rev, err := s.get(ctx, key, &v)
		if err != nil {
			return v, err
		}
		if err := change(&v); err != nil {
			return v, err
		}
		put := append([]clientv3.Op{clientv3.OpPut(key, encode(v))}, ops...)
		resp, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).Then(put...).Commit()
		if err != nil {
			return v, err
		}
		if resp.Succeeded {
			return v, nil
		}
	}
	return v, errors.New("the record kept changing under the update")
}

func (s *Store) PutKey(ctx context.Context, k inventory.Key) error {
	_, err := s.kv.Put(ctx, keyPrefix+k.ID, encode(k))
	return err
}

func (s *Store) GetKey(ctx context.Context, id string) (inventory.Key, error) {
	var k inventory.Key
	_, err := s.get(ctx, keyPrefix+id, &k)
	return k, err
}

func (s *Store) RevokeKey(ctx context.Context, id string, ev orchapi.Event) error {
	_, err := update(ctx, s, keyPrefix+id, func(k *inventory.Key) error { k.Revoked = true; return nil }, eventOp(ev))
	return err
}

func (s *Store) PutPending(ctx context.Context, p inventory.Pending, ev *orchapi.Event) error {
	ops := []clientv3.Op{clientv3.OpPut(pendingPrefix+p.HostID, encode(p))}
	if ev != nil {
		ops = append(ops, eventOp(*ev))
	}
	_, err := s.kv.Txn(ctx).Then(ops...).Commit()
	return err
}

func (s *Store) GetPending(ctx context.Context, hostID string) (inventory.Pending, error) {
	var p inventory.Pending
	_, err := s.get(ctx, pendingPrefix+hostID, &p)
	return p, err
}

func (s *Store) ListPending(ctx context.Context) ([]inventory.Pending, error) {
	return list[inventory.Pending](ctx, s, pendingPrefix)
}

func (s *Store) Admit(ctx context.Context, p inventory.Pending, h inventory.Host, ev orchapi.Event) error {
	key := pendingPrefix + p.HostID
	resp, err := s.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), ">", 0)).
		Then(clientv3.OpDelete(key), clientv3.OpPut(hostPrefix+h.ID, encode(h)), eventOp(ev)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return inventory.ErrNotFound
	}
	return nil
}

func (s *Store) GetHost(ctx context.Context, id string) (inventory.Host, error) {
	var h inventory.Host
	_, err := s.get(ctx, hostPrefix+id, &h)
	return h, err
}

func (s *Store) ListHosts(ctx context.Context) ([]inventory.Host, error) {
	return list[inventory.Host](ctx, s, hostPrefix)
}

func (s *Store) UpdateHost(ctx context.Context, id string, change func(*inventory.Host), ev *orchapi.Event) (inventory.Host, error) {
	var ops []clientv3.Op
	if ev != nil {
		ops = append(ops, eventOp(*ev))
	}
	return update(ctx, s, hostPrefix+id, func(h *inventory.Host) error { change(h); return nil }, ops...)
}

func (s *Store) AppendEvent(ctx context.Context, ev orchapi.Event) error {
	_, err := s.kv.Do(ctx, eventOp(ev))
	return err
}

func (s *Store) ListEvents(ctx context.Context) ([]orchapi.Event, error) {
	return list[orchapi.Event](ctx, s, eventPrefix)
}
