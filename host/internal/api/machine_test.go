package api

import (
	"encoding/json"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// machineBody is the image form with what a Machine of the Machine API carries: a command, args, a
// user, a working directory, an environment, files and a volume mounted at a path.
func machineBody() map[string]any {
	b := createBody()
	b["command"] = []string{"/bin/app"}
	b["args"] = []string{"--port", "8080"}
	b["user"] = "1000:1000"
	b["workingDir"] = "/srv"
	b["env"] = map[string]string{"TOKEN": "s3cret"}
	b["files"] = []map[string]any{{"path": "/etc/app.conf", "mode": 0o600, "content": []byte("listen 8080")}}
	b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "mountPath": "/data"}}
	b["restartOnBoot"] = true
	return b
}

func TestCreate_GivesMvmdTheMachinesRunConfiguration(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", machineBody())
	require.Equal(t, 201, code, string(b))

	got := e.rt.created["web"]
	assert.Equal(t, []string{"/bin/app"}, got.Command)
	assert.Equal(t, []string{"--port", "8080"}, got.Args)
	assert.Equal(t, "1000:1000", got.User)
	assert.Equal(t, "/srv", got.WorkingDir)
	assert.Equal(t, map[string]string{"TOKEN": "s3cret"}, got.Env)
	assert.Equal(t, []hostapi.File{{Path: "/etc/app.conf", Mode: 0o600, Content: []byte("listen 8080")}}, got.Files)
	assert.Equal(t, []mvmd.Attachment{{ID: "web.data", MountPath: "/data"}}, got.Disks)
	assert.True(t, got.RestartOnBoot)

	var m hostapi.MicroVM
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, []hostapi.DiskSpec{{Name: "data", SizeBytes: 1 << 20, MountPath: "/data"}}, m.Disks, "the microVM shows where its disk is mounted")
	assert.NotContains(t, string(b), "s3cret", "nor any value of its environment")
}

func TestCreate_ComparesARepeatOnWhatMvmdReports(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", machineBody())
	require.Equal(t, 201, code)

	code, _ = e.do(t, c, "POST", "/v1/microvms", machineBody())
	assert.Equal(t, 200, code, "the same body")
	value := machineBody()
	value["env"] = map[string]string{"TOKEN": "another"}
	code, _ = e.do(t, c, "POST", "/v1/microvms", value)
	assert.Equal(t, 200, code, "an environment value mvmd does not report is not compared")

	for what, mutate := range map[string]func(map[string]any){
		"working directory": func(b map[string]any) { b["workingDir"] = "/opt" },
		"environment name":  func(b map[string]any) { b["env"] = map[string]string{"OTHER": "s3cret"} },
		"file mode": func(b map[string]any) {
			b["files"] = []map[string]any{{"path": "/etc/app.conf", "mode": 0o644, "content": []byte("listen 8080")}}
		},
		"file size": func(b map[string]any) {
			b["files"] = []map[string]any{{"path": "/etc/app.conf", "mode": 0o600, "content": []byte("listen 80")}}
		},
		"file path": func(b map[string]any) {
			b["files"] = []map[string]any{{"path": "/etc/other.conf", "mode": 0o600, "content": []byte("listen 8080")}}
		},
		"mount path": func(b map[string]any) {
			b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "mountPath": "/srv/data"}}
		},
	} {
		b := machineBody()
		mutate(b)
		code, _ := e.do(t, c, "POST", "/v1/microvms", b)
		assert.Equal(t, 409, code, "another "+what)
	}
}

