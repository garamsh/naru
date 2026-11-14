// Package etcd keeps the groups and their members in orch's etcd, and writes the engine's events to
// the inventory's event log.
package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/groups/wire"
	"github.com/garamsh/naru/orch/internal/inventory"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
)

// Prefixes of the records. A group is kept under its name and a member under its ID.
const (
	groupPrefix  = "/naru/orch/groups/"
	memberPrefix = "/naru/orch/group-members/"
	// A Pod prefix is claimed under its network and its address, so a second claim of the same
	// prefix finds the key taken. The claim's value is the ID of the member that holds it.
	claimPrefix = "/naru/orch/pod-prefix-claims/"
)

// member is a stored member record. Revision is the key's modification revision, so it is not
// stored.
type member struct {
	ID            string    `json:"id"`
	Group         string    `json:"group"`
	Host          string    `json:"host"`
	MachineID     string    `json:"machineId"`
	TemplateHash  string    `json:"templateHash"`
	Network       string    `json:"network,omitempty"`
	PodPrefix     string    `json:"podPrefix,omitempty"`
	Phase         string    `json:"phase"`
	LastSeen      time.Time `json:"lastSeen,omitzero"`
	FenceDeadline time.Time `json:"fenceDeadline,omitzero"`
}

// Store is the groups and members over an etcd client.
type Store struct {
	kv clientv3.KV
}

var _ groups.Store = (*Store)(nil)

// New returns a store over kv.
func New(kv clientv3.KV) *Store { return &Store{kv: kv} }

func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("a stored record does not marshal: %v", err))
	}
	return string(b)
}

// PutGroup creates the group or replaces its spec.
func (s *Store) PutGroup(ctx context.Context, g groups.GroupSpec) error {
	_, err := s.kv.Put(ctx, groupPrefix+g.Name, encode(wire.API(g)))
	return err
}

// GetGroup returns the group, or inventory.ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, name string) (groups.GroupSpec, error) {
	resp, err := s.kv.Get(ctx, groupPrefix+name)
	if err != nil {
		return groups.GroupSpec{}, err
	}
	if len(resp.Kvs) == 0 {
		return groups.GroupSpec{}, inventory.ErrNotFound
	}
	return decodeGroup(resp.Kvs[0].Key, resp.Kvs[0].Value)
}

func decodeGroup(key, value []byte) (groups.GroupSpec, error) {
	var a orchapi.GroupSpec
	if err := json.Unmarshal(value, &a); err != nil {
		return groups.GroupSpec{}, fmt.Errorf("record %s: %w", key, err)
	}
	return wire.Spec(a), nil
}

// DeleteGroup removes the group record; the group's members are not touched.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	resp, err := s.kv.Delete(ctx, groupPrefix+name)
	if err != nil {
		return err
	}
	if resp.Deleted == 0 {
		return inventory.ErrNotFound
	}
	return nil
}

// ListGroups returns every group, in name order.
func (s *Store) ListGroups(ctx context.Context) ([]groups.GroupSpec, error) {
	resp, err := s.kv.Get(ctx, groupPrefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, err
	}
	out := make([]groups.GroupSpec, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		g, err := decodeGroup(kv.Key, kv.Value)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

// ListMembers returns every member record, gone ones included.
func (s *Store) ListMembers(ctx context.Context) ([]groups.Member, error) {
	resp, err := s.kv.Get(ctx, memberPrefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		return nil, err
	}
	out := make([]groups.Member, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		var r member
		if err := json.Unmarshal(kv.Value, &r); err != nil {
			return nil, fmt.Errorf("record %s: %w", kv.Key, err)
		}
		out = append(out, groups.Member{
			ID: r.ID, Group: r.Group, Host: r.Host, MachineID: r.MachineID, TemplateHash: r.TemplateHash,
			Network: r.Network, PodPrefix: r.PodPrefix, Phase: groups.Phase(r.Phase), LastSeen: r.LastSeen, FenceDeadline: r.FenceDeadline, Revision: kv.ModRevision,
		})
	}
	return out, nil
}

// ListNetworks returns the networks, in name order.
func (s *Store) ListNetworks(ctx context.Context) ([]groups.Network, error) {
	all, err := netetcd.New(s.kv).List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]groups.Network, len(all))
	for i, n := range all {
		out[i] = groups.Network{Name: n.Name, Prefix: n.Prefix}
	}
	return out, nil
}

// claimKey is where the claim of a Pod prefix of a network is kept.
func claimKey(network, podPrefix string) string {
	return claimPrefix + network + "/" + strings.ReplaceAll(podPrefix, "/", "_")
}

// PutMember writes m when m.Revision is the stored revision, and returns the record with its new
// one. A Revision of 0 creates the record, and fails with groups.ErrConflict when it exists or when
// another member holds the Pod prefix m asks for: the record and the claim are written in one
// step. Writing a member that is gone ends its claim.
func (s *Store) PutMember(ctx context.Context, m groups.Member) (groups.Member, error) {
	if m.ID == "" || strings.Contains(m.ID, "/") {
		return groups.Member{}, fmt.Errorf("member ID %q cannot be a key", m.ID)
	}
	key := memberPrefix + m.ID
	rec := member{
		ID: m.ID, Group: m.Group, Host: m.Host, MachineID: m.MachineID, TemplateHash: m.TemplateHash,
		Network: m.Network, PodPrefix: m.PodPrefix,
		Phase: string(m.Phase), LastSeen: m.LastSeen, FenceDeadline: m.FenceDeadline,
	}
	cond := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", m.Revision)}
	ops := []clientv3.Op{clientv3.OpPut(key, encode(rec))}
	if m.PodPrefix != "" {
		claim := claimKey(m.Network, m.PodPrefix)
		switch {
		case m.Phase == groups.PhaseGone:
			// Only the holder ends a claim: the prefix may be another member's by now.
			ops = append(ops, clientv3.OpTxn([]clientv3.Cmp{clientv3.Compare(clientv3.Value(claim), "=", m.ID)}, []clientv3.Op{clientv3.OpDelete(claim)}, nil))
		case m.Revision == 0:
			cond = append(cond, clientv3.Compare(clientv3.CreateRevision(claim), "=", 0))
			ops = append(ops, clientv3.OpPut(claim, m.ID))
		}
	}
	resp, err := s.kv.Txn(ctx).If(cond...).Then(ops...).Commit()
	if err != nil {
		return groups.Member{}, err
	}
	if !resp.Succeeded {
		return groups.Member{}, groups.ErrConflict
	}
	m.Revision = resp.Header.Revision
	return m, nil
}
