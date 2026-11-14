package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

func TestSnapshot_TakeListGetResumeDelete(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	reader := e.as(t, ReaderOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)

	code, b := e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "snap-1"})
	require.Equal(t, 201, code, string(b))
	var sn hostapi.Snapshot
	require.NoError(t, json.Unmarshal(b, &sn))
	assert.Equal(t, "snap-1", sn.Name)
	assert.Equal(t, "web", sn.MicroVM)
	assert.Equal(t, uint64(1<<30), sn.RootDiskBytes)
	assert.Equal(t, []hostapi.SnapshotDisk{{ID: "web.data", SizeBytes: 1 << 20}}, sn.Disks)
	assert.Equal(t, mvmd.StateRunning, e.rt.machines["web"].State, "the snapshot stops the microVM")

	// The same name again is the snapshot already taken.
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "snap-1"})
	assert.Equal(t, 201, code)
	assert.Len(t, e.rt.snapshots, 1)

	code, b = e.do(t, reader, "GET", "/v1/snapshots", nil)
	require.Equal(t, 200, code, string(b))
	var list struct{ Snapshots []hostapi.Snapshot }
	require.NoError(t, json.Unmarshal(b, &list))
	require.Len(t, list.Snapshots, 1)
	assert.Equal(t, "snap-1", list.Snapshots[0].Name)

	code, b = e.do(t, reader, "GET", "/v1/snapshots/snap-1", nil)
	require.Equal(t, 200, code, string(b))
	code, _ = e.do(t, reader, "GET", "/v1/snapshots/other", nil)
	assert.Equal(t, 404, code)

	// Resume makes a new microVM of its own, with the policy and prefix of the request.
	code, b = e.do(t, c, "POST", "/v1/snapshots/snap-1/resume", hostapi.ResumeRequest{
		Name: "web2", PodPrefix: "10.9.0.0/24",
		NetworkPolicy: hostapi.Policy{Egress: []hostapi.Rule{{Prefix: "10.0.0.0/8", Protocol: "tcp", Ports: []hostapi.PortRange{{First: 443, Last: 443}}}}},
	})
	require.Equal(t, 201, code, string(b))
	m := microVMOf(t, b)
	assert.Equal(t, "web2", m.Name)
	assert.Equal(t, "running", m.State)
	assert.Equal(t, "10.9.0.0/24", m.PodPrefix)
	assert.Len(t, m.NetworkPolicy.Egress, 1)
	assert.Contains(t, e.rt.machines, "web2")
	assert.Equal(t, mvmd.StateRunning, e.rt.machines["web"].State, "the source microVM is left as it was")

	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/snap-1", nil)
	assert.Equal(t, 204, code)
	assert.Empty(t, e.rt.snapshots)
	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/snap-1", nil)
	assert.Equal(t, 204, code, "an absent snapshot is deleted already")
	code, _ = e.do(t, reader, "GET", "/v1/snapshots/snap-1", nil)
	assert.Equal(t, 404, code)
}

func TestSnapshot_ErrorPaths(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)

	code, _ = e.do(t, c, "POST", "/v1/microvms/nope/snapshot", hostapi.SnapshotRequest{Name: "s"})
	assert.Equal(t, 404, code, "an unknown microVM")
	code, b := e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "Bad.Name"})
	assert.Equal(t, 422, code)
	assert.Contains(t, string(b), "name")
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", map[string]any{"name": "s", "bogus": 1})
	assert.Equal(t, 400, code)

	// A stopped microVM cannot be snapshotted: mvmd's precondition is 409.
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/stop", nil)
	require.Equal(t, 200, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	assert.Equal(t, 409, code)
	assert.Empty(t, e.rt.snapshots)

	// Resume: an unknown snapshot, a bad body, a name that is taken, a disk that is gone.
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	require.Equal(t, 200, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/snapshots/nope/resume", hostapi.ResumeRequest{Name: "x"})
	assert.Equal(t, 404, code)
	for field, body := range map[string]hostapi.ResumeRequest{
		"name":                    {Name: "Bad"},
		"podPrefix":               {Name: "x", PodPrefix: "10.9.0.1/24"},
		"networkPolicy.egress[0]": {Name: "x", NetworkPolicy: hostapi.Policy{Egress: []hostapi.Rule{{Prefix: "bad", Protocol: "tcp"}}}},
	} {
		code, b := e.do(t, c, "POST", "/v1/snapshots/s/resume", body)
		assert.Equal(t, 422, code, field)
		assert.Contains(t, string(b), field)
	}
	code, _ = e.do(t, c, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "web"})
	assert.Equal(t, 409, code, "web exists")
	delete(e.rt.disks, "web.data")
	code, b = e.do(t, c, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "web3"})
	assert.Equal(t, 409, code, "a disk the snapshot recorded is gone")
	assert.Contains(t, string(b), "web.data")
	assert.NotContains(t, e.rt.machines, "web3")

	// A snapshot that mvmd says is in use is 409 on delete.
	e.rt.failAt["deleteSnapshot s"] = mvmd.ErrPrecondition
	code, _ = e.do(t, c, "DELETE", "/v1/snapshots/s", nil)
	assert.Equal(t, 409, code)
}

func TestSnapshot_Roles(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	reader := e.as(t, ReaderOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "s"})
	require.Equal(t, 201, code)
	before := len(e.rt.calls)

	code, _ = e.do(t, reader, "POST", "/v1/microvms/web/snapshot", hostapi.SnapshotRequest{Name: "t"})
	assert.Equal(t, 403, code)
	code, _ = e.do(t, reader, "POST", "/v1/snapshots/s/resume", hostapi.ResumeRequest{Name: "x"})
	assert.Equal(t, 403, code)
	code, _ = e.do(t, reader, "DELETE", "/v1/snapshots/s", nil)
	assert.Equal(t, 403, code)
	code, _ = e.do(t, e.as(t, "other"), "GET", "/v1/snapshots", nil)
	assert.Equal(t, 403, code)
	assert.Len(t, e.rt.calls, before, "a refused request reached mvmd")
	assert.Contains(t, e.rt.snapshots, "s")
}
