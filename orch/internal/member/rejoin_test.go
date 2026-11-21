package member_test

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/orch/internal/member"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

var quiet = slog.New(slog.DiscardHandler)

// grownFromOne is a three-member cluster whose member 0 started alone, with its own peer URL as its
// initial cluster, and whose members 1 and 2 joined it with --join.
func grownFromOne(t *testing.T) *orchtest.Cluster {
	t.Helper()
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	c.AddMember(t)
	c.AddMember(t)
	return c
}

// removeStopped stops member out and runs the housekeeper of member via until it has removed it.
func removeStopped(t *testing.T, c *orchtest.Cluster, out, via int) {
	t.Helper()
	require.NoError(t, c.Members[out].Stop())
	cl := c.Members[via].Etcd()
	now := time.Now()
	h := &member.Housekeeper{Client: cl, Quota: member.MaxQuota, Departure: time.Hour, Now: func() time.Time { return now }, Log: quiet}
	require.NoError(t, h.Pass(t.Context()))
	now = now.Add(2 * time.Hour)
	require.NoError(t, h.Pass(t.Context()))
	gone, err := cl.MemberList(t.Context())
	require.NoError(t, err)
	require.Len(t, gone.Members, 2, "the housekeeper removed the member")
}

// idsOf is the IDs of the members the cluster lists through member via.
func idsOf(t *testing.T, c *orchtest.Cluster, via int) map[uint64]bool {
	t.Helper()
	list, err := c.Members[via].Etcd().MemberList(t.Context())
	require.NoError(t, err)
	ids := map[uint64]bool{}
	for _, m := range list.Members {
		ids[m.ID] = true
	}
	return ids
}

// assertRejoinedAsNew asserts that the cluster lists three members of which exactly one is not in
// before.
func assertRejoinedAsNew(t *testing.T, c *orchtest.Cluster, via int, before map[uint64]bool) {
	t.Helper()
	after := idsOf(t, c, via)
	assert.Len(t, after, 3, "the removed member joined the cluster again")
	fresh := 0
	for id := range after {
		if !before[id] {
			fresh++
		}
	}
	assert.Equal(t, 1, fresh, "it came back as a new member")
}

func markerOf(c member.Config) string { return c.DataDir + ".rejoin" }

func TestStart_RejoinsAFirstMemberThatStartedAloneWhenTheHousekeeperRemovedIt(t *testing.T) {
	c := grownFromOne(t)
	before := idsOf(t, c, 1)
	cfg := c.MemberConfig(0)
	require.Empty(t, cfg.Join, "member 0 has no join flag")
	require.Equal(t, "m0="+cfg.PeerURL, cfg.InitialCluster, "member 0's initial cluster is itself alone")
	removeStopped(t, c, 0, 1)

	c.Restart(t, 0)

	assertRejoinedAsNew(t, c, 1, before)
	assert.NoFileExists(t, markerOf(cfg), "the marker goes once the member runs")
}

func TestStart_RejoinsAStaticMemberTheHousekeeperRemoved(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 3})
	lead := c.Leader(t, []int{0, 1, 2}, 30*time.Second)
	out := (lead + 1) % 3
	before := idsOf(t, c, lead)
	removeStopped(t, c, out, lead)

	c.Restart(t, out)

	assertRejoinedAsNew(t, c, lead, before)
}

func TestStart_RejoinsAfterACrashBetweenTheDiscardAndTheJoin(t *testing.T) {
	c := grownFromOne(t)
	before := idsOf(t, c, 1)
	cfg := c.MemberConfig(0)
	removeStopped(t, c, 0, 1)
	peers, err := member.RejoinPeers(t.Context(), cfg, quiet)
	require.NoError(t, err)
	require.NotEmpty(t, peers, "the member found that the cluster removed it")
	require.False(t, member.HasData(cfg.DataDir), "the process died after the discard")
	require.FileExists(t, markerOf(cfg))

	c.Restart(t, 0)

	assertRejoinedAsNew(t, c, 1, before)
}

