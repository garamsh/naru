package command_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostFake is a host API that records the requests it is sent.
type hostFake struct {
	cfg      string
	requests []string
	bodies   map[string]map[string]any
}

// newHostFake serves handlers behind the host authority of a new deployment and returns the
// client file of a provisioner. Every request is noted as "METHOD /path" with its JSON body.
func newHostFake(t *testing.T, handlers map[string]http.HandlerFunc) *hostFake {
	t.Helper()
	dep := newDeployment(t)
	mustRun(t, "gen", "host-ca", "--ca-dir", dep.ca)
	f := &hostFake{bodies: map[string]map[string]any{}}
	mux := http.NewServeMux()
	for pattern, h := range handlers {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			key := r.Method + " " + r.URL.EscapedPath()
			f.requests = append(f.requests, key)
			f.bodies[key] = body
			h(w, r)
		})
	}
	addr := hostServer(t, dep, mux)
	f.cfg = filepath.Join(t.TempDir(), "c.yaml")
	mustRun(t, "gen", "host-client", "--ca-dir", dep.ca, "--role", "provisioner", "--endpoint", addr, "--out", f.cfg)
	return f
}

func answer(status int, v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if v != nil {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
}

func TestMicrovm_GetShowsTheFenceTheHostKeeps(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"GET /v1/microvms/web": answer(200, map[string]any{"name": "web", "state": "created", "fenced": true, "fenceLapsedAt": "2026-11-14T03:00:00Z"}),
	})

	out, err := run(t, "microvm", "get", "--config", f.cfg, "web")

	require.NoError(t, err, out)
	assert.Contains(t, out, `"fenced": true`)
	assert.Contains(t, out, `"fenceLapsedAt": "2026-11-14T03:00:00Z"`)
}

func TestMicrovmCreate_FencedAsksForAFenceAndAFileWithFlagsIsRefused(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"POST /v1/microvms": answer(201, map[string]any{"name": "web", "state": "created", "fenced": true}),
	})
	image := "r.example/app@sha256:" + d("1")

	out, err := run(t, "microvm", "create", "--config", f.cfg, "--name", "web", "--image", image, "--fenced")
	require.NoError(t, err, out)
	assert.Equal(t, true, f.bodies["POST /v1/microvms"]["fenced"])

	_, err = run(t, "microvm", "create", "--config", f.cfg, "--name", "web", "--image", image)
	require.NoError(t, err)
	assert.NotContains(t, f.bodies["POST /v1/microvms"], "fenced", "control: a microVM has no fence unless it is asked to")

	body := filepath.Join(t.TempDir(), "r.json")
	require.NoError(t, os.WriteFile(body, []byte(`{"name":"web","image":"`+image+`","cpu":1,"memory":1024}`), 0o600))
	_, err = run(t, "microvm", "create", "--config", f.cfg, "-f", body, "--fenced")
	assert.Error(t, err, "-f carries the whole request")
}

func TestMicrovm_AHostRefusalNamesItsFields(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"POST /v1/microvms": answer(422, map[string]any{"error": "the request is invalid", "fields": []string{"memory"}}),
	})

	out, err := run(t, "microvm", "create", "--config", f.cfg, "--name", "web", "--image", "r.example/app@sha256:"+d("1"))

	require.Error(t, err)
	assert.Contains(t, out, "the request is invalid")
	assert.Contains(t, out, "memory")
}

