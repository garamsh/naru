package etcd_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	groupsetcd "github.com/garamsh/naru/orch/internal/groups/etcd"
	"github.com/garamsh/naru/orch/internal/inventory"
	invetcd "github.com/garamsh/naru/orch/internal/inventory/etcd"
	netetcd "github.com/garamsh/naru/orch/internal/networks/etcd"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func newStore(t *testing.T) (*groupsetcd.Store, *invetcd.Store) {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	return groupsetcd.New(c.Members[0].Etcd()), invetcd.New(c.Members[0].Etcd())
}

func spec(name string) groups.GroupSpec {
	return groups.GroupSpec{
		Name: name, Count: 3, Region: "kr-seoul",
		RequiredLabels:  map[string]string{"disk": "ssd"},
		PreferredLabels: []groups.LabelPreference{{Key: "rack", Value: "a", Weight: 5}},
		Template: groups.Template{
			Form: groups.FormImage, CPUs: 2, MemoryMiB: 256, Version: "r@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			Disks:   []groups.Disk{{Name: "data", SizeBytes: 1 << 30}},
			Network: "lab", PodPrefixBits: 26, RootDiskBytes: 8 << 30,
			Policy: groups.NetworkPolicy{Egress: []groups.Rule{{Prefix: "10.0.0.0/8", Protocol: "tcp", Ports: []int{443}}}},
		},
		Rollout: groups.Rollout{MaxUnavailable: 1, MaxSurge: 1}, MinAvailable: 2, Stateful: true, FenceTTL: 30 * time.Second,
	}
}

func TestGroupsRoundTripThroughTheStore(t *testing.T) {
	s, _ := newStore(t)
	_, err := s.GetGroup(t.Context(), "web")
	require.ErrorIs(t, err, inventory.ErrNotFound)

	require.NoError(t, s.PutGroup(t.Context(), spec("web")))
	require.NoError(t, s.PutGroup(t.Context(), spec("api")))
	got, err := s.GetGroup(t.Context(), "web")
	require.NoError(t, err)
	require.Equal(t, spec("web"), got)
	require.Equal(t, spec("web").Template.Hash(), got.Template.Hash())

	all, err := s.ListGroups(t.Context())
	require.NoError(t, err)
	require.Len(t, all, 2)
	require.Equal(t, "api", all[0].Name)

	require.NoError(t, s.DeleteGroup(t.Context(), "web"))
	require.ErrorIs(t, s.DeleteGroup(t.Context(), "web"), inventory.ErrNotFound)
}

func TestPutMemberIsACompareAndSwapOnTheRevision(t *testing.T) {
	s, _ := newStore(t)
	m := groups.Member{ID: "web-1", Group: "web", Host: "h1", MachineID: "web-1", TemplateHash: "abc", Phase: groups.PhasePending, LastSeen: time.Unix(100, 0).UTC()}

	created, err := s.PutMember(t.Context(), m)
	require.NoError(t, err)
	require.NotZero(t, created.Revision)

	// A second create of the same ID is a conflict, and so is a write at a stale revision.
	_, err = s.PutMember(t.Context(), m)
	require.ErrorIs(t, err, groups.ErrConflict)

	created.Phase = groups.PhaseGone
	updated, err := s.PutMember(t.Context(), created)
	require.NoError(t, err)
	require.Greater(t, updated.Revision, created.Revision)
	_, err = s.PutMember(t.Context(), created)
	require.ErrorIs(t, err, groups.ErrConflict)

	list, err := s.ListMembers(t.Context())
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, updated, list[0])
	require.Equal(t, groups.PhaseGone, list[0].Phase)
}

func TestOnlyOneOfTwoRacingWritersWins(t *testing.T) {
	s, _ := newStore(t)
	m, err := s.PutMember(t.Context(), groups.Member{ID: "web-1", Group: "web", Phase: groups.PhasePending})
	require.NoError(t, err)

	var wins, conflicts int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutMember(t.Context(), m)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if err == groups.ErrConflict {
				conflicts++
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, wins)
	require.Equal(t, 7, conflicts)
}

