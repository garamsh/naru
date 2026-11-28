package etcd

import (
	"context"
	"encoding/json"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/machines"
)

// The keys of the aliases and of the operator's grants to callers.
const (
	// AliasPrefix is where each alias is kept, by hostname, so a hostname is held once in the fleet.
	AliasPrefix = "/naru/orch/aliases/"
	// CallerPrefix is where each caller's grant is kept, by its name.
	CallerPrefix = "/naru/orch/callers/"
)

func aliasKey(hostname string) string { return AliasPrefix + hostname }

func (s *Store) MarkDeleting(ctx context.Context, r machines.Record, rev int64, aliases []machines.AliasEntry, ev machineapi.Event) error {
	key := machineKey(r.Caller, r.Name)
	cmps := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", rev)}
	ops := []clientv3.Op{clientv3.OpPut(key, encode(r)), eventOp(r.Caller, ev)}
	for _, a := range aliases {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(aliasKey(a.Hostname)), "=", a.Rev))
		ops = append(ops, clientv3.OpDelete(aliasKey(a.Hostname)))
	}
	if _, err := s.guarded(ctx, cmps, ops...); err != nil {
		return err
	}
	s.trim(ctx, r.Caller)
	return nil
}

func (s *Store) Alias(ctx context.Context, hostname string) (machines.Alias, int64, error) {
	resp, err := s.kv.Get(ctx, aliasKey(hostname))
	if err != nil {
		return machines.Alias{}, 0, err
	}
	if len(resp.Kvs) == 0 {
		return machines.Alias{}, 0, machines.ErrNotFound
	}
	var a machines.Alias
	// Decoded first: the operands of one return have no order against the call among them.
	if err := json.Unmarshal(resp.Kvs[0].Value, &a); err != nil {
		return machines.Alias{}, 0, err
	}
	return a, resp.Kvs[0].ModRevision, nil
}

func (s *Store) Aliases(ctx context.Context) ([]machines.AliasEntry, error) {
	resp, err := s.kv.Get(ctx, AliasPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	as, err := decode[machines.Alias](resp)
	if err != nil {
		return nil, err
	}
	out := make([]machines.AliasEntry, len(as))
	for i, a := range as {
		out[i] = machines.AliasEntry{Alias: a, Rev: resp.Kvs[i].ModRevision}
	}
	return out, nil
}

func (s *Store) CreateAlias(ctx context.Context, a machines.Alias, m machines.Record, machine int64) error {
	key := machineKey(m.Caller, m.Name)
	_, err := s.guarded(ctx,
		[]clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(aliasKey(a.Hostname)), "=", 0), clientv3.Compare(clientv3.ModRevision(key), "=", machine)},
		clientv3.OpPut(aliasKey(a.Hostname), encode(a)), clientv3.OpPut(key, encode(m)))
	return err
}

func (s *Store) DeleteAlias(ctx context.Context, a machines.Alias, rev int64) error {
	_, err := s.guarded(ctx, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(aliasKey(a.Hostname)), "=", rev)}, clientv3.OpDelete(aliasKey(a.Hostname)))
	return err
}

func (s *Store) Caller(ctx context.Context, name string) (machines.Caller, error) {
	resp, err := s.kv.Get(ctx, CallerPrefix+name)
	if err != nil {
		return machines.Caller{}, err
	}
	c := machines.Caller{Name: name}
	if len(resp.Kvs) == 0 {
		return c, nil
	}
	return c, json.Unmarshal(resp.Kvs[0].Value, &c)
}

func (s *Store) Callers(ctx context.Context) ([]machines.Caller, error) {
	resp, err := s.kv.Get(ctx, CallerPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	return decode[machines.Caller](resp)
}

func (s *Store) PutCaller(ctx context.Context, c machines.Caller) error {
	_, err := s.kv.Put(ctx, CallerPrefix+c.Name, encode(c))
	return err
}

// Routing reads every Machine and every alias in one transaction, so at one revision of the store,
// which it returns: what the routing table of that revision is made from.
func (s *Store) Routing(ctx context.Context) ([]machines.Record, []machines.Alias, int64, error) {
	resp, err := s.kv.Txn(ctx).Then(
		clientv3.OpGet(MachinePrefix, clientv3.WithPrefix()),
		clientv3.OpGet(AliasPrefix, clientv3.WithPrefix()),
	).Commit()
	if err != nil {
		return nil, nil, 0, err
	}
	recs, err := decode[machines.Record]((*clientv3.GetResponse)(resp.Responses[0].GetResponseRange()))
	if err != nil {
		return nil, nil, 0, err
	}
	aliases, err := decode[machines.Alias]((*clientv3.GetResponse)(resp.Responses[1].GetResponseRange()))
	if err != nil {
		return nil, nil, 0, err
	}
	return recs, aliases, resp.Header.Revision, nil
}