func TestMicrovm_SnapshotResumeAndFenceSpeakTheHostAPI(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"POST /v1/microvms/src/snapshot":  answer(201, map[string]any{"name": "snap1", "microvm": "src"}),
		"POST /v1/snapshots/snap1/resume": answer(201, map[string]any{"name": "copy1", "state": "running"}),
		"GET /v1/microvms/src/fence":      answer(200, map[string]any{"phase": "armed", "lengthSeconds": 60, "remainingSeconds": 50, "renewal": 3}),
		"POST /v1/microvms/src/fence":     answer(200, map[string]any{"phase": "armed", "lengthSeconds": 90, "renewal": 4}),
	})

	out, err := run(t, "microvm", "snapshot", "--config", f.cfg, "src", "--name", "snap1")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"name": "snap1"`)
	assert.Equal(t, "snap1", f.bodies["POST /v1/microvms/src/snapshot"]["name"])

	out, err = run(t, "snapshot", "resume", "--config", f.cfg, "snap1", "--name", "copy1", "--network", "neta", "--pod-prefix", "10.9.0.0/24")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"name": "copy1"`)
	resume := f.bodies["POST /v1/snapshots/snap1/resume"]
	assert.Equal(t, "copy1", resume["name"])
	assert.Equal(t, "neta", resume["network"])
	assert.Equal(t, "10.9.0.0/24", resume["podPrefix"])

	out, err = run(t, "microvm", "fence", "get", "--config", f.cfg, "src")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"phase": "armed"`)

	out, err = run(t, "microvm", "fence", "renew", "--config", f.cfg, "src", "--length", "90", "--renewal", "4")
	require.NoError(t, err, out)
	assert.EqualValues(t, 90, f.bodies["POST /v1/microvms/src/fence"]["lengthSeconds"])
	assert.EqualValues(t, 4, f.bodies["POST /v1/microvms/src/fence"]["renewal"])

	_, err = run(t, "microvm", "snapshot", "--config", f.cfg, "src")
	assert.Error(t, err, "a snapshot needs a name")
	_, err = run(t, "microvm", "fence", "renew", "--config", f.cfg, "src", "--length", "90")
	assert.Error(t, err, "a renewal needs its number")
	_, err = run(t, "snapshot", "resume", "--config", f.cfg, "snap1")
	assert.Error(t, err, "a resume needs the new microVM's name")
}

func TestSnapshot_ListGetAndDelete(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"GET /v1/snapshots":         answer(200, map[string]any{"snapshots": []any{map[string]any{"name": "a", "microvm": "src", "imageDigest": "sha256:" + d("1")}}}),
		"GET /v1/snapshots/a":       answer(200, map[string]any{"name": "a", "microvm": "src", "driver": "firecracker"}),
		"DELETE /v1/snapshots/a":    answer(204, nil),
		"DELETE /v1/snapshots/gone": answer(404, map[string]any{"error": "no snapshot named gone"}),
	})

	out, err := run(t, "snapshot", "list", "--config", f.cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, "NAME")
	assert.Contains(t, out, "src")
	out, err = run(t, "snapshot", "get", "--config", f.cfg, "a")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"driver": "firecracker"`)
	out, err = run(t, "snapshot", "delete", "--config", f.cfg, "a")
	require.NoError(t, err, out)
	assert.Contains(t, out, "deleted snapshot a")

	out, err = run(t, "snapshot", "delete", "--config", f.cfg, "gone")
	require.Error(t, err)
	assert.Contains(t, out, "no snapshot named gone", "control: a refusal is reported, not hidden")
}

func TestHost_NetworkPeersAndTunnelSpeakTheHostAPI(t *testing.T) {
	f := newHostFake(t, map[string]http.HandlerFunc{
		"PUT /v1/networks/neta":    answer(200, map[string]any{"name": "neta", "prefix": "10.8.0.0/16", "remoteMembers": []string{"10.0.2.7/32"}}),
		"GET /v1/networks/neta":    answer(200, map[string]any{"name": "neta", "prefix": "10.8.0.0/16", "remoteMembers": []string{}}),
		"GET /v1/networks":         answer(200, map[string]any{"networks": []any{map[string]any{"name": "neta", "prefix": "10.8.0.0/16", "remoteMembers": []string{}}}}),
		"DELETE /v1/networks/neta": answer(204, nil),
		"GET /v1/peers":            answer(200, map[string]any{"peers": []any{map[string]any{"name": "h2", "publicKey": "K", "prefixes": []string{"10.1.1.0/24"}}}}),
		"PUT /v1/peers":            answer(200, map[string]any{"peers": []any{map[string]any{"name": "h2", "publicKey": "K", "prefixes": []string{"10.1.1.0/24"}}}}),
		"GET /v1/tunnel":           answer(200, map[string]any{"publicKey": "PUB", "listenPort": 51820, "machinePrefixes": []string{"10.0.0.0/24"}}),
	})

	out, err := run(t, "host", "network", "put", "--config", f.cfg, "neta", "--prefix", "10.8.0.0/16", "--remote-member", "10.0.2.7/32")
	require.NoError(t, err, out)
	put := f.bodies["PUT /v1/networks/neta"]
	assert.Equal(t, "10.8.0.0/16", put["prefix"])
	assert.Equal(t, []any{"10.0.2.7/32"}, put["remoteMembers"])
	_, err = run(t, "host", "network", "put", "--config", f.cfg, "neta")
	assert.Error(t, err, "a network needs its prefix")

	out, err = run(t, "host", "network", "get", "--config", f.cfg, "neta")
	require.NoError(t, err, out)
	assert.Contains(t, out, `"prefix": "10.8.0.0/16"`)
	out, err = run(t, "host", "network", "list", "--config", f.cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"name": "neta"`)
	out, err = run(t, "host", "network", "delete", "--config", f.cfg, "neta")
	require.NoError(t, err, out)
	assert.Contains(t, out, "deleted network neta")

	out, err = run(t, "host", "peers", "get", "--config", f.cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"publicKey": "K"`)
	list := filepath.Join(t.TempDir(), "peers.json")
	require.NoError(t, os.WriteFile(list, []byte(`{"peers":[{"name":"h2","publicKey":"K","prefixes":["10.1.1.0/24"]}]}`), 0o600))
	out, err = run(t, "host", "peers", "set", "--config", f.cfg, "-f", list)
	require.NoError(t, err, out)
	assert.Len(t, f.bodies["PUT /v1/peers"]["peers"], 1)
	require.NoError(t, os.WriteFile(list, []byte(`{"peers":[],"extra":1}`), 0o600))
	_, err = run(t, "host", "peers", "set", "--config", f.cfg, "-f", list)
	assert.Error(t, err, "a list with a field the API does not know is refused")

	out, err = run(t, "host", "tunnel", "--config", f.cfg)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"publicKey": "PUB"`)
}
