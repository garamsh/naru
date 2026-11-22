package member_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

func TestQuota_IsAShareOfTheDiskWithinItsBoundsAndRefusesADiskTooSmallForIt(t *testing.T) {
	tests := []struct {
		name string
		disk uint64
		want int64
		fail bool
	}{
		{"a disk of 8 GiB gives an eighth", 8 << 30, 1 << 30, false},
		{"a disk of 2 GiB gives 256 MiB, below etcd's 2 GiB default that filled it", 2 << 30, 256 << 20, false},
		{"the smallest disk gives the smallest quota", member.MinDisk, member.MinQuota, false},
		{"a huge disk is held to the ceiling", 1 << 40, member.MaxQuota, false},
		{"a disk under the smallest is refused", member.MinDisk - 1, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := member.Quota(tt.disk)
			if tt.fail {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestBackendQuota_TakesTheOverrideBeforeTheDisk(t *testing.T) {
	got, err := member.Config{DataDir: t.TempDir(), QuotaBytes: 5 << 20}.BackendQuota()

	require.NoError(t, err)
	assert.Equal(t, int64(5<<20), got, "an override below the disk-derived floor is used as it is")
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
	const quota = 512 * mib // a share of 64 MiB
	tests := []struct {
		name        string
		size, inUse int64
		want        bool
	}{
		{"a large database that is mostly free pages", 400 * mib, 20 * mib, true},
		{"a large database that holds its data", 400 * mib, 300 * mib, false},
		{"a database under a share of the quota", 63 * mib, 1 * mib, false},
		{"a database at a share of the quota and exactly half free", 64 * mib, 32 * mib, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, member.ShouldDefrag(quota, tt.size, tt.inUse))
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
	const quota = 256 << 20
	require.Greater(t, st.DbSize, int64(quota/member.DefragShare), "the history made a database worth defragmenting")
	require.Less(t, st.DbSizeInUse, st.DbSize/2, "compaction left most of it free")

	h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}
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
// it records the members removed, defragmented and disarmed.
type fakeCluster struct {
	member.Cluster
	members []*etcdserverpb.Member
	down    map[string]bool
	// dbSize and inUse are what Status reports; 1 MiB each when zero.
	dbSize, inUse int64
	alarms        []*etcdserverpb.AlarmMember
	// failDefrag names the endpoints whose defragmentation fails.
	failDefrag map[string]bool
	removed    []uint64
	defragged  []string
	disarmed   []uint64
}

func (f *fakeCluster) MemberList(context.Context, ...clientv3.OpOption) (*clientv3.MemberListResponse, error) {
	return &clientv3.MemberListResponse{Members: f.members}, nil
}

func (f *fakeCluster) Status(_ context.Context, endpoint string) (*clientv3.StatusResponse, error) {
	if f.down[endpoint] {
		return nil, errors.New("connection refused")
	}
	size, inUse := f.dbSize, f.inUse
	if size == 0 {
		size, inUse = 1<<20, 1<<20
	}
	return &clientv3.StatusResponse{DbSize: size, DbSizeInUse: inUse}, nil
}

func (f *fakeCluster) MemberRemove(_ context.Context, id uint64) (*clientv3.MemberRemoveResponse, error) {
	f.removed = append(f.removed, id)
	return &clientv3.MemberRemoveResponse{}, nil
}

func (f *fakeCluster) Defragment(_ context.Context, endpoint string) (*clientv3.DefragmentResponse, error) {
	f.defragged = append(f.defragged, endpoint)
	if f.failDefrag[endpoint] {
		return nil, errors.New("defragment failed")
	}
	return &clientv3.DefragmentResponse{}, nil
}

func (f *fakeCluster) AlarmList(context.Context) (*clientv3.AlarmResponse, error) {
	return &clientv3.AlarmResponse{Alarms: f.alarms}, nil
}

func (f *fakeCluster) AlarmDisarm(_ context.Context, m *clientv3.AlarmMember) (*clientv3.AlarmResponse, error) {
	f.disarmed = append(f.disarmed, m.MemberID)
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

func TestHousekeeper_DisarmsTheNoSpaceAlarmOfAMemberOnlyOnceItsDatabaseIsWellUnderTheQuota(t *testing.T) {
	const quota = 512 << 20
	tests := []struct {
		name   string
		dbSize int64
		down   []int
		want   []uint64
	}{
		{"well under half the quota", quota/member.DisarmShare - 1, nil, []uint64{1}},
		{"at half the quota the alarm would return at the next write", quota / member.DisarmShare, nil, nil},
		{"over the quota", quota, nil, nil},
		{"well under half the quota while another member is out", quota/member.DisarmShare - 1, []int{2}, []uint64{1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fakeMembers(3, tt.down...)
			cl.dbSize, cl.inUse = tt.dbSize, tt.dbSize
			cl.alarms = []*etcdserverpb.AlarmMember{{MemberID: 1, Alarm: etcdserverpb.AlarmType_NOSPACE}}
			h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}

			require.NoError(t, h.Pass(t.Context()))

			assert.Equal(t, tt.want, cl.disarmed)
		})
	}
}

func TestHousekeeper_DefragmentsNoMemberWhileOneIsOutAndTriesTheRestAfterOneFails(t *testing.T) {
	const quota = 512 << 20
	cl := fakeMembers(3)
	cl.dbSize, cl.inUse = 100<<20, 1<<20
	cl.failDefrag = map[string]bool{cl.members[0].ClientURLs[0]: true}
	h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}

	err := h.Pass(t.Context())

	require.ErrorContains(t, err, "defragment m1")
	assert.Len(t, cl.defragged, 3, "a member whose defragmentation fails does not keep the others from theirs")

	out := fakeMembers(3, 2)
	out.dbSize, out.inUse = 100<<20, 1<<20
	h = &member.Housekeeper{Client: out, Quota: quota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}
	require.NoError(t, h.Pass(t.Context()))
	assert.Empty(t, out.defragged, "a defragmentation blocks its member, so none runs while another is out")
}

func TestHousekeeper_RemovesNothingWhenItsFenceFails(t *testing.T) {
	cl := fakeMembers(3, 2)
	now := time.Now()
	fenced := errors.New("the term has ended")
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler),
		Fence: func(context.Context) error { return fenced }}
	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(2 * time.Hour)

	err := h.Pass(t.Context())

	require.ErrorIs(t, err, fenced)
	assert.Empty(t, cl.removed, "a leader that lost its term removes no member")
}

func TestHousekeeper_ReportsOnceAndLogsAnErrorWhenTheLiveDataIsOverTheLineTheAlarmIsDisarmedUnder(t *testing.T) {
	const quota = 512 << 20
	tests := []struct {
		name   string
		inUse  int64
		alarm  bool
		report int
	}{
		{"live data over half the quota with the alarm armed", quota / member.DisarmShare, true, 1},
		{"live data over half the quota before the alarm is armed", quota / member.DisarmShare, false, 1},
		{"live data just under half the quota", quota/member.DisarmShare - 1, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fakeMembers(3)
			cl.dbSize, cl.inUse = quota, tt.inUse
			if tt.alarm {
				cl.alarms = []*etcdserverpb.AlarmMember{{MemberID: 1, Alarm: etcdserverpb.AlarmType_NOSPACE}}
			}
			var logged strings.Builder
			var reports []string
			h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.NewTextHandler(&logged, nil)),
				Report: func(_ context.Context, name string, detail map[string]string) error {
					reports = append(reports, name+" "+detail["inUseBytes"])
					return nil
				}}

			require.NoError(t, h.Pass(t.Context()))
			require.NoError(t, h.Pass(t.Context()))

			assert.Len(t, reports, 3*tt.report, "once for each of the three members, not once per pass")
			assert.Equal(t, tt.report > 0, strings.Contains(logged.String(), "level=ERROR"))
			assert.Empty(t, cl.disarmed, "the live data is over the line, so the alarm stays")
		})
	}
}

