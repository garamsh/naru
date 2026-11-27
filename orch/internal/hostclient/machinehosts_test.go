package hostclient_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/machines"
)

// machineHosts is the reconciler's view of inv, which has listed the hosts as a pass does first.
func machineHosts(t *testing.T, inv inventory) (*hostclient.MachineHosts, []machines.Host) {
	t.Helper()
	m := hostclient.NewMachineHosts(inv, &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}, nil)
	hosts, err := m.List(context.Background())
	require.NoError(t, err)
	return m, hosts
}

func TestMachineHostsListsWhatEachHostReportsApartFromTheOperatorsLabels(t *testing.T) {
	up, down := newHost(t), newHost(t)
	down.status["GET /v1/disk-snapshots"] = 500
	_, hosts := machineHosts(t, inventory{
		{ID: "b-up", Address: up.addr(), Region: "kr-seoul", Labels: map[string]string{"naru.garam.sh/site": "a"}, Draining: true},
		{ID: "a-down", Address: down.addr(), Region: "kr-seoul"},
	})
	require.Len(t, hosts, 2)

	assert.Equal(t, machines.Host{ID: "a-down", Region: "kr-seoul"}, hosts[0], "a host that fails any of the three reads does not answer, and reports nothing")
	h := hosts[1]
	assert.True(t, h.Answering)
	assert.True(t, h.Draining, "the inventory's flag")
	assert.Equal(t, map[string]string{"naru.garam.sh/site": "a"}, h.Labels, "the operator's labels, from the inventory")
	assert.Equal(t, map[string]string{"naru.garam.sh/virtualization": "metal"}, h.Reported, "the host's own, apart")
	assert.Equal(t, hostapi.Resources{CPU: 8, Memory: 16 << 30, Disk: 1 << 40}, h.Capacity, "its capacity, not what it calls free")
	assert.Len(t, h.MicroVMs, 3)
	assert.Equal(t, []hostapi.DiskSnapshot{{Name: "s-1-0", MicroVM: "m-0123456789abcdef-1", Disk: "data", SizeBytes: 1024, AllocatedBytes: 512}}, h.DiskSnapshots)
}

func TestMachineHostsKeepsAHostAnsweringThroughOneLostProbe(t *testing.T) {
	h := newHost(t)
	m, hosts := machineHosts(t, inventory{{ID: "h", Address: h.addr()}})
	require.True(t, hosts[0].Answering)

	h.mu.Lock()
	h.status["GET /v1/host"] = 500
	h.mu.Unlock()
	for range 2 {
		hosts, err := m.List(t.Context())
		require.NoError(t, err)
		assert.True(t, hosts[0].Answering, "answered with the last answer")
		assert.Len(t, hosts[0].MicroVMs, 3)
	}
	hosts, err := m.List(t.Context())
	require.NoError(t, err)
	assert.False(t, hosts[0].Answering, "until the third failure in a row")
}

func TestMachineHostsSendsEachCallToTheHostsAPI(t *testing.T) {
	h := newHost(t)
	m, _ := machineHosts(t, inventory{{ID: "h", Address: h.addr()}})
	ctx := t.Context()

	_, err := m.Stop(ctx, "h", "m-0123456789abcdef-1", 30)
	require.NoError(t, err)
	var stop hostapi.StopRequest
	require.NoError(t, json.Unmarshal(h.body("POST /v1/microvms/m-0123456789abcdef-1/stop"), &stop))
	assert.Equal(t, uint32(30), stop.GraceSeconds, "the grace the caller asked for")

	require.NoError(t, m.SnapshotDisk(ctx, "h", "m-0123456789abcdef-1", "data", "s-1-0"))
	assert.JSONEq(t, `{"name":"s-1-0"}`, string(h.body("POST /v1/microvms/m-0123456789abcdef-1/disks/data/snapshot")))

	_, err = m.Start(ctx, "h", "m-0123456789abcdef-1")
	require.NoError(t, err)
	assert.True(t, h.saw("POST /v1/microvms/m-0123456789abcdef-1/start"))

	_, err = m.Create(ctx, "h", hostapi.CreateRequest{Name: "m-0123456789abcdef-1"})
	require.NoError(t, err)
	assert.True(t, h.saw("POST /v1/microvms"))

	_, err = m.Create(ctx, "elsewhere", hostapi.CreateRequest{Name: "m-0123456789abcdef-1"})
	require.Error(t, err, "a host the list did not read")
}