func TestEventsGoToTheInventoryLogAndARepeatedHoldIsWrittenOnce(t *testing.T) {
	_, inv := newStore(t)
	e := groupsetcd.NewEvents(inv)
	t0 := time.Unix(1000, 0).UTC()
	hold := groups.Event{Time: t0, Kind: groups.EventHold, Group: "web", Message: "no host has room"}

	require.NoError(t, e.Record(t.Context(), hold))
	hold.Time = t0.Add(10 * time.Second)
	require.NoError(t, e.Record(t.Context(), hold))
	hold.Time = t0.Add(11 * time.Minute)
	require.NoError(t, e.Record(t.Context(), hold))
	require.NoError(t, e.Record(t.Context(), groups.Event{Time: t0.Add(time.Second), Kind: groups.EventCreate, Group: "web", Member: "web-1", Host: "h1", Message: "create"}))

	evs, err := inv.ListEvents(t.Context())
	require.NoError(t, err)
	var types []string
	for _, ev := range evs {
		types = append(types, ev.Type)
	}
	require.Equal(t, []string{"group.hold", "group.create", "group.hold"}, types)
	require.Equal(t, orchapi.Event{Time: t0.Add(time.Second), Type: "group.create", Subject: "web-1", Detail: map[string]string{"message": "create", "group": "web", "member": "web-1", "host": "h1"}}, evs[1])
}

func podMember(id, network, prefix string) groups.Member {
	return groups.Member{ID: id, Group: "web", Network: network, PodPrefix: prefix, Phase: groups.PhasePending}
}

func TestAPodPrefixIsHeldByOneMemberOfANetworkUntilItIsGone(t *testing.T) {
	s, _ := newStore(t)
	ctx := t.Context()

	a, err := s.PutMember(ctx, podMember("web-1", "lab", "10.9.0.0/26"))
	require.NoError(t, err)
	got, err := s.ListMembers(ctx)
	require.NoError(t, err)
	require.Equal(t, "lab", got[0].Network)
	require.Equal(t, "10.9.0.0/26", got[0].PodPrefix)

	_, err = s.PutMember(ctx, podMember("web-2", "lab", "10.9.0.0/26"))
	require.ErrorIs(t, err, groups.ErrConflict, "the prefix is taken")
	_, err = s.PutMember(ctx, podMember("web-2", "lab", "10.9.0.64/26"))
	require.NoError(t, err, "control: another prefix of the network is free")
	_, err = s.PutMember(ctx, podMember("web-3", "other", "10.9.0.0/26"))
	require.NoError(t, err, "control: the same prefix on another network is free")

	// A write that keeps the member does not release the claim.
	a.LastSeen = time.Unix(200, 0).UTC()
	a, err = s.PutMember(ctx, a)
	require.NoError(t, err)
	_, err = s.PutMember(ctx, podMember("web-4", "lab", "10.9.0.0/26"))
	require.ErrorIs(t, err, groups.ErrConflict)

	a.Phase = groups.PhaseGone
	gone, err := s.PutMember(ctx, a)
	require.NoError(t, err)
	_, err = s.PutMember(ctx, podMember("web-4", "lab", "10.9.0.0/26"))
	require.NoError(t, err, "a gone member's prefix is free")

	// The gone member written again does not end the claim of the member that holds it now.
	gone.LastSeen = time.Unix(300, 0).UTC()
	_, err = s.PutMember(ctx, gone)
	require.NoError(t, err)
	_, err = s.PutMember(ctx, podMember("web-5", "lab", "10.9.0.0/26"))
	require.ErrorIs(t, err, groups.ErrConflict)
}

func TestOnlyOneOfManyMembersRacingForAPodPrefixGetsIt(t *testing.T) {
	s, _ := newStore(t)
	var wins, conflicts int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutMember(t.Context(), podMember(fmt.Sprintf("web-%d", i), "lab", "10.9.0.0/26"))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
			} else if errors.Is(err, groups.ErrConflict) {
				conflicts++
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 1, wins)
	require.Equal(t, 7, conflicts)
}

func TestListNetworksReadsTheNetworksOrchKeeps(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	s := groupsetcd.New(c.Members[0].Etcd())
	none, err := s.ListNetworks(t.Context())
	require.NoError(t, err)
	require.Empty(t, none)

	nets := netetcd.New(c.Members[0].Etcd())
	require.NoError(t, nets.Put(t.Context(), orchapi.Network{Name: "lab", Prefix: "10.9.0.0/24"}))
	require.NoError(t, nets.Put(t.Context(), orchapi.Network{Name: "a", Prefix: "10.8.0.0/24", APIEndpoint: "10.8.0.2"}))
	got, err := s.ListNetworks(t.Context())
	require.NoError(t, err)
	require.Equal(t, []groups.Network{{Name: "a", Prefix: "10.8.0.0/24"}, {Name: "lab", Prefix: "10.9.0.0/24"}}, got)
}
