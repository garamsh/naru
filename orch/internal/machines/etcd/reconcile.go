package etcd

import (
	"context"
	"encoding/json"
	"strings"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// What the reconciler reads and writes beside the Machine API: every snapshot, the deletions of
// snapshots still on their hosts, the hosts' allocations, and the writes that bind and remove a
// Machine together with its host's allocation.

func allocationKey(host string) string { return AllocationPrefix + host }

func (s *Store) AllSnapshots(ctx context.Context) ([]machines.SnapshotEntry, error) {
	resp, err := s.kv.Get(ctx, SnapshotPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	snaps, err := decode[machines.Snapshot](resp)
	if err != nil {
		return nil, err
	}
	out := make([]machines.SnapshotEntry, len(snaps))
	for i, snap := range snaps {
		out[i] = machines.SnapshotEntry{Snapshot: snap, Rev: resp.Kvs[i].ModRevision}
	}
	return out, nil
}

func (s *Store) UpdateSnapshot(ctx context.Context, snap machines.Snapshot, rev int64) (int64, error) {
	key := snapshotKey(snap.Caller, snap.Snapshot.Name)
	return s.guarded(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev)}, clientv3.OpPut(key, encode(snap)))
}

func (s *Store) Deletions(ctx context.Context) ([]machines.Deletion, error) {
	resp, err := s.kv.Get(ctx, DeletionPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decode[machines.Deletion](resp)
}

func (s *Store) DropDeletion(ctx context.Context, id string) error {
	_, err := s.kv.Delete(ctx, DeletionPrefix+id)
	return err
}

func (s *Store) Allocations(ctx context.Context) (map[string]machines.Allocation, error) {
	resp, err := s.kv.Get(ctx, AllocationPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	out := make(map[string]machines.Allocation, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var r hostapi.Resources
		if err := json.Unmarshal(kv.Value, &r); err != nil {
			return nil, err
		}
		out[strings.TrimPrefix(string(kv.Key), AllocationPrefix)] = machines.Allocation{Resources: r, Rev: kv.ModRevision}
	}
	return out, nil
}

// allocationGuard holds while the host's allocation is the one read at rev, none for 0.
func allocationGuard(host string, rev int64) clientv3.Cmp {
	if rev == 0 {
		return clientv3.Compare(clientv3.CreateRevision(allocationKey(host)), "=", 0)
	}
	return clientv3.Compare(clientv3.ModRevision(allocationKey(host)), "=", rev)
}

func (s *Store) Bind(ctx context.Context, r machines.Record, rev int64, alloc machines.Allocation) (int64, error) {
	key := machineKey(r.Caller, r.Name)
	return s.guarded(ctx,
		[]clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev), allocationGuard(r.Binding.Host, alloc.Rev)},
		clientv3.OpPut(key, encode(r)), clientv3.OpPut(allocationKey(r.Binding.Host), encode(alloc.Resources)))
}

func (s *Store) Remove(ctx context.Context, r machines.Record, rev int64, alloc *machines.Allocation, ev machineapi.Event) (int64, error) {
	key := machineKey(r.Caller, r.Name)
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev)}
	ops := []clientv3.Op{clientv3.OpDelete(key), clientv3.OpDelete(IDPrefix + r.ID), eventOp(r.Caller, ev)}
	if alloc != nil && r.Binding != nil {
		cmps = append(cmps, allocationGuard(r.Binding.Host, alloc.Rev))
		ops = append(ops, clientv3.OpPut(allocationKey(r.Binding.Host), encode(alloc.Resources)))
	}
	at, err := s.guarded(ctx, cmps, ops...)
	if err != nil {
		return 0, err
	}
	s.trim(ctx, r.Caller)
	return at, nil
}

// guarded commits ops while every cmp holds, and returns the revision it committed at;
// machines.ErrRaced when a cmp does not hold.
func (s *Store) guarded(ctx context.Context, cmps []clientv3.Cmp, ops ...clientv3.Op) (int64, error) {
	resp, err := s.kv.Txn(ctx).If(cmps...).Then(ops...).Commit()
	if err != nil {
		return 0, err
	}
	if !resp.Succeeded {
		return 0, machines.ErrRaced
	}
	return resp.Header.Revision, nil
}

// Watch sends on the channel it returns whenever a Machine or a snapshot is written, so the
// reconciler runs a pass at once after a Machine API write on any member (ADR 0109 item 4). A
// send that finds the last one unread is dropped: one pass covers both. The channel closes when ctx
// ends.
func Watch(ctx context.Context, w clientv3.Watcher) <-chan struct{} {
	out := make(chan struct{}, 1)
	ctx = clientv3.WithRequireLeader(ctx)
	machines := w.Watch(ctx, MachinePrefix, clientv3.WithPrefix())
	snapshots := w.Watch(ctx, SnapshotPrefix, clientv3.WithPrefix())
	go func() {
		defer close(out)
		for {
			select {
			case _, ok := <-machines:
				if !ok {
					return
				}
			case _, ok := <-snapshots:
				if !ok {
					return
				}
			}
			select {
			case out <- struct{}{}:
			default:
			}
		}
	}()
	return out
}