func TestStart_JoinsThroughTheNextPeerWhenTheFirstDoesNotAnswer(t *testing.T) {
	c := grownFromOne(t)
	before := idsOf(t, c, 1)
	cfg := c.MemberConfig(0)
	removeStopped(t, c, 0, 1)
	peers, err := member.RejoinPeers(t.Context(), cfg, quiet)
	require.NoError(t, err)
	writeMarker(t, cfg, append([]string{"https://127.0.0.1:1"}, peers...)...)

	c.Restart(t, 0)

	assertRejoinedAsNew(t, c, 1, before)
}

func TestRejoinPeers_AsksThePeersOfItsOwnDataAndDiscardsOnlyWhenTheClusterDoesNotListIt(t *testing.T) {
	tests := []struct {
		name  string
		start func(*testing.T) *orchtest.Cluster
		out   int
	}{
		{"a first member that started alone", grownFromOne, 0},
		{"a member of a static initial cluster", func(t *testing.T) *orchtest.Cluster { return orchtest.Start(t, orchtest.Options{Members: 3}) }, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := tt.start(t)
			require.NoError(t, c.Members[tt.out].Stop())
			cfg := c.MemberConfig(tt.out)

			peers, err := member.RejoinPeers(t.Context(), cfg, quiet)
			require.NoError(t, err)
			assert.Empty(t, peers, "the cluster still lists the member")
			assert.True(t, member.HasData(cfg.DataDir))
			assert.NoFileExists(t, markerOf(cfg))

			cfg.PeerURL = "https://127.0.0.1:1"
			peers, err = member.RejoinPeers(t.Context(), cfg, quiet)
			require.NoError(t, err)
			assert.Empty(t, peers)
			assert.True(t, member.HasData(cfg.DataDir), "matched by name, not by peer URL")
		})
	}
}

func TestRejoinPeers_ALeftoverMarkerDoesNotDecideWhatTheClusterAnswers(t *testing.T) {
	t.Run("the cluster lists the member: it started and the marker is stale", func(t *testing.T) {
		c := grownFromOne(t)
		cfg := c.MemberConfig(2)
		require.NoError(t, c.Members[2].Stop())
		writeMarker(t, cfg, c.MemberConfig(1).ClientURL)

		peers, err := member.RejoinPeers(t.Context(), cfg, quiet)

		require.NoError(t, err)
		assert.Empty(t, peers)
		assert.True(t, member.HasData(cfg.DataDir))
		assert.NoFileExists(t, markerOf(cfg))
	})
	t.Run("the cluster does not list the member: the crash came before the discard", func(t *testing.T) {
		c := grownFromOne(t)
		cfg := c.MemberConfig(0)
		removeStopped(t, c, 0, 1)
		writeMarker(t, cfg, c.MemberConfig(1).ClientURL)

		peers, err := member.RejoinPeers(t.Context(), cfg, quiet)

		require.NoError(t, err)
		assert.NotEmpty(t, peers)
		assert.False(t, member.HasData(cfg.DataDir))
	})
}

func writeMarker(t *testing.T, cfg member.Config, peers ...string) {
	t.Helper()
	data, err := json.Marshal(map[string][]string{"peers": peers})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(markerOf(cfg), data, 0o600))
}

func TestRejoinPeers_KeepsTheDirectoryWhenNoPeerIsKnownOrNoneAnswers(t *testing.T) {
	c := orchtest.Start(t, orchtest.Options{Members: 1})
	for name, join := range map[string]string{"no peer is known": "", "the only peer does not answer": "https://127.0.0.1:1"} {
		t.Run(name, func(t *testing.T) {
			files := c.CA.IssueFiles(t, t.TempDir(), "m9")
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "member", "snap"), 0o755))
			cfg := member.Config{Name: "m9", DataDir: dir, PeerURL: "https://127.0.0.1:2", Join: join, Cert: files.Cert, Key: files.Key, CA: files.CA}

			peers, err := member.RejoinPeers(t.Context(), cfg, quiet)

			require.NoError(t, err)
			assert.Empty(t, peers)
			assert.True(t, member.HasData(dir))
		})
	}
}
