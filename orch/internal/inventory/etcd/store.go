// Package etcd keeps the inventory in orch's etcd under /naru/orch/.
package etcd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

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
	// allocPrefix holds one key per allocated Machine prefix, named by the prefix, so that creating
	// it is what makes an allocation unique. profileKey holds the host profile's YAML.
	allocPrefix = "/naru/orch/allocations/"
	profileKey  = "/naru/orch/profile"
)

// maxEvents is how many events the log keeps. Groups write one for each action they take, and
// a log kept forever grows for as long as orch runs and is read whole by anything that lists it;
// this holds about a week of a busy fleet's events at a few thousand a day, and older events
// are dropped, oldest first.
const maxEvents = 20000

// trimBatch is how many events one trim transaction deletes. etcd refuses a transaction of more
// than --max-txn-ops operations (128 by default), so a log far past its bound is trimmed in
// batches below that.
const trimBatch = 100

// casRetries bounds the retries of a read-modify-write that lost a race.
const casRetries = 8

// Store is the inventory over an etcd client.
type Store struct {
	kv clientv3.KV
	// keepEvents is how many events the log keeps; maxEvents unless a test says less.
	keepEvents int
}

var _ inventory.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv, keepEvents: maxEvents} }

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
		// Each attempt decodes into a fresh value: JSON leaves a field the document omits as it
		// was, so a reused value would carry the earlier attempt's field into the write.
		v = *new(T)
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

func (s *Store) PutPending(ctx context.Context, p inventory.Pending, ev *orchapi.Event, create bool) error {
	key := pendingPrefix + p.HostID
	pend := clientv3.Compare(clientv3.CreateRevision(key), ">", 0)
	if create {
		pend = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	}
	ops := []clientv3.Op{clientv3.OpPut(key, encode(p))}
	if ev != nil {
		ops = append(ops, eventOp(*ev))
	}
	resp, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.CreateRevision(hostPrefix+p.HostID), "=", 0), pend).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if resp.Succeeded {
		return nil
	}
	if _, err := s.GetHost(ctx, p.HostID); err == nil {
		return inventory.ErrAlreadyAdmitted
	} else if !errors.Is(err, inventory.ErrNotFound) {
		return err
	}
	return inventory.ErrConflict
}

func (s *Store) GetPending(ctx context.Context, hostID string) (inventory.Pending, error) {
	var p inventory.Pending
	_, err := s.get(ctx, pendingPrefix+hostID, &p)
	return p, err
}

func (s *Store) ListPending(ctx context.Context) ([]inventory.Pending, error) {
	return list[inventory.Pending](ctx, s, pendingPrefix)
}

// Reserve is inventory.Store's Reserve. The allocations are read at one revision and the write
// compares that none of them changed since, so two reservations cannot take the same or overlapping
// prefixes.
func (s *Store) Reserve(ctx context.Context, hostID string, pick func([]netip.Prefix) (netip.Prefix, error), until, now time.Time) (inventory.Allocation, error) {
	for range casRetries {
		resp, err := s.kv.Get(ctx, allocPrefix, clientv3.WithPrefix())
		if err != nil {
			return inventory.Allocation{}, err
		}
		var claimed []netip.Prefix
		var expired []string
		var own *inventory.Allocation
		for _, kv := range resp.Kvs {
			var a inventory.Allocation
			if err := json.Unmarshal(kv.Value, &a); err != nil {
				return inventory.Allocation{}, fmt.Errorf("record %s: %w", kv.Key, err)
			}
			switch {
			case a.HostID == hostID && !a.ReservedUntil.IsZero():
				own = &a
			case !a.ReservedUntil.IsZero() && !a.ReservedUntil.After(now):
				expired = append(expired, string(kv.Key))
			default:
				pf, err := netip.ParsePrefix(a.Prefix)
				if err != nil {
					return inventory.Allocation{}, fmt.Errorf("record %s: %w", kv.Key, err)
				}
				claimed = append(claimed, pf)
			}
		}
		var a inventory.Allocation
		if own != nil {
			a = *own
			a.ReservedUntil = until
		} else {
			pf, err := pick(claimed)
			if err != nil {
				return inventory.Allocation{}, err
			}
			a = inventory.Allocation{Prefix: pf.String(), HostID: hostID, ReservedUntil: until}
		}
		ops := []clientv3.Op{clientv3.OpPut(allocPrefix+a.Prefix, encode(a))}
		for _, k := range expired {
			// A key the new reservation overwrites is not deleted as well: a transaction may not
			// touch one key twice.
			if k != allocPrefix+a.Prefix {
				ops = append(ops, clientv3.OpDelete(k))
			}
		}
		t, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(allocPrefix), "<", resp.Header.Revision+1).WithPrefix()).Then(ops...).Commit()
		if err != nil {
			return inventory.Allocation{}, err
		}
		if t.Succeeded {
			return a, nil
		}
	}
	return inventory.Allocation{}, inventory.ErrConflict
}

