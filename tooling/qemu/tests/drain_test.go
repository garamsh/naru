//go:build qemu_real

package tests_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainingHosts is the hosts orch lists, by ID, with whether each is draining.
func (f *fleet) drainingHosts(i int) map[string]bool {
	code, b := f.get(i, http.MethodGet, "/v1/hosts")
	if code != http.StatusOK {
		return nil
	}
	var hs []struct {
		ID       string `json:"id"`
		Draining bool   `json:"draining"`
	}
	_ = json.Unmarshal(b, &hs)
	out := map[string]bool{}
	for _, h := range hs {
		out[h.ID] = h.Draining
	}
	return out
}

func TestFleet_DrainEmptiesAHostKeepsTheCountAndNothingIsPlacedBackUntilUndrain(t *testing.T) {
	f := newFleet(t, fleetOpts{controls: 1, hosts: 2})
	f.startOrch()
	key := f.enrollmentKey()
	ids := map[int]string{}
	for _, i := range f.hosts() {
		ids[i] = f.admit(f.enroll(i, key), "eu-1", "")
	}
	h1, h2 := f.hosts()[0], f.hosts()[1]

	f.must(f.orch("group", "apply", "-f", f.groupFile("web", 4, 256, "")), "apply web")
	eventually(t, "four members run, some on each host", func() bool {
		g, ok := f.group(1, "web")
		return ok && g.count("running") == 4 && g.running()[ids[h1]] > 0 && g.running()[ids[h2]] > 0
	})

	f.must(f.orch("host", "drain", ids[h1]), "drain the first host")
	eventually(t, "orch lists the host as draining", func() bool { return f.drainingHosts(1)[ids[h1]] })
	assert.False(t, f.drainingHosts(1)[ids[h2]], "control: the other host is not draining")
	eventually(t, "the members left the host and the count is kept", func() bool {
		g, ok := f.group(1, "web")
		return ok && g.count("running") == 4 && g.running()[ids[h1]] == 0 && g.running()[ids[h2]] == 4
	})

	// Nothing is placed on a draining host, also for a group made after the drain.
	f.must(f.orch("group", "apply", "-f", f.groupFile("more", 2, 256, "")), "apply more")
	eventually(t, "the new group runs on the host that is not draining", func() bool {
		g, ok := f.group(1, "more")
		return ok && g.running()[ids[h2]] == 2
	})
	g, _ := f.group(1, "web")
	require.Equal(t, 0, g.running()[ids[h1]], "setup: the draining host is still empty")

	f.must(f.orch("host", "undrain", ids[h1]), "undrain the first host")
	eventually(t, "orch no longer lists the host as draining", func() bool {
		v, ok := f.drainingHosts(1)[ids[h1]]
		return ok && !v
	})
	f.must(f.orch("group", "apply", "-f", f.groupFile("again", 4, 256, "")), "apply again")
	eventually(t, "a member is placed on the undrained host", func() bool {
		g, ok := f.group(1, "again")
		return ok && g.count("running") == 4 && g.running()[ids[h1]] > 0
	})
}
