package member_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

func TestQuota_IsAShareOfTheDiskWithinItsBounds(t *testing.T) {
	tests := []struct {
		name string
		disk uint64
		want int64
	}{
		{"a disk of 8 GiB gives an eighth", 8 << 30, 1 << 30},
		{"aurora's 2 GiB disk gives 256 MiB, below etcd's 2 GiB default that filled it", 2 << 30, 256 << 20},
		{"a small disk is held to the floor", 100 << 20, member.MinQuota},
		{"a huge disk is held to the ceiling", 1 << 40, member.MaxQuota},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, member.Quota(tt.disk))
		})
	}
}

func TestDiskBytes_MeasuresTheNearestDirectoryThatExists(t *testing.T) {
	dir := t.TempDir()

	exists, err := member.DiskBytes(dir)
	require.NoError(t, err)
	below, err := member.DiskBytes(dir + "/orch/etcd")

	require.NoError(t, err)
	assert.NotZero(t, exists)
	assert.Equal(t, exists, below, "etcd's data directory does not exist before etcd makes it")
}

func TestShouldDefrag(t *testing.T) {
	const mib = 1 << 20
	tests := []struct {
		name        string
		size, inUse int64
		want        bool
	}{
		{"a large database that is mostly free pages", 400 * mib, 20 * mib, true},
		{"a large database that holds its data", 400 * mib, 300 * mib, false},
		{"a small database that is mostly free pages", 8 * mib, 1 * mib, false},
		{"a database that is exactly half free", 64 * mib, 32 * mib, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, member.ShouldDefrag(tt.size, tt.inUse))
		})
	}
}

func TestRemovable(t *testing.T) {
	assert.NoError(t, member.Removable(3, true))
	assert.ErrorContains(t, member.Removable(2, true), "3 or more", "two members cannot spare one")
	assert.ErrorContains(t, member.Removable(3, false), "quorum", "a second member out would lose the quorum")
}

func TestHousekeeper_DefragmentsADatabaseCompactionLeftMostlyFree(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	cl := c.Members[0].Etcd()
	value := strings.Repeat("x", 1<<20)
	var rev int64
	for range 48 { // 48 revisions of 1 MiB under one key: 48 MiB of history
		resp, err := cl.Put(t.Context(), "/churn", value)
		require.NoError(t, err)
		rev = resp.Header.Revision
	}
	_, err := cl.Compact(t.Context(), rev, clientv3.WithCompactPhysical())
	require.NoError(t, err)
	st, err := cl.Status(t.Context(), cl.Endpoints()[0])
	require.NoError(t, err)
	require.Greater(t, st.DbSize, int64(member.MinDefragBytes), "the history made a database worth defragmenting")
	require.Less(t, st.DbSizeInUse, st.DbSize/2, "compaction left most of it free")

	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}
	require.NoError(t, h.Pass(t.Context()))

	after, err := cl.Status(t.Context(), cl.Endpoints()[0])
	require.NoError(t, err)
	assert.Less(t, after.DbSize, st.DbSize/2, "the defragmentation returned the free pages")
}

func TestHousekeeper_RemovesAMemberThatStaysOutOnlyAfterTheDepartureTimeAndOnlyOne(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 3})
	lead := c.Leader(t, []int{0, 1, 2}, 30*time.Second)
	out := (lead + 1) % 3
	require.NoError(t, c.Members[out].Stop())
	cl := c.Members[lead].Etcd()
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}

	require.NoError(t, h.Pass(t.Context()))
	list, err := cl.MemberList(t.Context())
	require.NoError(t, err)
	require.Len(t, list.Members, 3, "a member that has just stopped is not removed")

	now = now.Add(time.Hour + time.Minute)
	require.NoError(t, h.Pass(t.Context()))

	list, err = cl.MemberList(t.Context())
	require.NoError(t, err)
	assert.Len(t, list.Members, 2, "a member out for the departure time is removed")
}

