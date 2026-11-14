package hostclient_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/hostclient"
)

type inventory []orchapi.Host

func (i inventory) Hosts(context.Context) ([]orchapi.Host, error) { return i, nil }

// fakeHost records the requests it gets.
type fakeHost struct {
	mu       sync.Mutex
	requests []string
	bodies   map[string][]byte
	status   map[string]int
	srv      *httptest.Server
}

func newHost(t *testing.T) *fakeHost {
	t.Helper()
	f := &fakeHost{bodies: map[string][]byte{}, status: map[string]int{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, key)
		f.bodies[key], _ = io.ReadAll(r.Body)
		if st := f.status[key]; st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		switch key {
		case "GET /v1/host":
			_, _ = w.Write([]byte(`{"free":{"cpu":6,"memory":8589934592},"mvmd":{"kernels":[{"name":"node-1"},{"name":"node-2"}]}}`))
		case "GET /v1/microvms":
			_, _ = w.Write([]byte(`{"microvms":[{"name":"a","state":"running","network":"lab","podPrefix":"10.9.0.64/26","addresses":["10.9.0.2"]},{"name":"b","state":"stopped"},{"name":"c","state":"created"}]}`))
		case "GET /v1/microvms/db-1/fence":
			_, _ = w.Write([]byte(`{"phase":"armed","remainingSeconds":90}`))
		case "GET /v1/microvms/db-1":
			_, _ = w.Write([]byte(`{"name":"db-1","fenceLapsedAt":"2026-11-14T01:02:03Z"}`))
		case "POST /v1/microvms/db-1/fence":
			_, _ = w.Write([]byte(`{"phase":"armed"}`))
		default:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHost) addr() string { u, _ := url.Parse(f.srv.URL); return u.Host }

func client(inv inventory) *hostclient.Client {
	return hostclient.New(inv, &http.Client{Transport: &http.Transport{TLSClientConfig: insecure()}})
}

func TestListReportsReadinessCapacityKernelsAndMachines(t *testing.T) {
	up := newHost(t)
	down := newHost(t)
	down.status["GET /v1/microvms"] = 500
	c := client(inventory{
		{ID: "up", Address: up.addr(), Region: "kr-seoul", Labels: map[string]string{"rack": "a"}},
		{ID: "down", Address: down.addr(), Region: "kr-seoul"},
	})
	hosts, err := c.List(t.Context())
	require.NoError(t, err)
	require.Len(t, hosts, 2)

	require.Equal(t, "down", hosts[0].ID)
	require.False(t, hosts[0].Ready)
	require.Empty(t, hosts[0].Machines)

	h := hosts[1]
	require.True(t, h.Ready)
	require.Equal(t, groups.Capacity{CPUs: 6, MemoryMiB: 8192}, h.Capacity)
	require.Equal(t, []string{"node-1", "node-2"}, h.NodeVersions)
	require.Equal(t, map[string]string{"rack": "a"}, h.Labels)
	require.Equal(t, []groups.Machine{
		{ID: "a", State: groups.MachineRunning, Network: "lab", PodPrefix: "10.9.0.64/26", Addresses: []string{"10.9.0.2"}}, {ID: "b", State: groups.MachineStopped}, {ID: "c", State: groups.MachineCreated},
	}, h.Machines)
}

func TestCreateSendsTheImageForm(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	err := c.Create(t.Context(), "h", groups.MachineSpec{ID: "web-1", Template: groups.Template{
		Form: groups.FormImage, CPUs: 2, MemoryMiB: 256, Version: "r@sha256:abc", RootDiskBytes: 8 << 30, Network: "lab",
		Disks:  []groups.Disk{{Name: "data", SizeBytes: 1 << 30}},
		Policy: groups.NetworkPolicy{Egress: []groups.Rule{{Prefix: "10.0.0.0/8", Protocol: "tcp", Ports: []int{443}}}},
	}, PodPrefix: "10.9.0.0/26"})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(h.bodies["POST /v1/microvms"], &got))
	require.Equal(t, "web-1", got["name"])
	require.Equal(t, "lab", got["network"])
	require.Equal(t, "10.9.0.0/26", got["podPrefix"])
	require.Equal(t, "r@sha256:abc", got["image"])
	require.EqualValues(t, 2, got["cpu"])
	require.EqualValues(t, 256<<20, got["memory"])
	require.EqualValues(t, 8<<30, got["rootDiskBytes"])
	require.Equal(t, []any{map[string]any{"name": "data", "sizeBytes": float64(1 << 30)}}, got["disks"])
	require.NotContains(t, got, "bootSet")
	egress := got["networkPolicy"].(map[string]any)["egress"]
	require.Equal(t, []any{map[string]any{"prefix": "10.0.0.0/8", "protocol": "tcp", "ports": []any{map[string]any{"first": 443.0, "last": 443.0}}}}, egress)
}

func TestCreateSendsTheOSForm(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	err := c.Create(t.Context(), "h", groups.MachineSpec{ID: "db-1", Template: groups.Template{
		Form: groups.FormOS, CPUs: 1, MemoryMiB: 128, NodeVersion: "node-2", Disks: []groups.Disk{{Name: "data", SizeBytes: 2 << 30}},
	}})
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, json.Unmarshal(h.bodies["POST /v1/microvms"], &got))
	require.Equal(t, "node-2", got["bootSet"])
	require.EqualValues(t, 2<<30, got["dataDisk"])
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte(groups.DefaultUserData)), got["seed"], "no user-data gives the default")
	require.Empty(t, got["image"])

	// User-data the template gives is sent as written.
	err = c.Create(t.Context(), "h", groups.MachineSpec{ID: "db-3", Template: groups.Template{
		Form: groups.FormOS, CPUs: 1, MemoryMiB: 128, NodeVersion: "node-2", Disks: []groups.Disk{{Name: "data", SizeBytes: 2 << 30}},
		UserData: "#cloud-config\nhostname: db\n",
	}})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(h.bodies["POST /v1/microvms"], &got))
	require.Equal(t, base64.StdEncoding.EncodeToString([]byte("#cloud-config\nhostname: db\n")), got["seed"])

	// An OS-form template with no disk has no data disk to size.
	err = c.Create(t.Context(), "h", groups.MachineSpec{ID: "db-2", Template: groups.Template{Form: groups.FormOS, CPUs: 1, MemoryMiB: 128, NodeVersion: "node-2"}})
	require.Error(t, err)
}

