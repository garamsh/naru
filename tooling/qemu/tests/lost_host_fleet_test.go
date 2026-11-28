//go:build qemu_real

package tests_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A host that stops answering loses its members to another host that fits, with no drain: the
// stateless group keeps its count on the host that is left, which the operator's recovery of a lost
// host relies on (docs/operate/recover-a-member-or-host.md).
func TestFleet_AStatelessMemberOfAHostThatStopsAnsweringIsMadeOnTheOtherHostWithNoDrain(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2, vmMiB: 1536, vmCPUs: 4})
	f.startOrch()
	ids := map[int]string{}
	for _, i := range f.hosts() {
		ids[i] = f.admit(f.enroll(i, f.enrollmentKey()), "eu-1", "tier=a")
	}
	h1, h2 := f.hosts()[0], f.hosts()[1]

	f.must(f.orch("group", "apply", "-f", f.groupFile("lost", 1, 256, "")), "apply lost")
	var lost, kept int
	eventually(t, "the member runs on one of the two hosts", func() bool {
		g, ok := f.group(1, "lost")
		if !ok || g.count("running") != 1 {
			return false
		}
		switch {
		case g.running()[ids[h1]] == 1:
			lost, kept = h1, h2
		case g.running()[ids[h2]] == 1:
			lost, kept = h2, h1
		default:
			return false
		}
		return true
	})

	// The host is stopped with no call to orch: no drain, no removal.
	f.node(lost).Stop()
	eventually(t, "the member runs on the host that is left", func() bool {
		g, ok := f.group(1, "lost")
		return ok && g.running()[ids[kept]] == 1
	})
	g, ok := f.group(1, "lost")
	assert.True(t, ok)
	assert.Equal(t, 1, g.running()[ids[kept]], "the count is kept on the host that answers")
}
