// Package etcd keeps the Machine API's records in orch's etcd:
//
//   - a Machine under /naru/orch/machines/<caller>/<name>, its id claimed under
//     /naru/orch/machine-ids/<id> in the transaction that creates it, so no two Machines share one;
//   - a snapshot under /naru/orch/snapshots/<caller>/<name>;
//   - the zone of the stable names, which every member is started with, under /naru/orch/machine-zone;
//   - each caller's event log under /naru/orch/machine-events/<caller>/, apart from the fleet's,
//     with the revision of the newest event it dropped under /naru/orch/machine-event-floors/<caller>.
//
// An event is appended in the transaction that writes the status it carries, and its cursor is the
// revision that committed it, as in the fleet's log.
package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// The keys' prefixes.
const (
	MachinePrefix  = "/naru/orch/machines/"
	IDPrefix       = "/naru/orch/machine-ids/"
	SnapshotPrefix = "/naru/orch/snapshots/"
	EventPrefix    = "/naru/orch/machine-events/"
	FloorPrefix    = "/naru/orch/machine-event-floors/"
	ZoneKey        = "/naru/orch/machine-zone"
)

// maxEvents and maxEventAge bound each caller's log: its newest 20000 events, none older than 7
// days (ADR 0108 item 6).
const (
	maxEvents   = 20000
	maxEventAge = 7 * 24 * time.Hour
)

// trimBatch is how many events one trim transaction drops, below etcd's 128 operations.
const trimBatch = 100

// Store is the records over an etcd client.
type Store struct {
	kv clientv3.KV
	// keepEvents is how many events a caller's log keeps; maxEvents unless a test says less.
	keepEvents int
	// now dates the age bound of the logs.
	now func() time.Time
}

var _ machines.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv, keepEvents: maxEvents, now: time.Now} }

func machineKey(caller, name string) string  { return MachinePrefix + caller + "/" + name }
func snapshotKey(caller, name string) string { return SnapshotPrefix + caller + "/" + name }
func eventsOf(caller string) string          { return EventPrefix + caller + "/" }
func floorKey(caller string) string          { return FloorPrefix + caller }

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("a stored record does not marshal: %v", err))
	}
	return string(b)
}

// eventOp appends ev to the caller's log, under a key that sorts by its time.
func eventOp(caller string, ev machineapi.Event) clientv3.Op {
	var salt [4]byte
	_, _ = rand.Read(salt[:])
	return clientv3.OpPut(fmt.Sprintf("%s%020d-%s", eventsOf(caller), ev.Time.UnixNano(), hex.EncodeToString(salt[:])), encode(ev))
}

func (s *Store) Machine(ctx context.Context, caller, name string) (machines.Record, int64, error) {
	resp, err := s.kv.Get(ctx, machineKey(caller, name))
	if err != nil {
		return machines.Record{}, 0, err
	}
	if len(resp.Kvs) == 0 {
		return machines.Record{}, 0, machines.ErrNotFound
	}
	var r machines.Record
	return r, resp.Kvs[0].ModRevision, json.Unmarshal(resp.Kvs[0].Value, &r)
}

func (s *Store) Machines(ctx context.Context, caller string) ([]machines.Record, int64, error) {
	resp, err := s.kv.Get(ctx, MachinePrefix+caller+"/", clientv3.WithPrefix())
	if err != nil {
		return nil, 0, err
	}
	out, err := decode[machines.Record](resp)
	return out, resp.Header.Revision, err
}

func (s *Store) All(ctx context.Context) ([]machines.Record, error) {
	resp, err := s.kv.Get(ctx, MachinePrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decode[machines.Record](resp)
}

// decode reads every value of resp, which etcd orders by key.
func decode[T any](resp *clientv3.GetResponse) ([]T, error) {
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

func (s *Store) Create(ctx context.Context, r machines.Record, from int64, ev machineapi.Event) error {
	key, id := machineKey(r.Caller, r.Name), IDPrefix+r.ID
	guards := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0), clientv3.Compare(clientv3.CreateRevision(id), "=", 0)}
	if src := r.Spec.FromSnapshot; src != nil {
		guards = append(guards, clientv3.Compare(clientv3.ModRevision(snapshotKey(r.Caller, src.Snapshot)), "=", from))
	}
	resp, err := s.kv.Txn(ctx).
		If(guards...).
		Then(clientv3.OpPut(key, encode(r)), clientv3.OpPut(id, r.Caller+"/"+r.Name), eventOp(r.Caller, ev)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return machines.ErrRaced
	}
	s.trim(ctx, r.Caller)
	return nil
}

func (s *Store) Update(ctx context.Context, r machines.Record, rev int64, ev machineapi.Event) error {
	key := machineKey(r.Caller, r.Name)
	resp, err := s.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).
		Then(clientv3.OpPut(key, encode(r)), eventOp(r.Caller, ev)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return machines.ErrRaced
	}
	s.trim(ctx, r.Caller)
	return nil
}

func (s *Store) Snapshot(ctx context.Context, caller, name string) (machines.Snapshot, int64, error) {
	resp, err := s.kv.Get(ctx, snapshotKey(caller, name))
	if err != nil {
		return machines.Snapshot{}, 0, err
	}
	if len(resp.Kvs) == 0 {
		return machines.Snapshot{}, 0, machines.ErrNotFound
	}
	var snap machines.Snapshot
	return snap, resp.Kvs[0].ModRevision, json.Unmarshal(resp.Kvs[0].Value, &snap)
}

