// Package etcd keeps each proxy host's acknowledgement of the routing table in orch's etcd, under
// /naru/orch/route-acks/<host>, so a new leader waits on the same hosts and revisions.
package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/orch/internal/routes"
)

// AckPrefix is where each host's acknowledgement is kept, by host ID.
const AckPrefix = "/naru/orch/route-acks/"

// Store keeps the acknowledgements.
type Store struct{ kv clientv3.KV }

var _ routes.Acks = (*Store)(nil)

// New returns the store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

func (s *Store) Acks(ctx context.Context) (map[string]routes.Ack, error) {
	resp, err := s.kv.Get(ctx, AckPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	out := make(map[string]routes.Ack, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var a routes.Ack
		if err := json.Unmarshal(kv.Value, &a); err != nil {
			return nil, fmt.Errorf("ack %s: %w", kv.Key, err)
		}
		out[strings.TrimPrefix(string(kv.Key), AckPrefix)] = a
	}
	return out, nil
}

func (s *Store) Track(ctx context.Context, host string) error {
	key := AckPrefix + host
	_, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).Then(clientv3.OpPut(key, encode(routes.Ack{}))).Commit()
	return err
}

// Raise writes a when it is above the kept ack, guarded on the kept one's revision so that two
// writes never lower it.
func (s *Store) Raise(ctx context.Context, host string, a routes.Ack) error {
	key := AckPrefix + host
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var rev int64
	if len(resp.Kvs) == 1 {
		var kept routes.Ack
		if err := json.Unmarshal(resp.Kvs[0].Value, &kept); err != nil {
			return fmt.Errorf("ack %s: %w", key, err)
		}
		if a.Loaded <= kept.Loaded {
			return nil
		}
		rev = resp.Kvs[0].ModRevision
	}
	_, err = s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).Then(clientv3.OpPut(key, encode(a))).Commit()
	return err
}

func encode(a routes.Ack) string {
	b, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return string(b)
}
