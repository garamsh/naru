package api

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

func fencedBody() map[string]any {
	b := createBody()
	b["fenced"] = true
	return b
}

func TestFence_AFencedCreateIsNotStartedAndItsFenceIsRenewedThenItStarts(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", fencedBody())
	require.Equal(t, 201, code, string(b))
	m := microVMOf(t, b)
	assert.True(t, m.Fenced)
	assert.Equal(t, mvmd.StateCreated, m.State)
	assert.NotContains(t, e.rt.calls, "start web", "a fenced create reached the start")

	// An identical repeat is the same microVM, and does not start it either.
	code, b = e.do(t, c, "POST", "/v1/microvms", fencedBody())
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, mvmd.StateCreated, microVMOf(t, b).State)
	assert.NotContains(t, e.rt.calls, "start web")

	// The same body without the fence is another body.
	code, _ = e.do(t, c, "POST", "/v1/microvms", createBody())
	assert.Equal(t, 409, code)

	// Mvmd refuses the start until the fence is armed: 409.
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	assert.Equal(t, 409, code)

	code, b = e.do(t, c, "POST", "/v1/microvms/web/fence", hostapi.FenceRequest{LengthSeconds: 90, Renewal: 1})
	require.Equal(t, 200, code, string(b))
	var f hostapi.Fence
	require.NoError(t, json.Unmarshal(b, &f))
	assert.Equal(t, hostapi.Fence{Phase: hostapi.FenceArmed, LengthSeconds: 90, RemainingSeconds: 90, Renewal: 1}, f)
	assert.Equal(t, 90*time.Second, e.rt.fences["web"].Length, "the length did not reach mvmd")

	code, b = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	require.Equal(t, 200, code, string(b))
	assert.Equal(t, "running", microVMOf(t, b).State)
}

func TestFence_GetReportsTheFenceAndAStaleRenewalChangesNothing(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", fencedBody())
	require.Equal(t, 201, code)

	reader := e.as(t, ReaderOrganization)
	code, b := e.do(t, reader, "GET", "/v1/microvms/web/fence", nil)
	require.Equal(t, 200, code, string(b))
	var f hostapi.Fence
	require.NoError(t, json.Unmarshal(b, &f))
	assert.Equal(t, hostapi.FenceNotArmed, f.Phase)

	code, _ = e.do(t, c, "POST", "/v1/microvms/web/fence", hostapi.FenceRequest{LengthSeconds: 60, Renewal: 5})
	require.Equal(t, 200, code)
	code, b = e.do(t, c, "POST", "/v1/microvms/web/fence", hostapi.FenceRequest{LengthSeconds: 600, Renewal: 5})
	require.Equal(t, 200, code, string(b))
	require.NoError(t, json.Unmarshal(b, &f))
	assert.Equal(t, uint64(60), f.LengthSeconds, "a renewal number not above the last one changed the fence")
	assert.Equal(t, uint64(5), f.Renewal)

	code, b = e.do(t, reader, "GET", "/v1/microvms/web/fence", nil)
	require.Equal(t, 200, code, string(b))
	require.NoError(t, json.Unmarshal(b, &f))
	assert.Equal(t, hostapi.FenceArmed, f.Phase)
}

func TestFence_ErrorsAndRoles(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	reader := e.as(t, ReaderOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms", map[string]any{
		"name": "fenced", "image": "registry.example/app@" + digest, "cpu": 1, "memory": 1 << 28, "rootDiskBytes": 1 << 30, "fenced": true,
	})
	require.Equal(t, 201, code)

	req := hostapi.FenceRequest{LengthSeconds: 30, Renewal: 1}
	code, _ = e.do(t, c, "POST", "/v1/microvms/nope/fence", req)
	assert.Equal(t, 404, code, "an unknown microVM")
	code, _ = e.do(t, c, "GET", "/v1/microvms/nope/fence", nil)
	assert.Equal(t, 404, code)
	code, _ = e.do(t, c, "POST", "/v1/microvms/web/fence", req)
	assert.Equal(t, 409, code, "a microVM with no fence of its own")
	code, _ = e.do(t, c, "GET", "/v1/microvms/web/fence", nil)
	assert.Equal(t, 409, code)

	for field, body := range map[string]hostapi.FenceRequest{
		"lengthSeconds": {LengthSeconds: 0, Renewal: 1},
		"renewal":       {LengthSeconds: 30, Renewal: 0},
	} {
		code, b := e.do(t, c, "POST", "/v1/microvms/fenced/fence", body)
		assert.Equal(t, 422, code, field)
		assert.Contains(t, string(b), field)
	}
	code, _ = e.do(t, c, "POST", "/v1/microvms/fenced/fence", hostapi.FenceRequest{LengthSeconds: 1 << 40, Renewal: 1})
	assert.Equal(t, 422, code, "a length that overflows a duration")
	code, _ = e.do(t, c, "POST", "/v1/microvms/fenced/fence", map[string]any{"lengthSeconds": 30, "renewal": 1, "bogus": 1})
	assert.Equal(t, 400, code)
	assert.NotContains(t, e.rt.calls, "renew fenced", "a refused renewal reached mvmd")

	code, _ = e.do(t, reader, "POST", "/v1/microvms/fenced/fence", req)
	assert.Equal(t, 403, code, "a reader may not renew")
	assert.NotContains(t, e.rt.calls, "renew fenced")
	code, _ = e.do(t, reader, "GET", "/v1/microvms/fenced/fence", nil)
	assert.Equal(t, 200, code, "a reader may inspect")
}

func TestFence_TheViewShowsALapse(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, _ := e.do(t, c, "POST", "/v1/microvms", fencedBody())
	require.Equal(t, 201, code)
	lapsed := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	m := e.rt.machines["web"]
	m.State, m.FenceLapsedAt = mvmd.StateStopped, lapsed
	m.Failure = "the Machine's own fence lapsed and ended its VMM"
	e.rt.machines["web"] = m

	code, b := e.do(t, c, "GET", "/v1/microvms/web", nil)
	require.Equal(t, 200, code)
	v := microVMOf(t, b)
	assert.True(t, v.Fenced)
	assert.True(t, v.FenceLapsedAt.Equal(lapsed), v.FenceLapsedAt.String())
	assert.Contains(t, v.Failure, "own fence lapsed")
	assert.Contains(t, string(b), `"fenceLapsedAt":"2026-11-14T03:00:00Z"`)

	// A microVM whose fence has not lapsed shows no time.
	code, b = e.do(t, c, "POST", "/v1/microvms", map[string]any{
		"name": "plain", "image": "registry.example/app@" + digest, "cpu": 1, "memory": 1 << 28, "rootDiskBytes": 1 << 30,
	})
	require.Equal(t, 201, code, string(b))
	assert.NotContains(t, string(b), "fenceLapsedAt")
	assert.Contains(t, string(b), `"fenced":false`)
}