func TestMachineHostsTakesAMicroVMOrDiskSnapshotAlreadyGoneAsDone(t *testing.T) {
	h := newHost(t)
	m, _ := machineHosts(t, inventory{{ID: "h", Address: h.addr()}})
	ctx := t.Context()
	for _, k := range []string{"DELETE /v1/microvms/gone", "POST /v1/microvms/gone/stop", "DELETE /v1/disk-snapshots/gone", "POST /v1/microvms/gone/start", "POST /v1/microvms/gone/disks/data/snapshot"} {
		h.status[k] = http.StatusNotFound
	}

	require.NoError(t, m.Delete(ctx, "h", "gone"))
	vm, err := m.Stop(ctx, "h", "gone", 0)
	require.NoError(t, err)
	assert.Equal(t, "stopped", vm.State)
	require.NoError(t, m.DeleteDiskSnapshot(ctx, "h", "gone"))

	_, err = m.Start(ctx, "h", "gone")
	require.Error(t, err, "a start of one that is gone fails")
	require.Error(t, m.SnapshotDisk(ctx, "h", "gone", "data", "s-1-0"), "as does a snapshot of it")
}

func TestMachineHostsMarksAnAnswerItRepeatsStale(t *testing.T) {
	h := newHost(t)
	m, hosts := machineHosts(t, inventory{{ID: "h", Address: h.addr()}})
	require.False(t, hosts[0].Stale, "control: a fresh answer")

	h.mu.Lock()
	h.status["GET /v1/microvms"] = 500
	h.mu.Unlock()
	hosts, err := m.List(t.Context())
	require.NoError(t, err)
	assert.True(t, hosts[0].Answering)
	assert.True(t, hosts[0].Stale, "the last answer, given again")

	h.mu.Lock()
	delete(h.status, "GET /v1/microvms")
	h.mu.Unlock()
	hosts, err = m.List(t.Context())
	require.NoError(t, err)
	assert.False(t, hosts[0].Stale)
}

func TestMachineHostsReachesAGuestFromAMemberThatListedNoHost(t *testing.T) {
	h := newHost(t)
	m := hostclient.NewMachineHosts(inventory{{ID: "h", Address: h.addr()}}, &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}, nil)
	ctx := t.Context()
	const vm = "m-0123456789abcdef-1"

	out, err := m.Exec(ctx, "h", vm, hostapi.ExecRequest{Command: []string{"cat", "/data/a"}, TimeoutSeconds: 30})
	require.NoError(t, err)
	assert.Equal(t, hostapi.ExecResult{ExitCode: 137, Signal: 9, TimedOut: true, Stdout: []byte("out"), Stderr: []byte{}}, out, "the host's result as it is")
	assert.JSONEq(t, `{"command":["cat","/data/a"],"timeoutSeconds":30}`, string(h.body("POST /v1/microvms/"+vm+"/exec")))

	b, err := m.ReadFile(ctx, "h", vm, "/data/a b")
	require.NoError(t, err)
	assert.Equal(t, "content", string(b))
	assert.Equal(t, "path=%2Fdata%2Fa+b", h.query("GET /v1/microvms/"+vm+"/files"))

	require.NoError(t, m.WriteFile(ctx, "h", vm, "/data/a", []byte("raw")))
	assert.Equal(t, "raw", string(h.body("PUT /v1/microvms/"+vm+"/files")), "the content, as it is")
	assert.Equal(t, "path=%2Fdata%2Fa", h.query("PUT /v1/microvms/"+vm+"/files"))
}

func TestMachineHostsNamesTheHostsAnswerToAGuestCall(t *testing.T) {
	const key = "GET /v1/microvms/m-0123456789abcdef-1/files"
	for _, c := range []struct {
		status int
		code   string
		want   error
	}{
		{409, hostapi.CodeNotRunning, machines.ErrNotRunning},
		{404, "", machines.ErrNotRunning},
		{404, hostapi.CodeFileNotFound, machines.ErrFileNotFound},
		{413, "", machines.ErrTooLarge},
		{422, "", machineapi.ErrInvalid},
		{409, "", machines.ErrHostUnavailable},
		{502, "", machines.ErrHostUnavailable},
	} {
		h := newHost(t)
		h.status[key], h.codes[key] = c.status, c.code
		m := hostclient.NewMachineHosts(inventory{{ID: "h", Address: h.addr()}}, &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}, nil)
		_, err := m.ReadFile(t.Context(), "h", "m-0123456789abcdef-1", "/data/a")
		assert.ErrorIs(t, err, c.want, "%d %q", c.status, c.code)
	}

	h := newHost(t)
	m := hostclient.NewMachineHosts(inventory{{ID: "h", Address: h.addr()}}, &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}}, nil)
	_, err := m.ReadFile(t.Context(), "other", "m-0123456789abcdef-1", "/data/a")
	assert.ErrorIs(t, err, machines.ErrHostUnavailable, "a host the inventory does not hold")
	h.srv.Close()
	_, err = m.ReadFile(t.Context(), "h", "m-0123456789abcdef-1", "/data/a")
	assert.ErrorIs(t, err, machines.ErrHostUnavailable, "a host that does not answer")
}
