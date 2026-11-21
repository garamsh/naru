package member_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