func (s *Store) Release(ctx context.Context, a inventory.Allocation) error {
	key := allocPrefix + a.Prefix
	_, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.Value(key), "=", encode(a))).Then(clientv3.OpDelete(key)).Commit()
	return err
}

func (s *Store) Admit(ctx context.Context, p inventory.Pending, h inventory.Host, ev orchapi.Event, alloc *inventory.Allocation) error {
	key := pendingPrefix + p.HostID
	cmps := []clientv3.Cmp{
		clientv3.Compare(clientv3.CreateRevision(key), ">", 0),
		clientv3.Compare(clientv3.CreateRevision(hostPrefix+h.ID), "=", 0),
	}
	ops := []clientv3.Op{clientv3.OpDelete(key), clientv3.OpPut(hostPrefix+h.ID, encode(h)), eventOp(ev)}
	if alloc != nil {
		confirmed := *alloc
		confirmed.ReservedUntil = time.Time{}
		cmps = append(cmps, clientv3.Compare(clientv3.Value(allocPrefix+alloc.Prefix), "=", encode(*alloc)))
		ops = append(ops, clientv3.OpPut(allocPrefix+alloc.Prefix, encode(confirmed)))
	}
	resp, err := s.kv.Txn(ctx).If(cmps...).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if resp.Succeeded {
		return nil
	}
	if _, err := s.GetPending(ctx, p.HostID); err != nil {
		return err
	}
	if _, err := s.GetHost(ctx, h.ID); err == nil {
		return inventory.ErrAlreadyAdmitted
	}
	return inventory.ErrAllocationTaken
}

// DropStoredConfigs is inventory.Store's DropStoredConfigs, one compare-guarded write per record.
func (s *Store) DropStoredConfigs(ctx context.Context) (int, error) {
	resp, err := s.kv.Get(ctx, hostPrefix, clientv3.WithPrefix())
	if err != nil {
		return 0, err
	}
	n := 0
	for _, kv := range resp.Kvs {
		var probe struct {
			Config []byte `json:"config"`
		}
		if err := json.Unmarshal(kv.Value, &probe); err != nil {
			return n, fmt.Errorf("record %s: %w", kv.Key, err)
		}
		if len(probe.Config) == 0 {
			continue
		}
		key := string(kv.Key)
		wrote := false
		_, err := update(ctx, s, key, func(h *inventory.Host) error {
			wrote = len(h.Config) > 0
			h.Config = nil
			return nil
		})
		if err != nil {
			return n, err
		}
		if wrote {
			n++
		}
	}
	return n, nil
}

