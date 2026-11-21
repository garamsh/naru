//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/tooling/qemu/internal/fixture"
)

// The tests of orch's etcd housekeeping (ADR 0088) run three control Nodes with the leader's departure
// time, pass interval and etcd retention and quota set short in spec.orch.

const (
	removedMember = "removed a member that stayed unreachable"
	discardedDir  = "discarded the member directory of a member the cluster removed"
	notAnswering  = "a member does not answer"
	defragmented  = "defragmented a member"
	disarmed      = "disarmed the no-space alarm of a member"
)

// removeAndRejoin stops control Node out, waits for the leader among the others to remove it after the
// departure time and not before, and boots it again: it must discard its old data directory, join
// afresh and take a write.
func removeAndRejoin(t *testing.T, f *fleet, out int) {
	t.Helper()
	var alive []int
	for _, i := range f.controls() {
		if i != out {
			alive = append(alive, i)
		}
	}
	f.node(out).Stop()

	var leader int
	eventually(t, "a member that answers leads", func() bool { leader = f.leader(alive); return leader != 0 })
	// The control: the leader has seen the member out, and it has not removed it, since the departure time has not passed.
	eventually(t, "the leader sees the member out", func() bool { return f.node(leader).Has(notAnswering, orchName(out)) })
	assert.False(t, f.node(leader).Has(removedMember, orchName(out)), "a member is not removed before the departure time")
	f.must(f.orchAs(f.root, leader, "enroll-key", "create"), "the two members left take a write")
	// After it, the leader removes that member, and no other: the others answer, and the cluster had three.
	eventually(t, "the leader removes the member that stayed out", func() bool {
		return f.node(leader).Has(removedMember, orchName(out))
	})
	for _, i := range alive {
		assert.False(t, f.node(leader).Has(removedMember, orchName(i)), "orch-%d answers and is not removed", i)
	}

	// The member starts again with its old data directory, which the cluster has forgotten: it wipes it and joins afresh.
	require.NoError(t, f.node(out).Reboot())
	eventually(t, "the removed member discards its member directory", func() bool { return f.node(out).Has(discardedDir) })
	eventually(t, "the member serves again as a member of the cluster", func() bool {
		return f.orchAs(f.root, out, "host", "pending").Err == nil
	})
	f.must(f.orchAs(f.root, out, "enroll-key", "create"), "the rejoined member takes a write through the cluster")
}

