// Package etcd keeps each proxy host's acknowledgement of the routing table in orch's etcd, under
// /naru/orch/route-acks/<host>, so a new leader waits on the same hosts and revisions.
package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// Track creates the host's ack at revision 0 when it has none, raises its MaxAge to maxAge when
// that is above the kept one, and sets LastSent to sent.
func (s *Store) Track(ctx context.Context, host string, maxAge time.Duration, sent time.Time) error {
	return s.change(ctx, host, func(kept routes.Ack) (routes.Ack, bool) {
		kept.MaxAge, kept.LastSent = max(kept.MaxAge, maxAge), sent
		return kept, true
	})
}

// Raise keeps loaded as the host's when it is above the kept one.
func (s *Store) Raise(ctx context.Context, host string, loaded int64) error {
	return s.change(ctx, host, func(kept routes.Ack) (routes.Ack, bool) {
		if loaded <= kept.Loaded {
			return kept, false
		}
		kept.Loaded = loaded
		return kept, true
	})
}

// Forget deletes the host's ack when it is still a, so a Track since is kept.
func (s *Store) Forget(ctx context.Context, host string, a routes.Ack) error {
	key := AckPrefix + host
	_, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.Value(key), "=", encode(a))).Then(clientv3.OpDelete(key)).Commit()
	return err
}

// change writes what f makes of the kept ack (the zero Ack when there is none), when f says to
// and it differs, guarded on the kept one's revision so that two writes never lower an ack.
func (s *Store) change(ctx context.Context, host string, f func(routes.Ack) (routes.Ack, bool)) error {
	key := AckPrefix + host
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var kept routes.Ack
	var rev int64
	if len(resp.Kvs) == 1 {
		if err := json.Unmarshal(resp.Kvs[0].Value, &kept); err != nil {
			return fmt.Errorf("ack %s: %w", key, err)
		}
		rev = resp.Kvs[0].ModRevision
	}
	next, ok := f(kept)
	if !ok || (rev != 0 && next == kept) {
		return nil
	}
	_, err = s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).Then(clientv3.OpPut(key, encode(next))).Commit()
	return err
}

func encode(a routes.Ack) string {
	b, err := json.Marshal(a)
	if err != nil {
		panic(err)
	}
	return string(b)
}
