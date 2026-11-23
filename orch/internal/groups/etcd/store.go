// Package etcd keeps the groups and their members in orch's etcd, and writes the engine's events to
// the inventory's event log.
package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/groups/wire"
	"github.com/garamsh/naru/orch/internal/inventory"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
	"github.com/garamsh/naru/orch/internal/prefixes"
)

// Prefixes of the records. A group is kept under its name and a member under its ID.
const (
	groupPrefix  = "/naru/orch/groups/"
	memberPrefix = "/naru/orch/group-members/"
	// A Pod prefix is claimed under its network and its address, so a second claim of the same
	// prefix finds the key taken. The claim's value is the ID of the member that holds it.
	claimPrefix = "/naru/orch/pod-prefix-claims/"
)

// claimRetries bounds the reads of a network's claims that a concurrent claim made stale.
const claimRetries = 8

var errClaimMoved = errors.New("the claims of the network or the member changed")

// member is a stored member record. Revision is the key's modification revision, so it is not
// stored.
type member struct {
	ID             string    `json:"id"`
	Group          string    `json:"group"`
	Host           string    `json:"host"`
	MachineID      string    `json:"machineId"`
	TemplateHash   string    `json:"templateHash"`
	Network        string    `json:"network,omitempty"`
	PodPrefix      string    `json:"podPrefix,omitempty"`
	Phase          string    `json:"phase"`
	LastSeen       time.Time `json:"lastSeen,omitzero"`
	FenceDeadline  time.Time `json:"fenceDeadline,omitzero"`
	CreateFailures int       `json:"createFailures,omitempty"`
	RetryAt        time.Time `json:"retryAt,omitzero"`
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

// PutGroup writes g as one transaction that holds only while the stored group is still prev, or is
// still absent when prev is nil. It returns inventory.ErrConflict when another writer got there first.
func (s *Store) PutGroup(ctx context.Context, g groups.GroupSpec, prev *groups.GroupSpec) error {
	key := groupPrefix + g.Name
	resp, err := s.kv.Get(ctx, key)
	if err != nil {
		return err
	}
	var cond clientv3.Cmp
	switch {
	case len(resp.Kvs) == 0 && prev == nil:
		cond = clientv3.Compare(clientv3.CreateRevision(key), "=", 0)
	case len(resp.Kvs) == 0 || prev == nil:
		return inventory.ErrConflict
	default:
		cur, err := decodeGroup(resp.Kvs[0].Key, resp.Kvs[0].Value)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(cur, *prev) {
			return inventory.ErrConflict
		}
		cond = clientv3.Compare(clientv3.ModRevision(key), "=", resp.Kvs[0].ModRevision)
	}
	t, err := s.kv.Txn(ctx).If(cond).Then(clientv3.OpPut(key, encode(wire.API(g)))).Commit()
	if err != nil {
		return err
	}
	if !t.Succeeded {
		return inventory.ErrConflict
	}
	return nil
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

// DeleteGroup removes the group record and the records of its members in one transaction, so a
// group applied again under the name numbers its members from 1. It returns inventory.ErrConflict
// while a member is not gone, or when a member record changed between the read and the write.
func (s *Store) DeleteGroup(ctx context.Context, name string) error {
	key := groupPrefix + name
	resp, err := s.kv.Get(ctx, memberPrefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}
	cond := []clientv3.Cmp{
		clientv3.Compare(clientv3.CreateRevision(key), ">", 0),
		clientv3.Compare(clientv3.ModRevision(memberPrefix), "<", resp.Header.Revision+1).WithPrefix(),
	}
	ops := []clientv3.Op{clientv3.OpDelete(key)}
	for _, kv := range resp.Kvs {
		var r member
		if err := json.Unmarshal(kv.Value, &r); err != nil {
			return fmt.Errorf("record %s: %w", kv.Key, err)
		}
		if r.Group != name {
			continue
		}
		if groups.Phase(r.Phase) != groups.PhaseGone {
			return fmt.Errorf("%w: member %s is not gone", inventory.ErrConflict, r.ID)
		}
		ops = append(ops, clientv3.OpDelete(string(kv.Key)))
	}
	t, err := s.kv.Txn(ctx).If(cond...).Then(ops...).Commit()
	if err != nil {
		return err
	}
	if t.Succeeded {
		return nil
	}
	if _, err := s.GetGroup(ctx, name); err != nil {
		return err
	}
	return inventory.ErrConflict
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
			Network: r.Network, PodPrefix: r.PodPrefix, Phase: groups.Phase(r.Phase), LastSeen: r.LastSeen, FenceDeadline: r.FenceDeadline,
			CreateFailures: r.CreateFailures, RetryAt: r.RetryAt, Revision: kv.ModRevision,
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

func networkClaims(network string) string { return claimPrefix + network + "/" }

// claimFree reports whether podPrefix overlaps no claim of the network, and the revision it read at.
func (s *Store) claimFree(ctx context.Context, network, podPrefix string) (bool, int64, error) {
	want, err := netip.ParsePrefix(podPrefix)
	if err != nil {
		return false, 0, fmt.Errorf("pod prefix %q: %w", podPrefix, err)
	}
	resp, err := s.kv.Get(ctx, networkClaims(network), clientv3.WithPrefix())
	if err != nil {
		return false, 0, err
	}
	var held []netip.Prefix
	for _, kv := range resp.Kvs {
		p, err := netip.ParsePrefix(strings.ReplaceAll(strings.TrimPrefix(string(kv.Key), networkClaims(network)), "_", "/"))
		if err != nil {
			return false, 0, fmt.Errorf("claim %s: %w", kv.Key, err)
		}
		held = append(held, p)
	}
	return !prefixes.Overlaps(held, want), resp.Header.Revision, nil
}

// PutMember writes m when m.Revision is the stored revision, and returns the record with its new
// one. A Revision of 0 creates the record, and fails with groups.ErrConflict when it exists or when
// another member holds the Pod prefix m asks for: the record and the claim are written in one
// step. A claim that overlaps another of the network is a conflict. Writing a member that is gone ends its claim.
func (s *Store) PutMember(ctx context.Context, m groups.Member) (groups.Member, error) {
	// A new claim also loses when an unrelated claim of the network was written between its read
	// and its write; it is then read again.
	for range claimRetries {
		out, err := s.putMember(ctx, m)
		if !errors.Is(err, errClaimMoved) {
			return out, err
		}
	}
	return groups.Member{}, groups.ErrConflict
}

func (s *Store) putMember(ctx context.Context, m groups.Member) (groups.Member, error) {
	if m.ID == "" || strings.Contains(m.ID, "/") {
		return groups.Member{}, fmt.Errorf("member ID %q cannot be a key", m.ID)
	}
	key := memberPrefix + m.ID
	rec := member{
		ID: m.ID, Group: m.Group, Host: m.Host, MachineID: m.MachineID, TemplateHash: m.TemplateHash,
		Network: m.Network, PodPrefix: m.PodPrefix,
		Phase: string(m.Phase), LastSeen: m.LastSeen, FenceDeadline: m.FenceDeadline,
		CreateFailures: m.CreateFailures, RetryAt: m.RetryAt,
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
			// The claims are read at one revision and the write compares that none under the
			// network changed since, so two claims that overlap cannot both be written.
			free, rev, err := s.claimFree(ctx, m.Network, m.PodPrefix)
			if err != nil {
				return groups.Member{}, err
			}
			if !free {
				return groups.Member{}, groups.ErrConflict
			}
			cond = append(cond, clientv3.Compare(clientv3.ModRevision(networkClaims(m.Network)), "<", rev+1).WithPrefix())
			ops = append(ops, clientv3.OpPut(claim, m.ID))
		}
	}
	resp, err := s.kv.Txn(ctx).If(cond...).Then(ops...).Commit()
	if err != nil {
		return groups.Member{}, err
	}
	if !resp.Succeeded {
		if m.Revision == 0 && m.PodPrefix != "" && m.Phase != groups.PhaseGone {
			return groups.Member{}, errClaimMoved
		}
		return groups.Member{}, groups.ErrConflict
	}
	m.Revision = resp.Header.Revision
	return m, nil
}
