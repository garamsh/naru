package orchd_test

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
	"github.com/garamsh/naru/orch/internal/groups"
	"github.com/garamsh/naru/orch/internal/hostclient"
	"github.com/garamsh/naru/orch/internal/inventory"
	"github.com/garamsh/naru/orch/internal/orchtest"
)

// hostAPIs serve the host API of several hosts as the real client reaches it, and record every
// call that changes a microVM as "<verb> <host>/<name>", in order.
type hostAPIs struct {
	mu  sync.Mutex
	vms map[string]map[string]*hostapi.MicroVM
	ops []string
}

func newHostAPIs() *hostAPIs { return &hostAPIs{vms: map[string]map[string]*hostapi.MicroVM{}} }

// serve starts the API of host id and returns its address.
func (a *hostAPIs) serve(t *testing.T, id string) string {
	t.Helper()
	a.vms[id] = map[string]*hostapi.MicroVM{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { a.handle(id, w, r) }))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return u.Host
}

func (a *hostAPIs) handle(id string, w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	vms := a.vms[id]
	send := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case r.URL.Path == "/v1/host":
		send(hostapi.Host{Free: hostapi.Resources{CPU: 64, Memory: 64 << 30}})
	case r.URL.Path == "/v1/microvms" && r.Method == http.MethodGet:
		list := []hostapi.MicroVM{}
		for _, m := range vms {
			list = append(list, *m)
		}
		send(map[string]any{"microvms": list})
	case r.URL.Path == "/v1/microvms" && r.Method == http.MethodPost:
		var req hostapi.CreateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if _, ok := vms[req.Name]; !ok {
			a.ops = append(a.ops, "create "+id+"/"+req.Name)
			m := &hostapi.MicroVM{Name: req.Name, MachineID: req.Name, State: "running", Fenced: req.Fenced, ImageDigest: req.Image}
			if req.Fenced {
				m.State = "created"
			}
			vms[req.Name] = m
		}
		w.WriteHeader(http.StatusCreated)
		send(vms[req.Name])
	case len(parts) >= 3 && vms[parts[2]] == nil:
		w.WriteHeader(http.StatusNotFound)
		send(hostapi.ErrorBody{Error: "no such microVM"})
	case len(parts) == 3 && r.Method == http.MethodGet:
		send(vms[parts[2]])
	case len(parts) == 3 && r.Method == http.MethodDelete:
		a.ops = append(a.ops, "delete "+id+"/"+parts[2])
		delete(vms, parts[2])
		w.WriteHeader(http.StatusNoContent)
	case len(parts) == 4 && parts[3] == "stop":
		a.ops = append(a.ops, "stop "+id+"/"+parts[2])
		vms[parts[2]].State = "stopped"
		send(vms[parts[2]])
	case len(parts) == 4 && parts[3] == "start":
		a.ops = append(a.ops, "start "+id+"/"+parts[2])
		vms[parts[2]].State = "running"
		send(vms[parts[2]])
	case len(parts) == 4 && parts[3] == "fence" && r.Method == http.MethodPost:
		var req hostapi.FenceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		send(hostapi.Fence{Phase: hostapi.FenceArmed, LengthSeconds: req.LengthSeconds, RemainingSeconds: req.LengthSeconds, Renewal: req.Renewal})
	case len(parts) == 4 && parts[3] == "fence":
		send(hostapi.Fence{Phase: hostapi.FenceArmed, LengthSeconds: 600, RemainingSeconds: 600})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// onHost is the names of the microVMs the host holds in state.
func (a *hostAPIs) onHost(id, state string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for n, m := range a.vms[id] {
		if m.State == state {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func (a *hostAPIs) opsSoFar() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.ops)
}

// drainFleet is a one-member orch whose groups engine drives two admitted hosts through the shipped
// host client, and two host APIs standing behind them.
type drainFleet struct {
	*one
	apis *hostAPIs
}

func startDrainFleet(t *testing.T) *drainFleet {
	t.Helper()
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // the fake host APIs serve a throwaway certificate
	d := &drainFleet{apis: newHostAPIs()}
	d.one = startOne(t, orchtest.Options{NewHosts: func(inv inventory.Service) groups.Hosts { return hostclient.New(inv, insecure) }})
	for _, id := range []string{"host-a", "host-b"} {
		h := newHost(t, id)
		req := h.request(d.secret)
		req.Address = d.apis.serve(t, id)
		_, err := d.anon.Enroll(t.Context(), req)
		require.NoError(t, err)
		_, err = d.op.Admit(t.Context(), orchapi.AdmitRequest{HostID: id, Code: identity.PairingCode(h.pub), Statement: orchtest.Statement(t, d.operator, id, h.pub), Region: "kr-seoul"})
		require.NoError(t, err)
	}
	d.c.Leader(t, []int{0}, 30*time.Second)
	return d
}

func (d *drainFleet) draining(t *testing.T) map[string]bool {
	t.Helper()
	hs, err := d.reader.Hosts(t.Context())
	require.NoError(t, err)
	out := map[string]bool{}
	for _, h := range hs {
		out[h.ID] = h.Draining
	}
	return out
}

func TestDrainAPIIsAnOperatorsAndEachDrainIsRecorded(t *testing.T) {
	d := startDrainFleet(t)

	_, err := d.reader.Drain(t.Context(), "host-a")
	require.Equal(t, http.StatusForbidden, status(err), "a reader")
	_, err = d.anon.Drain(t.Context(), "host-a")
	require.Equal(t, http.StatusUnauthorized, status(err), "no certificate")
	_, err = d.reader.Undrain(t.Context(), "host-a")
	require.Equal(t, http.StatusForbidden, status(err), "a reader may not undrain")
	require.Equal(t, map[string]bool{"host-a": false, "host-b": false}, d.draining(t), "setup: the refusals changed nothing")

	_, err = d.op.Drain(t.Context(), "no-such-host")
	require.Equal(t, http.StatusNotFound, status(err))

	h, err := d.op.Drain(t.Context(), "host-a")
	require.NoError(t, err)
	require.True(t, h.Draining)
	require.Equal(t, map[string]bool{"host-a": true, "host-b": false}, d.draining(t))

	h, err = d.op.Undrain(t.Context(), "host-a")
	require.NoError(t, err)
	require.False(t, h.Draining)
	require.Equal(t, map[string]bool{"host-a": false, "host-b": false}, d.draining(t))

	events, err := d.reader.Events(t.Context())
	require.NoError(t, err)
	var types []string
	for _, e := range events {
		if e.Subject == "host-a" {
			types = append(types, e.Type)
		}
	}
	require.Contains(t, types, inventory.EventDrained)
	require.Contains(t, types, inventory.EventUndrained)
}

func TestDrainMovesAnImageMemberOffTheHostKeepsTheCountAndUndrainPlacesOnItAgain(t *testing.T) {
	d := startDrainFleet(t)
	spec := webGroup()
	spec.Count = 2
	spec.Rollout = orchapi.GroupRollout{MaxUnavailable: 1, MaxSurge: 1}
	spec.MinAvailable = 1
	_, err := d.op.ApplyGroup(t.Context(), spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(d.apis.onHost("host-a", "running")) == 1 && len(d.apis.onHost("host-b", "running")) == 1
	}, 30*time.Second, 100*time.Millisecond, "setup: a member on each host")

	_, err = d.op.Drain(t.Context(), "host-a")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(d.apis.onHost("host-a", "running")) == 0 && len(d.apis.onHost("host-b", "running")) == 2
	}, 30*time.Second, 100*time.Millisecond, "the member left the draining host and the count is kept")

	// Nothing is placed on the draining host, also for a larger count.
	spec.Count = 4
	_, err = d.op.ApplyGroup(t.Context(), spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(d.apis.onHost("host-b", "running")) == 4 }, 30*time.Second, 100*time.Millisecond)
	require.Never(t, func() bool { return len(d.apis.onHost("host-a", "")) > 0 }, 2*time.Second, 100*time.Millisecond)

	_, err = d.op.Undrain(t.Context(), "host-a")
	require.NoError(t, err)
	spec.Count = 6
	_, err = d.op.ApplyGroup(t.Context(), spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return len(d.apis.onHost("host-a", "running")) > 0 && len(d.apis.onHost("host-a", "running"))+len(d.apis.onHost("host-b", "running")) == 6
	}, 30*time.Second, 100*time.Millisecond, "after undrain a member is placed on the host again")
}