func (s *Store) RemoveHost(ctx context.Context, id string, ev orchapi.Event) (inventory.Host, error) {
	key := hostPrefix + id
	var h inventory.Host
	for range casRetries {
		h = inventory.Host{}
		rev, err := s.get(ctx, key, &h)
		if err != nil {
			return h, err
		}
		ops := []clientv3.Op{clientv3.OpDelete(key), eventOp(ev)}
		if h.MachinePrefix != "" {
			ops = append(ops, clientv3.OpDelete(allocPrefix+h.MachinePrefix))
		}
		resp, err := s.kv.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision(key), "=", rev)).Then(ops...).Commit()
		if err != nil {
			return h, err
		}
		if resp.Succeeded {
			return h, nil
		}
	}
	return h, errors.New("the record kept changing under the removal")
}

func (s *Store) ListAllocations(ctx context.Context) ([]inventory.Allocation, error) {
	return list[inventory.Allocation](ctx, s, allocPrefix)
}

func (s *Store) GetProfile(ctx context.Context) ([]byte, error) {
	resp, err := s.kv.Get(ctx, profileKey)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, inventory.ErrNotFound
	}
	return resp.Kvs[0].Value, nil
}

func (s *Store) PutProfile(ctx context.Context, yaml []byte, ev orchapi.Event) error {
	_, err := s.kv.Txn(ctx).Then(clientv3.OpPut(profileKey, string(yaml)), eventOp(ev)).Commit()
	return err
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
	return update(ctx, s, hostPrefix+id, func(h *inventory.Host) error {
		change(h)
		// A write never carries the config older records held, so a heartbeat cannot bring it back.
		h.Config = nil
		return nil
	}, ops...)
}

func (s *Store) AppendEvent(ctx context.Context, ev orchapi.Event) error {
	if _, err := s.kv.Do(ctx, eventOp(ev)); err != nil {
		return err
	}
	return s.trimEvents(ctx)
}

// trimEvents drops the oldest events beyond the bound. The log is bounded here, on the appends
// that make it grow fastest; a failed trim is tried again by the next append, so it is not an
// error of the append that was already kept.
func (s *Store) trimEvents(ctx context.Context) error {
	for {
		n, err := s.kv.Get(ctx, eventPrefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
		if err != nil || n.Count <= int64(s.keepEvents) {
			return nil
		}
		old, err := s.kv.Get(ctx, eventPrefix, clientv3.WithPrefix(), clientv3.WithKeysOnly(),
			clientv3.WithSort(clientv3.SortByModRevision, clientv3.SortAscend), clientv3.WithLimit(min(n.Count-int64(s.keepEvents), trimBatch)))
		if err != nil || len(old.Kvs) == 0 {
			return nil
		}
		ops := make([]clientv3.Op, 0, len(old.Kvs))
		for _, kv := range old.Kvs {
			ops = append(ops, clientv3.OpDelete(string(kv.Key)))
		}
		if _, err := s.kv.Txn(ctx).Then(ops...).Commit(); err != nil {
			return nil
		}
	}
}

// ListEvents lists the events in the order etcd committed them. An event's Cursor is the
// revision that committed it: unlike the time in its key, it only grows, whichever orch member
// wrote it, so no event is committed behind a cursor a reader already holds.
func (s *Store) ListEvents(ctx context.Context, after string) ([]orchapi.Event, error) {
	opts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByModRevision, clientv3.SortAscend)}
	if after != "" {
		rev, err := strconv.ParseInt(after, 10, 64)
		if err != nil || rev < 0 {
			return nil, fmt.Errorf("%w: cursor %q is not an event cursor", inventory.ErrInvalid, after)
		}
		opts = append(opts, clientv3.WithMinModRev(rev+1))
	}
	resp, err := s.kv.Get(ctx, eventPrefix, opts...)
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Event, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var ev orchapi.Event
		if err := json.Unmarshal(kv.Value, &ev); err != nil {
			return nil, fmt.Errorf("record %s: %w", kv.Key, err)
		}
		ev.Cursor = strconv.FormatInt(kv.ModRevision, 10)
		out = append(out, ev)
	}
	return out, nil
}