func housekeepingFleet(t *testing.T, join bool) *fleet {
	t.Helper()
	f := newFleet(t, fleetOpts{controls: 3, orchJoin: join, orchExtra: func(int) map[string]any {
		return map[string]any{"departAfter": "90s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })
	return f
}

// TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts runs
// it on a cluster whose members all started with a static initial cluster, for a member that is not the
// leader; and on a cluster that grew from Node 1 alone, as the image's does, for Node 1 itself, which
// has no join flag, and then for Node 3, which joined.
func TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts(t *testing.T) {
	t.Run("a member of a static initial cluster", func(t *testing.T) {
		f := housekeepingFleet(t, false)
		removeAndRejoin(t, f, f.leader(f.controls())%3+1)
	})
	t.Run("a first member that started alone, then a member that joined", func(t *testing.T) {
		f := housekeepingFleet(t, true)
		removeAndRejoin(t, f, 1)
		eventually(t, "a member leads again", func() bool { return f.leader(f.controls()) != 0 })
		removeAndRejoin(t, f, 3)
	})
}

// bloatedProfile sets the host profile with a registry authority repeated until the profile is about
// 400 KiB, set through control Node via: every set is a revision of that size, which compaction and
// defragmentation must give back.
func (f *fleet) bloatedProfile(via, n int) result {
	f.t.Helper()
	guest, err := json.Marshal(inputs.Guest)
	require.NoError(f.t, err)
	certs, err := fixture.ServerCerts(net.ParseIP(nodeIP(1)))
	require.NoError(f.t, err)
	bundle := strings.Repeat(string(certs.CA), 400)
	profile := filepath.Join(f.dir, fmt.Sprintf("bloated-%d.yaml", n))
	orchURL := "https://" + nodeIP(1) + ":" + orchPort
	f.must(nrun("gen", "host-profile", "--guest", f.file("guest.json", string(guest)), "--firecracker-sha256", inputs.FirecrackerSHA256,
		"--machine-pool", "10.77.0.0/16", "--orch-url", orchURL, "--orch-ca-sha256", f.caSHA,
		"--registry", "host="+nodeIP(1)+":"+joinRegistryPort+",ca="+f.file("registry-ca.pem", bundle),
		"--host-management-prefix", managementPrefix, "--host-egress-interface", "eth0", "--host-tunnel-port", "51820", "--out", profile), "gen host-profile")
	return f.orchAs(f.root, via, "host", "profile", "set", "-f", profile)
}

const spaceExceeded = "database space exceeded"

func refusedForSpace(r result) bool {
	return r.Err != nil && strings.Contains(r.Stdout+r.Stderr, spaceExceeded)
}

func TestFleet_TheHousekeeperKeepsEtcdUnderItsQuotaThroughMoreWritesThanTheQuotaHolds(t *testing.T) {
	const quota = 16 << 20
	f := newFleet(t, fleetOpts{controls: 3, orchExtra: func(int) map[string]any {
		return map[string]any{"etcdQuotaBytes": quota, "etcdCompactionRetention": "30s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })

	// About 400 KiB a set, one every 3 seconds, which keeps the live data well under the quota, until more than twice the
	// quota has been written and a defragmentation has been seen. Without compaction and defragmentation the quota is
	// spent and the sets fail with "database space exceeded".
	deadline := time.Now().Add(15 * time.Minute)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	var written int
	defragSeen := func() bool {
		for _, i := range f.controls() {
			if f.node(i).Has(defragmented) {
				return true
			}
		}
		return false
	}
	for n := 0; written < 2*quota || !defragSeen(); n++ {
		require.True(t, time.Now().Before(deadline), "after %d bytes written the housekeeper had not defragmented a member", written)
		<-tick.C
		res := f.bloatedProfile(1, n)
		f.must(res, "a write while the quota is smaller than what has been written")
		written += 400 << 10
	}

	for _, i := range f.controls() {
		assert.False(t, f.node(i).Has(spaceExceeded), "no member ran out of space")
	}
}

func TestFleet_TheHousekeeperDisarmsTheNoSpaceAlarmOnceCompactionAndDefragmentationFreeTheDatabase(t *testing.T) {
	const quota = 8 << 20
	f := newFleet(t, fleetOpts{controls: 3, orchExtra: func(int) map[string]any {
		return map[string]any{"etcdQuotaBytes": quota, "etcdCompactionRetention": "20s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })

	// Revisions of 400 KiB, faster than the 20 seconds that must pass before compaction may drop one, fill the quota.
	require.True(t, f.fill(1, 40), "the writes filled the quota and etcd raised its no-space alarm: the premise of the test")

	eventually(t, "the leader disarms the alarm of a member whose database is free again", func() bool {
		l := f.leader(f.controls())
		return l != 0 && f.node(l).Has(disarmed)
	})
	eventually(t, "the cluster takes a write again", func() bool { return f.orchAs(f.root, 1, "enroll-key", "create").Err == nil })
}

// fill sets bloated profiles through control Node via until one is refused for want of space, and
// reports whether one was within max.
func (f *fleet) fill(via, max int) bool {
	f.t.Helper()
	for n := range max {
		r := f.bloatedProfile(via, n)
		if refusedForSpace(r) {
			return true
		}
		f.must(r, "a write before the quota is spent")
	}
	return false
}

// TestFleet_AMemberThatWasOutWhileTheQuotaWasSpentIsRemovedAndRejoinsAfterTheAlarmIsDisarmed fills the
// quota of a cluster, stops a member while the no-space alarm is armed, and expects the leader to
// remove it despite the alarm, to defragment and disarm once it is gone, and the member to rejoin.
func TestFleet_AMemberThatWasOutWhileTheQuotaWasSpentIsRemovedAndRejoinsAfterTheAlarmIsDisarmed(t *testing.T) {
	const quota = 8 << 20
	f := newFleet(t, fleetOpts{controls: 3, orchExtra: func(int) map[string]any {
		return map[string]any{"etcdQuotaBytes": quota, "etcdCompactionRetention": "20s", "departAfter": "60s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })
	require.True(t, f.fill(1, 40), "the writes filled the quota and etcd raised its no-space alarm: the premise of the test")

	out := f.leader(f.controls())%3 + 1
	var alive []int
	for _, i := range f.controls() {
		if i != out {
			alive = append(alive, i)
		}
	}
	f.node(out).Stop()

	var leader int
	eventually(t, "a member that answers leads", func() bool { leader = f.leader(alive); return leader != 0 })
	if !f.node(leader).Has(removedMember, orchName(out)) {
		assert.True(t, refusedForSpace(f.orchAs(f.root, leader, "enroll-key", "create")), "the alarm holds while the member is out")
	}
	eventually(t, "the leader removes the member although the alarm is armed", func() bool {
		return f.node(leader).Has(removedMember, orchName(out))
	})
	eventually(t, "the leader disarms the alarm once the member is gone and the database is free", func() bool {
		return f.node(leader).Has(defragmented) && f.node(leader).Has(disarmed)
	})
	eventually(t, "the two members take a write again", func() bool { return f.orchAs(f.root, leader, "enroll-key", "create").Err == nil })

	require.NoError(t, f.node(out).Reboot())
	eventually(t, "the removed member discards its member directory", func() bool { return f.node(out).Has(discardedDir) })
	eventually(t, "the member serves again as a member of the cluster", func() bool {
		return f.orchAs(f.root, out, "host", "pending").Err == nil
	})
	f.must(f.orchAs(f.root, out, "enroll-key", "create"), "the rejoined member takes a write through the cluster")
}