func TestCreateRefusesAnImageTemplateWithoutARootDisk(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	tpl := groups.Template{Form: groups.FormImage, CPUs: 1, MemoryMiB: 128, Version: "r@sha256:abc", RootDiskBytes: 1 << 30}
	require.NoError(t, c.Create(t.Context(), "h", groups.MachineSpec{ID: "ok-1", Template: tpl}), "control: the same template with a root disk is created")

	tpl.RootDiskBytes = 0
	require.Error(t, c.Create(t.Context(), "h", groups.MachineSpec{ID: "bad-1", Template: tpl}))
}

func TestFencedCreateAsksForAFenceAndRenewalSendsLengthAndRenewalNumber(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	now := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }

	err := c.Create(t.Context(), "h", groups.MachineSpec{ID: "db-1", Fenced: true, Template: groups.Template{Form: groups.FormImage, CPUs: 1, MemoryMiB: 128, Version: "r@sha256:abc", RootDiskBytes: 1 << 30}})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(h.bodies["POST /v1/microvms"], &got))
	require.Equal(t, true, got["fenced"])

	deadline := now.Add(90*time.Second + 500*time.Millisecond)
	require.NoError(t, c.RenewFence(t.Context(), "h", "db-1", deadline))
	var fr map[string]any
	require.NoError(t, json.Unmarshal(h.bodies["POST /v1/microvms/db-1/fence"], &fr))
	require.EqualValues(t, 90, fr["lengthSeconds"], "the length is rounded down")
	require.EqualValues(t, deadline.UnixNano(), fr["renewal"])

	require.Error(t, c.RenewFence(t.Context(), "h", "db-1", now.Add(time.Millisecond)))
}

func TestInspectFenceReadsTheDeadlineFromTheTimeLeftAndWhenItLapsed(t *testing.T) {
	h := newHost(t)
	c := client(inventory{{ID: "h", Address: h.addr()}})
	now := time.Date(2026, 11, 14, 1, 0, 0, 0, time.UTC)
	c.Now = func() time.Time { return now }

	deadline, lapsed, err := c.InspectFence(t.Context(), "h", "db-1")
	require.NoError(t, err)
	require.Equal(t, now.Add(90*time.Second), deadline)
	require.Equal(t, time.Date(2026, 11, 14, 1, 2, 3, 0, time.UTC), lapsed)

	h.bodies = map[string][]byte{}
	h.status["GET /v1/microvms/db-1/fence"] = 500
	_, _, err = c.InspectFence(t.Context(), "h", "db-1")
	require.Error(t, err)
}

func TestDeleteAndStopTreatAMissingMachineAsDoneButStartDoesNot(t *testing.T) {
	h := newHost(t)
	h.status["DELETE /v1/microvms/a"] = 404
	h.status["POST /v1/microvms/a/stop"] = 404
	h.status["POST /v1/microvms/a/start"] = 404
	h.status["DELETE /v1/microvms/b"] = 500
	c := client(inventory{{ID: "h", Address: h.addr()}})

	require.NoError(t, c.Delete(t.Context(), "h", "a"))
	require.NoError(t, c.Stop(t.Context(), "h", "a"))
	var he *hostapi.Error
	require.ErrorAs(t, c.Start(t.Context(), "h", "a"), &he)
	require.Equal(t, 404, he.Status)
	require.ErrorAs(t, c.Delete(t.Context(), "h", "b"), &he)
	require.Equal(t, 500, he.Status)

	require.Error(t, c.Delete(t.Context(), "nobody", "a"))
}