func TestHousekeeper_ARefusedReportIsLoggedAndTriedAtTheNextPass(t *testing.T) {
	const quota = 512 << 20
	cl := fakeMembers(3)
	cl.dbSize, cl.inUse = quota, quota
	calls := 0
	var logged strings.Builder
	h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.NewTextHandler(&logged, nil)),
		Report: func(context.Context, string, map[string]string) error {
			calls++
			return errors.New("etcdserver: no space")
		}}

	require.NoError(t, h.Pass(t.Context()))
	require.NoError(t, h.Pass(t.Context()))

	assert.Equal(t, 6, calls, "a record that failed is tried again")
	assert.Contains(t, logged.String(), "the quota event was not recorded")
}

func TestHousekeeper_KeepsAMemberWithNoClientURLsInsideTheDepartureTime(t *testing.T) {
	cl := fakeMembers(3)
	// A fourth member that was just added but has not started: no client URLs.
	cl.members = append(cl.members, &etcdserverpb.Member{ID: 10, Name: "m4"})
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}

	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(59 * time.Minute)
	require.NoError(t, h.Pass(t.Context()))

	assert.Empty(t, cl.removed, "a member added and not yet started is given the departure time to start")
}

func TestHousekeeper_RemovesAMemberThatStaysUnstartedPastTheDepartureTime(t *testing.T) {
	cl := fakeMembers(3)
	cl.members = append(cl.members, &etcdserverpb.Member{ID: 10, Name: "m4"})
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}

	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(2 * time.Hour)
	require.NoError(t, h.Pass(t.Context()))

	assert.Equal(t, []uint64{10}, cl.removed, "a voter that never starts counts toward the quorum, so it is removed")
}

