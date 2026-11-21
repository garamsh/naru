//go:build qemu_real

package tests_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A group whose required label no host carries is held, and the operator reads the reason from
// naructl group get, group list and events without the console.
func TestFleet_AGroupNoHostCanTakeShowsItsHoldReasonInGroupGetListAndEvents(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 1})
	f.startOrch()
	f.enrollAndAdmit("eu-1")

	f.must(f.orch("group", "apply", "-f", f.groupFile("stuck", 1, 256, "requiredLabels:\n  tier: gold\n")), "apply stuck")

	const reason = "required label tier=gold missing"
	eventually(t, "group get shows the hold with its time and reason", func() bool {
		r := f.orch("group", "get", "stuck")
		return r.Err == nil && strings.Contains(r.Stdout, "held at ") && strings.Contains(r.Stdout, reason)
	})
	list := f.must(f.orch("group", "list"), "list the groups")
	assert.Regexp(t, `stuck\s+eu-1\s+1\s+0\s+0\s+yes`, list)

	evs := f.must(f.orch("events", "--group", "stuck"), "events of the group")
	assert.Contains(t, evs, "group.hold")
	assert.Contains(t, evs, reason)

	// A group the host can take is not held, and its events are not in the first group's list.
	f.must(f.orch("group", "apply", "-f", f.groupFile("fits", 1, 256, "")), "apply fits")
	eventually(t, "the group with no required label runs", func() bool {
		g, ok := f.group(1, "fits")
		return ok && g.count("running") == 1
	})
	fits := f.must(f.orch("group", "get", "fits"), "get fits")
	assert.NotContains(t, fits, "held at", "control: a placed group shows no hold")
	only := f.must(f.orch("events", "--group", "stuck"), "events of the group")
	require.NotContains(t, only, "fits")
}
