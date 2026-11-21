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

// TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts runs
// it for Node 1, which starts with a static initial cluster as the image's first member does, and for
// Node 3, which joined Node 1's orch with spec.orch.join.
func TestFleet_TheLeaderRemovesAMemberThatStaysOutOnlyWhileTheOthersAnswerAndItRejoinsEmptyWhenItStarts(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  int
	}{{"a member started with a static initial cluster", 1}, {"a member that joined", 3}} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFleet(t, fleetOpts{controls: 3, orchJoin: true, orchExtra: func(int) map[string]any {
				return map[string]any{"departAfter": "90s", "housekeepingInterval": "5s"}
			}})
			f.startOrch()
			var alive []int
			for _, i := range f.controls() {
				if i != tt.out {
					alive = append(alive, i)
				}
			}
			eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })

			f.node(tt.out).Stop()

			var leader int
			eventually(t, "a member that answers leads", func() bool { leader = f.leader(alive); return leader != 0 })
			// The control: the leader has seen the member out, and it has not removed it, since the departure time has not passed.
			eventually(t, "the leader sees the member out", func() bool { return f.node(leader).Has(notAnswering, orchName(tt.out)) })
			assert.False(t, f.node(leader).Has(removedMember), "a member is not removed before the departure time")
			f.must(f.orchAs(f.root, leader, "enroll-key", "create"), "the two members left take a write")
			// After it, the leader removes that member, and no other: the others answer, and the cluster had three.
			eventually(t, "the leader removes the member that stayed out", func() bool {
				return f.node(leader).Has(removedMember, orchName(tt.out))
			})
			for _, i := range alive {
				assert.False(t, f.node(leader).Has(removedMember, orchName(i)), "orch-%d answers and is not removed", i)
			}

			// The member starts again with its old data directory, which the cluster has forgotten: it wipes it and joins afresh.
			require.NoError(t, f.node(tt.out).Reboot())
			eventually(t, "the removed member discards its member directory", func() bool { return f.node(tt.out).Has(discardedDir) })
			eventually(t, "the member serves again as a member of the cluster", func() bool {
				return f.orchAs(f.root, tt.out, "host", "pending").Err == nil
			})
			f.must(f.orchAs(f.root, tt.out, "enroll-key", "create"), "the rejoined member takes a write through the cluster")
		})
	}
}

// bloatedProfile sets the host profile with a registry authority repeated until the profile is about
// 400 KiB: every set is a revision of that size, which compaction and defragmentation must give back.
func (f *fleet) bloatedProfile(n int) result {
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
	return f.orch("host", "profile", "set", "-f", profile)
}

func TestFleet_TheHousekeeperKeepsEtcdUnderItsQuotaThroughMoreWritesThanTheQuotaHolds(t *testing.T) {
	const quota = 16 << 20
	f := newFleet(t, fleetOpts{controls: 3, orchExtra: func(int) map[string]any {
		return map[string]any{"etcdQuotaBytes": quota, "etcdCompactionRetention": "30s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })

	// About 400 KiB a set, every 3 seconds, for 4 minutes: some 30 MiB of revisions, twice the quota. Without compaction
	// and defragmentation the quota is spent and the sets fail with "database space exceeded".
	for n := range 80 {
		f.must(f.bloatedProfile(n), "a write while the quota is smaller than what has been written")
		time.Sleep(3 * time.Second)
	}

	leader := f.leader(f.controls())
	assert.True(t, f.node(leader).Has(defragmented), "the leader defragmented the members once compaction left them mostly free pages")
	assert.False(t, f.node(leader).Has("database space exceeded"), "no member ran out of space")
}

func TestFleet_TheHousekeeperDisarmsTheNoSpaceAlarmOnceCompactionAndDefragmentationFreeTheDatabase(t *testing.T) {
	const quota = 8 << 20
	f := newFleet(t, fleetOpts{controls: 3, orchExtra: func(int) map[string]any {
		return map[string]any{"etcdQuotaBytes": quota, "etcdCompactionRetention": "20s", "housekeepingInterval": "5s"}
	}})
	f.startOrch()
	eventually(t, "a member leads", func() bool { return f.leader(f.controls()) != 0 })

	// Revisions of 400 KiB, faster than the 20 seconds that must pass before compaction may drop one, fill the quota.
	var refused bool
	for n := range 40 {
		if f.bloatedProfile(n).Err != nil {
			refused = true
			break
		}
	}
	require.True(t, refused, "the writes filled the quota and etcd raised its no-space alarm: the premise of the test")

	eventually(t, "the leader disarms the alarm of a member whose database is free again", func() bool {
		return f.node(f.leader(f.controls())).Has(disarmed)
	})
	eventually(t, "the cluster takes a write again", func() bool { return f.orchAs(f.root, 1, "enroll-key", "create").Err == nil })
}