func TestDrainMovesAStatefulMemberOnlyAfterItIsStopped(t *testing.T) {
	d := startDrainFleet(t)
	spec := webGroup()
	spec.Name, spec.Count, spec.Stateful, spec.FenceTTLSeconds = "web", 1, true, 600
	spec.Rollout, spec.MinAvailable = orchapi.GroupRollout{MaxUnavailable: 1}, 0
	_, err := d.op.ApplyGroup(t.Context(), spec)
	require.NoError(t, err)
	var from, to string
	require.Eventually(t, func() bool {
		for _, id := range []string{"host-a", "host-b"} {
			if len(d.apis.onHost(id, "running")) == 1 {
				from, to = id, map[string]string{"host-a": "host-b", "host-b": "host-a"}[id]
				return true
			}
		}
		return false
	}, 30*time.Second, 100*time.Millisecond, "setup: the stateful member runs on one host")
	first := d.apis.onHost(from, "running")[0]

	_, err = d.op.Drain(t.Context(), from)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(d.apis.onHost(to, "running")) == 1 && len(d.apis.onHost(from, "")) == 0 }, 30*time.Second, 100*time.Millisecond, "the member moved to the other host")

	ops := d.apis.opsSoFar()
	idx := func(op string) int { return slices.Index(ops, op) }
	moved := d.apis.onHost(to, "running")[0]
	require.NotEqual(t, first, moved, "the replacement is a new member")
	require.NotEqual(t, -1, idx("stop "+from+"/"+first), "the member was stopped: %v", ops)
	require.Less(t, idx("stop "+from+"/"+first), idx("delete "+from+"/"+first), "it was stopped before it was deleted")
	require.Less(t, idx("delete "+from+"/"+first), idx("create "+to+"/"+moved), "and gone before its replacement was made: %v", ops)
}