func TestCreate_MakesADiskFromADiskSnapshot(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	e.rt.diskSnapshots["before"] = mvmd.DiskSnapshot{ID: "before", DiskID: "old.data", SizeBytes: 1 << 20}

	b := machineBody()
	b["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 21, "mountPath": "/data", "fromSnapshot": "before"}}
	code, body := e.do(t, c, "POST", "/v1/microvms", b)
	require.Equal(t, 201, code, string(body))
	assert.Equal(t, "before", e.rt.diskFrom["web.data"], "the disk is a copy of the snapshot")
	assert.Equal(t, uint64(1<<21), e.rt.disks["web.data"].SizeBytes)

	other := machineBody()
	other["name"] = "other"
	other["disks"] = []map[string]any{{"name": "data", "sizeBytes": 1 << 20, "fromSnapshot": "absent"}}
	code, _ = e.do(t, c, "POST", "/v1/microvms", other)
	assert.Equal(t, 404, code, "a snapshot the host does not keep")
	assert.NotContains(t, e.rt.machines, "other", "and nothing is made")
}

func TestStop_GivesTheGuestTheGraceAsked(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	stop := func(body any) int {
		code, b := e.do(t, c, "POST", "/v1/microvms", machineBody())
		require.Contains(t, []int{200, 201}, code, string(b))
		code, _ = e.do(t, c, "POST", "/v1/microvms/web/stop", body)
		return code
	}
	require.Equal(t, 200, stop(map[string]any{"graceSeconds": 30}))
	require.Equal(t, 200, stop(nil))
	assert.Equal(t, []time.Duration{30 * time.Second, defaultGrace}, e.rt.graces, "the grace asked, and the default with no body")

	assert.Equal(t, 422, stop(map[string]any{"graceSeconds": maxGrace + 1}))
	assert.Equal(t, 400, stop(map[string]any{"grace": 30}), "an unknown field")
	assert.Len(t, e.rt.graces, 2, "a refused stop reaches no mvmd")
}

func TestDiskSnapshots_AreTakenOfADiskListedAndDeleted(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", machineBody())
	require.Equal(t, 201, code)

	code, b := e.do(t, c, "POST", "/v1/microvms/web/disks/data/snapshot", hostapi.DiskSnapshotRequest{Name: "before"})
	require.Equal(t, 201, code, string(b))
	var d hostapi.DiskSnapshot
	require.NoError(t, json.Unmarshal(b, &d))
	assert.Equal(t, hostapi.DiskSnapshot{Name: "before", MicroVM: "web", Disk: "data", SizeBytes: 1 << 20, AllocatedBytes: 1 << 19, CreatedAt: d.CreatedAt}, d)
	assert.False(t, d.CreatedAt.IsZero())

	code, _ = e.do(t, c, "POST", "/v1/microvms/web/disks/data/snapshot", hostapi.DiskSnapshotRequest{Name: "before"})
	assert.Equal(t, 201, code, "the same name of the same disk is the snapshot taken")
	e.rt.disks["web.other"] = mvmd.Disk{ID: "web.other", SizeBytes: 1}
	m := e.rt.machines["web"]
	m.Disks = append(m.Disks, mvmd.Attachment{ID: "web.other"})
	e.rt.machines["web"] = m
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/disks/other/snapshot", hostapi.DiskSnapshotRequest{Name: "before"})
	assert.Equal(t, 409, code, "the name of a snapshot of another disk")
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/disks/absent/snapshot", hostapi.DiskSnapshotRequest{Name: "x"})
	assert.Equal(t, 404, code, "a disk the microVM has not")
	e.rt.disks["web.loose"] = mvmd.Disk{ID: "web.loose", SizeBytes: 1}
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/disks/loose/snapshot", hostapi.DiskSnapshotRequest{Name: "x"})
	assert.Equal(t, 404, code, "nor one mvmd holds under the microVM's prefix and the microVM does not")
	code, _ = e.do(t, c, "POST", "/v1/microvms/absent/disks/data/snapshot", hostapi.DiskSnapshotRequest{Name: "x"})
	assert.Equal(t, 404, code, "a microVM the host has not")
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/disks/data/snapshot", hostapi.DiskSnapshotRequest{Name: "Bad.Name"})
	assert.Equal(t, 422, code)

	reader := e.as(t, ReaderOrganization)
	code, _ = e.do(t, reader, "POST", "/v1/microvms/web/disks/data/snapshot", hostapi.DiskSnapshotRequest{Name: "y"})
	assert.Equal(t, 403, code, "a reader takes none")
	code, b = e.do(t, reader, "GET", "/v1/disk-snapshots", nil)
	require.Equal(t, 200, code, "and lists them")
	var list struct {
		DiskSnapshots []hostapi.DiskSnapshot `json:"diskSnapshots"`
	}
	require.NoError(t, json.Unmarshal(b, &list))
	if assert.Len(t, list.DiskSnapshots, 1) {
		assert.Equal(t, "before", list.DiskSnapshots[0].Name)
	}

	code, _ = e.do(t, c, "DELETE", "/v1/microvms/web", nil)
	require.Equal(t, 204, code)
	assert.Contains(t, e.rt.diskSnapshots, "before", "a disk snapshot outlives its microVM")
	code, _ = e.do(t, c, "DELETE", "/v1/disk-snapshots/before", nil)
	assert.Equal(t, 204, code)
	assert.NotContains(t, e.rt.diskSnapshots, "before")
	code, _ = e.do(t, c, "DELETE", "/v1/disk-snapshots/before", nil)
	assert.Equal(t, 204, code, "an absent one, so a retry succeeds")
}

func TestHost_ReportsItsVirtualizationApartFromTheOperatorsLabels(t *testing.T) {
	e := startEnvRevoking(t, netip10(), true, nil, func(d *Deps) { d.Virtualization = hostapi.VirtualizationNested })
	require.NoError(t, writeFile(e.hostFile, `{"region":"eu-1","labels":{"disk":"ssd","naru.garam.sh/virtualization":"metal"}}`))
	code, b := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	require.Equal(t, 200, code)
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, map[string]string{hostapi.LabelVirtualization: hostapi.VirtualizationNested}, h.ReportedLabels)
	assert.Equal(t, map[string]string{"disk": "ssd"}, h.Labels, "the operator's labels, without one the host reports")

	plain := start(t)
	_, b = plain.do(t, plain.as(t, ReaderOrganization), "GET", "/v1/host", nil)
	var none hostapi.Host
	require.NoError(t, json.Unmarshal(b, &none))
	assert.Equal(t, map[string]string{}, none.ReportedLabels, "a host that cannot tell reports none")
}

func netip10() netip.Addr { return netip.MustParseAddr("10.1.0.1") }

func writeFile(path, content string) error { return os.WriteFile(path, []byte(content), 0o600) }