// fakeCluster answers MemberList with its members and Status for those whose client URL is not in down;
// it records the members removed.
type fakeCluster struct {
	member.Cluster
	members []*etcdserverpb.Member
	down    map[string]bool
	removed []uint64
}

func (f *fakeCluster) MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{Members: f.members}, nil
}

func (f *fakeCluster) Status(_ context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	if f.down[endpoint] {
		return nil, errors.New("connection refused")
	}
	return &clientv3.StatusResponse{DbSize: 1 << 20, DbSizeInUse: 1 << 20}, nil
}

func (f *fakeCluster) MemberRemove(_ context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	f.removed = append(f.removed, id)
	return &clientv3.MemberRemoveResponse{}, nil
}

func (f *fakeCluster) AlarmList(context.Context) (*clientv3.AlarmResponse, error) {
	return &clientv3.AlarmResponse{}, nil
}

func fakeMembers(n int, down ...int) *fakeCluster {
	f := &fakeCluster{down: map[string]bool{}}
	for i := range n {
		url := fmt.Sprintf("https://10.0.0.%d:2379", i+1)
		f.members = append(f.members, &etcdserverpb.Member{ID: uint64(i + 1), Name: fmt.Sprintf("m%d", i+1), ClientURLs: []string{url}})
	}
	for _, d := range down {
		f.down[f.members[d].ClientURLs[0]] = true
	}
	return f
}

func TestHousekeeper_RemovesAMemberOnlyFromThreeOrMoreAndOnlyWhenNoOtherMemberIsOut(t *testing.T) {
	tests := []struct {
		name    string
		members int
		down    []int
		removed []uint64
	}{
		{"one of three is out", 3, []int{2}, []uint64{3}},
		{"one of five is out", 5, []int{4}, []uint64{5}},
		{"one of two is out: the cluster is too small to spare it", 2, []int{1}, nil},
		{"two of three are out", 3, []int{1, 2}, nil},
		{"two of five are out, the quorum still holds but another member does not answer", 5, []int{3, 4}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fakeMembers(tt.members, tt.down...)
			now := time.Now()
			h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}
			require.NoError(t, h.Pass(t.Context()))
			require.Empty(t, cl.removed, "nothing is removed before the departure time")

			now = now.Add(2 * time.Hour)
			require.NoError(t, h.Pass(t.Context()))

			assert.Equal(t, tt.removed, cl.removed)
		})
	}
}

func TestDiscardIfRemoved_DeletesTheMemberOnlyOfAClusterThatNoLongerHasIt(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 3})
	cl := c.Members[0].Etcd()
	list, err := cl.MemberList(t.Context())
	require.NoError(t, err)
	files := c.CA.IssueFiles(t, t.TempDir(), "m9")
	dataDir := func() string {
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "member", "snap"), 0o755))
		return dir
	}
	cfg := func(dir, peer, join string) member.Config {
		return member.Config{Name: "m9", DataDir: dir, PeerURL: peer, Join: join, Cert: files.Cert, Key: files.Key, CA: files.CA}
	}

	removed := dataDir()
	require.NoError(t, member.DiscardIfRemoved(t.Context(), cfg(removed, "https://127.0.0.1:1", cl.Endpoints()[0])))
	assert.False(t, member.HasData(removed), "the cluster does not list the member, so its directory is deleted and the next start joins afresh")

	listed := dataDir()
	require.NoError(t, member.DiscardIfRemoved(t.Context(), cfg(listed, list.Members[1].PeerURLs[0], cl.Endpoints()[0])))
	assert.True(t, member.HasData(listed), "a member the cluster lists keeps its directory")

	unasked := dataDir()
	require.NoError(t, member.DiscardIfRemoved(t.Context(), cfg(unasked, "https://127.0.0.1:1", "https://127.0.0.1:1")))
	assert.True(t, member.HasData(unasked), "a cluster that cannot be asked leaves the directory alone")
}