func TestHousekeeper_RemovesAnUnstartedMemberFromAClusterTooSmallToSpareAnOutOne(t *testing.T) {
	cl := fakeMembers(1)
	cl.members = append(cl.members, &etcdserverpb.Member{ID: 10, Name: "m2"})
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}

	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(2 * time.Hour)
	require.NoError(t, h.Pass(t.Context()))

	assert.Equal(t, []uint64{10}, cl.removed, "removing it lowers the quorum, so the minimum cluster size does not apply")
}

func TestHousekeeper_DoesNotRemoveAnUnstartedLearner(t *testing.T) {
	cl := fakeMembers(3)
	cl.members = append(cl.members, &etcdserverpb.Member{ID: 10, Name: "m4", IsLearner: true})
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}

	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(2 * time.Hour)
	require.NoError(t, h.Pass(t.Context()))

	assert.Empty(t, cl.removed, "a learner is no voter, so it costs the quorum nothing")
}

func TestHousekeeper_ANotYetStartedMemberInsideItsWindowDoesNotBlockRemovalOfADepartedOne(t *testing.T) {
	cl := fakeMembers(3, 2) // m3 is out from the first pass
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.DiscardHandler)}
	require.NoError(t, h.Pass(t.Context()))

	// A fourth member is added 40 minutes in and has not started when m3 has been out for an hour.
	now = now.Add(40 * time.Minute)
	cl.members = append(cl.members, &etcdserverpb.Member{ID: 10, Name: "m4"})
	require.NoError(t, h.Pass(t.Context()))
	require.Empty(t, cl.removed, "m3 has not been out for the departure time yet")
	now = now.Add(21 * time.Minute)
	require.NoError(t, h.Pass(t.Context()))

	assert.Equal(t, []uint64{3}, cl.removed, "the unstarted member is inside its own window, so it is not a second member out")
}

func TestHousekeeper_DefragmentsAMemberWhoseAlarmItsFreePagesKeepArmed(t *testing.T) {
	const quota = 512 << 20
	line := int64(quota / member.DisarmShare)
	tests := []struct {
		name          string
		dbSize, inUse int64
		alarm         bool
		defragged     bool
	}{
		{"armed, over the line but its data under it, with few free pages", line + 10<<20, line - 10<<20, true, true},
		{"the same database with no alarm armed", line + 10<<20, line - 10<<20, false, false},
		{"armed, and its data over the line, which a defragmentation cannot free", line + 10<<20, line + 5<<20, true, false},
		{"armed, and the database already under the line", line - 1, line - 1, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fakeMembers(3)
			cl.dbSize, cl.inUse = tt.dbSize, tt.inUse
			if tt.alarm {
				cl.alarms = []*etcdserverpb.AlarmMember{{MemberID: 1, Alarm: etcdserverpb.AlarmType_NOSPACE}}
			}
			require.False(t, member.ShouldDefrag(quota, tt.dbSize, tt.inUse), "ShouldDefrag alone would not defragment it")
			h := &member.Housekeeper{Client: cl, Quota: quota, Now: time.Now, Log: slog.New(slog.DiscardHandler)}

			require.NoError(t, h.Pass(t.Context()))

			assert.Equal(t, tt.defragged, len(cl.defragged) > 0)
		})
	}
}