func (s *Store) Snapshots(ctx context.Context, caller string) ([]machines.Snapshot, error) {
	resp, err := s.kv.Get(ctx, SnapshotPrefix+caller+"/", clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decode[machines.Snapshot](resp)
}

func (s *Store) CreateSnapshot(ctx context.Context, snap machines.Snapshot, machine int64) error {
	key := snapshotKey(snap.Caller, snap.Snapshot.Name)
	resp, err := s.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0), clientv3.Compare(clientv3.ModRevision(machineKey(snap.Caller, snap.Snapshot.Machine)), "=", machine)).
		Then(clientv3.OpPut(key, encode(snap))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return machines.ErrRaced
	}
	return nil
}

func (s *Store) DeleteSnapshot(ctx context.Context, caller, name string) error {
	resp, err := s.kv.Delete(ctx, snapshotKey(caller, name))
	if err != nil {
		return err
	}
	if resp.Deleted == 0 {
		return machines.ErrNotFound
	}
	return nil
}

func (s *Store) Events(ctx context.Context, caller string, after *int64) ([]machineapi.Event, int64, error) {
	opts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByModRevision, clientv3.SortAscend)}
	if after != nil {
		opts = append(opts, clientv3.WithMinModRev(*after+1))
	}
	resp, err := s.kv.Txn(ctx).Then(clientv3.OpGet(floorKey(caller)), clientv3.OpGet(eventsOf(caller), opts...)).Commit()
	if err != nil {
		return nil, 0, err
	}
	rev := resp.Header.Revision
	if after != nil {
		floor, err := floorOf(resp.Responses[0].GetResponseRange().Kvs)
		if err != nil {
			return nil, 0, err
		}
		switch {
		case *after > rev:
			return nil, 0, fmt.Errorf("%w: %d is after the log's revision %d", machines.ErrBadCursor, *after, rev)
		case *after < floor:
			return nil, 0, fmt.Errorf("%w: %d is before the oldest event kept, after %d", machines.ErrCursorExpired, *after, floor)
		}
	}
	kvs := resp.Responses[1].GetResponseRange().Kvs
	out := make([]machineapi.Event, 0, len(kvs))
	for _, kv := range kvs {
		var ev machineapi.Event
		if err := json.Unmarshal(kv.Value, &ev); err != nil {
			return nil, 0, fmt.Errorf("record %s: %w", kv.Key, err)
		}
		out = append(out, ev)
	}
	return out, rev, nil
}

func (s *Store) ClaimZone(ctx context.Context, zone string) (string, error) {
	if zone == "" {
		resp, err := s.kv.Get(ctx, ZoneKey)
		if err != nil || len(resp.Kvs) == 0 {
			return "", err
		}
		return string(resp.Kvs[0].Value), nil
	}
	resp, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.CreateRevision(ZoneKey), "=", 0)).Then(clientv3.OpPut(ZoneKey, zone)).Else(clientv3.OpGet(ZoneKey)).Commit()
	if err != nil {
		return "", err
	}
	if resp.Succeeded {
		return zone, nil
	}
	return string(resp.Responses[0].GetResponseRange().Kvs[0].Value), nil
}

// floorOf is the revision of the newest event the log dropped, 0 when it dropped none.
func floorOf(kvs []*mvccpb.KeyValue) (int64, error) {
	if len(kvs) == 0 {
		return 0, nil
	}
	return strconv.ParseInt(string(kvs[0].Value), 10, 64)
}

// trim drops the caller's oldest events beyond the count bound and those older than the age bound,
// recording the newest dropped revision as the log's floor in the same transaction. A trim that
// fails or races another is left to the next write, so it is no failure of the write it follows.
func (s *Store) trim(ctx context.Context, caller string) {
	prefix := eventsOf(caller)
	cutoff := fmt.Sprintf("%s%020d", prefix, max(s.now().Add(-maxEventAge).UnixNano(), 0))
	for {
		n, err := s.kv.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
		if err != nil {
			return
		}
		over := max(n.Count-int64(s.keepEvents), 0)
		aged, err := s.kv.Get(ctx, prefix, clientv3.WithRange(cutoff), clientv3.WithCountOnly())
		if err != nil {
			return
		}
		drop := min(max(over, aged.Count), trimBatch)
		if drop == 0 {
			return
		}
		old, err := s.kv.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithKeysOnly(),
			clientv3.WithSort(clientv3.SortByModRevision, clientv3.SortAscend), clientv3.WithLimit(drop))
		if err != nil || len(old.Kvs) == 0 {
			return
		}
		floor, err := s.kv.Get(ctx, floorKey(caller))
		if err != nil {
			return
		}
		cond := clientv3.Compare(clientv3.CreateRevision(floorKey(caller)), "=", 0)
		if len(floor.Kvs) > 0 {
			cond = clientv3.Compare(clientv3.ModRevision(floorKey(caller)), "=", floor.Kvs[0].ModRevision)
		}
		ops := make([]clientv3.Op, 0, len(old.Kvs)+1)
		newest := int64(0)
		for _, kv := range old.Kvs {
			ops = append(ops, clientv3.OpDelete(string(kv.Key)))
			newest = max(newest, kv.ModRevision)
		}
		ops = append(ops, clientv3.OpPut(floorKey(caller), strconv.FormatInt(newest, 10)))
		if resp, err := s.kv.Txn(ctx).If(cond).Then(ops...).Commit(); err != nil || !resp.Succeeded {
			return
		}
	}
}
