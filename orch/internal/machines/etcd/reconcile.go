package etcd

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
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

func (s *Store) UpdateSnapshot(ctx context.Context, snap machines.Snapshot, rev int64, del *machines.Deletion) (int64, error) {
	key := snapshotKey(snap.Caller, snap.Snapshot.Name)
	ops := []clientv3.Op{clientv3.OpPut(key, encode(snap))}
	if del != nil {
		ops = append(ops, clientv3.OpPut(DeletionPrefix+del.ID, encode(*del)))
	}
	return s.guarded(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev)}, ops...)
}

func (s *Store) PutDeletion(ctx context.Context, d machines.Deletion) error {
	_, err := s.kv.Put(ctx, DeletionPrefix+d.ID, encode(d))
	return err
}

func (s *Store) Tombstones(ctx context.Context) (map[string]machines.Tombstone, error) {
	resp, err := s.kv.Get(ctx, TombstonePrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	all, err := decode[machines.Tombstone](resp)
	if err != nil {
		return nil, err
	}
	out := make(map[string]machines.Tombstone, len(all))
	for _, t := range all {
		out[t.ID] = t
	}
	return out, nil
}

func (s *Store) DropTombstone(ctx context.Context, id string) error {
	_, err := s.kv.Delete(ctx, TombstonePrefix+id)
	return err
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

func (s *Store) Allocate(ctx context.Context, r machines.Record, rev int64, host string, alloc machines.Allocation, ev *machineapi.Event) (int64, error) {
	key := machineKey(r.Caller, r.Name)
	ops := []clientv3.Op{clientv3.OpPut(key, encode(r)), clientv3.OpPut(allocationKey(host), encode(alloc.Resources))}
	if ev != nil {
		ops = append(ops, eventOp(r.Caller, *ev))
	}
	at, err := s.guarded(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev), allocationGuard(host, alloc.Rev)}, ops...)
	if err != nil {
		return 0, err
	}
	if ev != nil {
		s.trim(ctx, r.Caller)
	}
	return at, nil
}

func (s *Store) Remove(ctx context.Context, r machines.Record, rev int64, alloc *machines.Allocation, ev machineapi.Event) (int64, error) {
	key := machineKey(r.Caller, r.Name)
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev)}
	ops := []clientv3.Op{
		clientv3.OpDelete(key), clientv3.OpDelete(IDPrefix + r.ID), eventOp(r.Caller, ev),
		clientv3.OpPut(TombstonePrefix+r.ID, encode(machines.Tombstone{ID: r.ID, Removed: ev.Time})),
	}
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

// Watch sends on the channel it returns whenever the Machine API writes something the reconciler
// acts on, so it runs a pass at once after an API write on any member (ADR 0109 item 4): a Machine
// created, given a new generation or marked deleting, and a snapshot created or deleted. The
// reconciler's own writes (a phase, a binding, a snapshot's host) wake nothing, so a write it makes
// in a pass does not start another. A send that finds the last one unread is dropped: one pass
// covers both.
//
// A watch that closes, as one does when its etcd member loses its leader, is opened again after a
// backoff from one second to thirty, with a log line, and a send follows: what was written while
// it was closed is covered by that pass. The channel closes when ctx ends.
func Watch(ctx context.Context, w clientv3.Watcher, log *slog.Logger) <-chan struct{} {
	out := make(chan struct{}, 1)
	wake := func() {
		select {
		case out <- struct{}{}:
		default:
		}
	}
	go func() {
		defer close(out)
		backoff := time.Second
		for first := true; ; first = false {
			if !first {
				log.Warn("the Machine watch closed; until it is open again the reconciler runs on its interval alone", "retry", backoff.String())
				select {
				case <-ctx.Done():
					return
				case <-time.After(backoff):
				}
				backoff = min(2*backoff, 30*time.Second)
			}
			wctx, cancel := context.WithCancel(clientv3.WithRequireLeader(ctx))
			machines := w.Watch(wctx, MachinePrefix, clientv3.WithPrefix(), clientv3.WithPrevKV())
			snapshots := w.Watch(wctx, SnapshotPrefix, clientv3.WithPrefix())
			if !first {
				log.Info("the Machine watch is open again")
				wake()
			}
			watchUntilClosed(machines, snapshots, wake, func() { backoff = time.Second })
			cancel()
			if ctx.Err() != nil {
				return
			}
		}
	}()
	return out
}

// watchUntilClosed reads both watches until either closes or fails, waking for each write the API
// makes, and calls ok on each response that carries events.
func watchUntilClosed(machines, snapshots clientv3.WatchChan, wake, ok func()) {
	for {
		select {
		case resp, open := <-machines:
			if !open || resp.Err() != nil {
				return
			}
			ok()
			if slices.ContainsFunc(resp.Events, apiWrote) {
				wake()
			}
		case resp, open := <-snapshots:
			if !open || resp.Err() != nil {
				return
			}
			ok()
			if slices.ContainsFunc(resp.Events, func(e *clientv3.Event) bool { return e.IsCreate() || e.Type == mvccpb.DELETE }) {
				wake()
			}
		}
	}
}

// apiWrote reports whether the Machine's write is one only the Machine API makes: a create, a new
// generation, or the mark of a delete.
func apiWrote(e *clientv3.Event) bool {
	if e.Type != mvccpb.PUT || e.IsCreate() || e.PrevKv == nil {
		return e.Type == mvccpb.PUT
	}
	var was, is struct {
		Status struct {
			Phase      machineapi.Phase `json:"phase"`
			Generation uint64           `json:"generation"`
		} `json:"status"`
	}
	if json.Unmarshal(e.PrevKv.Value, &was) != nil || json.Unmarshal(e.Kv.Value, &is) != nil {
		return true
	}
	return is.Status.Generation != was.Status.Generation ||
		is.Status.Phase == machineapi.PhaseDeleting && was.Status.Phase != machineapi.PhaseDeleting
}
