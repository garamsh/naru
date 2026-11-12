package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/apply"
)

// waitReboot returns once Deps.Reboot has been called, and fails when it is not.
func (r *rig) waitReboot(t *testing.T) {
	t.Helper()
	select {
	case <-r.rebooted:
	case <-time.After(5 * time.Second):
		t.Fatal("init was not asked to reboot")
	}
}

// noReboot asserts that Deps.Reboot was not called.
func (r *rig) noReboot(t *testing.T) {
	t.Helper()
	select {
	case <-r.rebooted:
		t.Fatal("init was asked to reboot")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestServer_RebootAnswers202BeforeInitIsAskedToReboot(t *testing.T) {
	r := newRig(t)
	r.start(t)
	// Deps.Reboot is held until the client has the answer. A server that asks first never lets
	// the handler return, so the client would wait on an answer that is still buffered.
	answered := make(chan struct{})
	r.onReboot = func() {
		select {
		case <-answered:
		case <-time.After(5 * time.Second):
			t.Error("init was asked to reboot before the client had the answer")
		}
	}
	c := r.admin(t)
	req, err := http.NewRequest(http.MethodPost, r.url("/v1/reboot"), nil)
	require.NoError(t, err)

	resp, err := c.Do(req)
	require.NoError(t, err)
	close(answered)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusAccepted, resp.StatusCode)
	assert.True(t, resp.Close, "the answer ends the connection")
	r.waitReboot(t)
}

func TestServer_RebootNamesTheActionItTook(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/reboot", "")

	require.Equal(t, http.StatusAccepted, status)
	assert.Equal(t, map[string]any{"actions": []any{map[string]any{"action": "reboot", "target": "node"}}}, decode(t, body))
	r.waitReboot(t)
}

func TestServer_RebootIsForAnAdminAndIsRefusedToEveryoneElseWithoutRebooting(t *testing.T) {
	tests := map[string]struct {
		organizations []string
		status        int
	}{
		"admin":    {[]string{"naru:admin"}, http.StatusAccepted},
		"reader":   {[]string{"naru:reader"}, http.StatusForbidden},
		"no role":  {[]string{"someone-else"}, http.StatusForbidden},
		"no group": {nil, http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.start(t)

			status, _ := r.do(t, r.client(t, r.clientCA, tt.organizations...), http.MethodPost, "/v1/reboot", "")

			assert.Equal(t, tt.status, status)
			if tt.status == http.StatusAccepted {
				r.waitReboot(t)
			} else {
				r.noReboot(t)
			}
		})
	}
}

func TestServer_RebootIsServedWhileTheBootHasNotFinished(t *testing.T) {
	r := newRig(t)
	require.NoError(t, r.server.Start(r.chosen))

	status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/reboot", "")

	assert.Equal(t, http.StatusAccepted, status, "a Node whose boot was refused can still be rebooted")
	r.waitReboot(t)
}

func TestServer_AfterARebootIsAcceptedARequestThatChangesTheNodeIsRefusedAndAReadIsNot(t *testing.T) {
	r := newRig(t)
	r.engine.result = apply.Result{Revision: 4, Mode: apply.NoReboot}
	r.start(t)
	c := r.admin(t)
	before, _ := r.do(t, c, http.MethodPost, "/v1/config", `{"revision":3,"patch":{"metadata":{"name":"x"}},"mode":"no-reboot"}`)
	require.Equal(t, http.StatusOK, before, "the control: the same apply is accepted before the reboot")
	require.Len(t, r.engine.applied(), 1)
	r.noReboot(t)

	status, _ := r.do(t, c, http.MethodPost, "/v1/reboot", "")
	require.Equal(t, http.StatusAccepted, status)
	r.waitReboot(t)

	changed, body := r.do(t, c, http.MethodPost, "/v1/config", `{"revision":4,"patch":{"metadata":{"name":"x"}},"mode":"no-reboot"}`)
	again, _ := r.do(t, c, http.MethodPost, "/v1/reboot", "")
	read, _ := r.do(t, c, http.MethodGet, "/v1/version", "")

	assert.Equal(t, http.StatusServiceUnavailable, changed)
	assert.Contains(t, string(body), "rebooting")
	assert.Equal(t, http.StatusServiceUnavailable, again)
	assert.Equal(t, http.StatusOK, read)
	assert.Len(t, r.engine.applied(), 1, "the refused apply never reaches the engine")
	r.noReboot(t)
}

func TestServer_ARebootModeApplyAnswers200WithTheActionsBeforeInitIsAskedToReboot(t *testing.T) {
	r := newRig(t)
	r.engine.result = apply.Result{Revision: 4, Mode: apply.Reboot, Reboot: true,
		Actions: []apply.Action{{Action: "store", Target: "config"}, {Action: "reboot", Target: "node"}}}
	r.start(t)
	answered := make(chan struct{})
	r.onReboot = func() {
		select {
		case <-answered:
		case <-time.After(5 * time.Second):
			t.Error("init was asked to reboot before the client had the answer")
		}
	}
	req, err := http.NewRequest(http.MethodPost, r.url("/v1/config"), strings.NewReader(`{"revision":3,"patch":{"spec":{"network":{"forwarding":["ipv4","ipv6"]}}},"mode":"reboot"}`))
	require.NoError(t, err)

	resp, err := r.admin(t).Do(req)
	require.NoError(t, err)
	close(answered)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, apply.Reboot, r.engine.applied()[0].Mode, "the mode reaches the engine")
	r.waitReboot(t)
}

func TestServer_AnApplyThatIsNotARebootDoesNotAskInitToReboot(t *testing.T) {
	tests := map[string]apply.Result{
		"a dry run in reboot mode": {Revision: 3, Mode: apply.Reboot, DryRun: true, Actions: []apply.Action{{Action: "store", Target: "config"}, {Action: "reboot", Target: "node"}}},
		"a staged apply":           {Revision: 4, Mode: apply.Staged},
		"a no-reboot apply":        {Revision: 4, Mode: apply.NoReboot},
	}
	for name, res := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.engine.result = res
			r.start(t)

			status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{"metadata":{"name":"x"}},"mode":"staged"}`)

			assert.Equal(t, http.StatusOK, status)
			r.noReboot(t)
			again, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{"metadata":{"name":"x"}},"mode":"staged"}`)
			assert.Equal(t, http.StatusOK, again, "the Node still takes changes")
		})
	}
}

func TestServer_ARefusedRebootModeApplyAsksForNoReboot(t *testing.T) {
	r := newRig(t)
	r.engine.err = &apply.StaleError{Stored: 5, Requested: 3}
	r.start(t)

	status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{"metadata":{"name":"x"}},"mode":"reboot"}`)

	assert.Equal(t, http.StatusConflict, status)
	r.noReboot(t)
}
