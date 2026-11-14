// Package crlsync keeps the signers of the control hosts on one revocation list. Each orch member
// stores the highest-sequence list it has seen in etcd, pushes it to its local signer, and stores
// what the signer holds afterwards when that is newer.
package crlsync

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"log/slog"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/identity"
)

// Key is where the list is kept in etcd.
const Key = "/naru/orch/crl"

// DefaultInterval is how often a member syncs.
const DefaultInterval = 10 * time.Second

// Peer is the local signer.
type Peer interface {
	// CRL returns the signed list the signer holds.
	CRL(ctx context.Context) ([]byte, error)
	// PushCRL gives the signer a list and returns the list it holds afterwards.
	PushCRL(ctx context.Context, data []byte) ([]byte, error)
}

// Store keeps one signed list with compare-and-swap.
type Store interface {
	// Get returns the stored list and its revision, or nil and 0 when there is none.
	Get(ctx context.Context) (data []byte, rev int64, err error)
	// Put stores data when the list is still at rev, and reports whether it did.
	Put(ctx context.Context, data []byte, rev int64) (bool, error)
}

// Syncer is one member's loop.
type Syncer struct {
	Store    Store
	Peer     Peer
	Key      ed25519.PublicKey
	Interval time.Duration
	Log      *slog.Logger
}

// Once syncs one time. A lost compare-and-swap is not an error: another member stored a list and
// the next sync reads it.
func (s *Syncer) Once(ctx context.Context) error {
	held, rev, err := s.Store.Get(ctx)
	if err != nil {
		return fmt.Errorf("read the stored list: %w", err)
	}
	var stored *identity.CRL
	if held != nil {
		if stored, err = identity.VerifyCRL(s.Key, held); err != nil {
			s.Log.Warn("the stored revocation list does not verify, it will be replaced", "err", err.Error())
			stored, held = nil, nil
		}
	}
	var got []byte
	if held != nil {
		got, err = s.Peer.PushCRL(ctx, held)
	} else {
		got, err = s.Peer.CRL(ctx)
	}
	if err != nil {
		return fmt.Errorf("the signer's list: %w", err)
	}
	crl, err := identity.VerifyCRL(s.Key, got)
	if err != nil {
		return fmt.Errorf("the signer's list: %w", err)
	}
	if stored != nil && crl.Seq <= stored.Seq {
		return nil
	}
	ok, err := s.Store.Put(ctx, got, rev)
	if err != nil {
		return fmt.Errorf("store the list: %w", err)
	}
	if !ok {
		s.Log.Debug("another member stored a list first, reading it next time")
	}
	return nil
}

// Run syncs every Interval until ctx ends. A failure is logged and tried again next time.
func (s *Syncer) Run(ctx context.Context) {
	interval := s.Interval
	if interval == 0 {
		interval = DefaultInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := s.Once(ctx); err != nil && ctx.Err() == nil {
			s.Log.Warn("revocation list sync failed", "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// EtcdStore keeps the list at Key in etcd.
type EtcdStore struct{ Client *clientv3.Client }

// Get implements Store.
func (e EtcdStore) Get(ctx context.Context) ([]byte, int64, error) {
	resp, err := e.Client.Get(ctx, Key)
	if err != nil {
		return nil, 0, err
	}
	if len(resp.Kvs) == 0 {
		return nil, 0, nil
	}
	return resp.Kvs[0].Value, resp.Kvs[0].ModRevision, nil
}

// Put implements Store; an absent key is at revision 0.
func (e EtcdStore) Put(ctx context.Context, data []byte, rev int64) (bool, error) {
	resp, err := e.Client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(Key), "=", rev)).
		Then(clientv3.OpPut(Key, string(data))).
		Commit()
	if err != nil {
		return false, err
	}
	return resp.Succeeded, nil
}
