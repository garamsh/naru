package api_test

import (
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/api"
	"github.com/garamsh/naru/lib/init/upgrade"
)

func TestServer_ConfigCarriesTheStatusBesideTheSpec(t *testing.T) {
	r := newRig(t)
	r.withUpgrades(t, &fakeUpgrades{slot: "trial", last: &upgrade.Outcome{From: "v1.2.2", To: "v1.2.3", Result: "promoted", Time: "2026-11-14T01:02:03Z"}})

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	require.Equal(t, http.StatusOK, status, string(body))
	doc := decode(t, body)["config"].(map[string]any)
	assert.Contains(t, doc, "spec", "the document is the configuration and its status")
	assert.Equal(t, map[string]any{
		"version":     "v1.2.3",
		"bootId":      "boot-1",
		"slot":        "trial",
		"lastUpgrade": map[string]any{"from": "v1.2.2", "to": "v1.2.3", "result": "promoted", "time": "2026-11-14T01:02:03Z"},
		"revision":    float64(3),
		"services": []any{
			map[string]any{"name": "mvmd", "state": "running", "restarts": float64(2)},
			map[string]any{"name": "gaon", "state": "stopped", "restarts": float64(0)},
		},
	}, doc["status"])
}

func TestServer_StatusOmitsAnUpgradeThatNeverHappenedAndNamesTheDefaultSlot(t *testing.T) {
	r := newRig(t)
	r.start(t)

	_, body := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	st := decode(t, body)["config"].(map[string]any)["status"].(map[string]any)
	assert.Equal(t, "default", st["slot"])
	assert.NotContains(t, st, "lastUpgrade")
}

func TestServer_StatusAgreesWithVersion(t *testing.T) {
	r := newRig(t)
	r.withUpgrades(t, &fakeUpgrades{slot: "default", last: &upgrade.Outcome{From: "a", To: "b", Result: "rolled-back", Reason: "unhealthy", Time: "t"}})

	_, cfgBody := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")
	_, verBody := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	st := decode(t, cfgBody)["config"].(map[string]any)["status"].(map[string]any)
	ver := decode(t, verBody)
	for _, k := range []string{"version", "bootId", "slot", "lastUpgrade"} {
		assert.Equal(t, ver[k], st[k], k)
	}
}

func TestServer_ConfigIs500WhenTheBootIDCannotBeRead(t *testing.T) {
	r := newRig(t)
	r.server = api.New(api.Deps{
		Services: &r.services, Logs: r.con, Version: "v1",
		BootID: func() (string, error) { return "", errors.New("no boot id") },
		Uptime: func() time.Duration { return 0 },
		Reboot: func() {},
		Log:    slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	r.start(t)

	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	assert.Equal(t, http.StatusInternalServerError, status)
}

func TestServer_AnApplyNamingStatusIs422AndNeverReachesTheEngine(t *testing.T) {
	for name, body := range map[string]string{
		"patch":  `{"revision":3,"patch":{"status":{"version":"v9"}},"mode":"no-reboot"}`,
		"config": `{"revision":3,"config":{"apiVersion":"x","status":{}},"mode":"no-reboot"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.start(t)

			status, resp := r.do(t, r.admin(t), http.MethodPost, "/v1/config", body)

			assert.Equal(t, http.StatusUnprocessableEntity, status, string(resp))
			assert.Equal(t, []any{"status"}, decode(t, resp)["fields"])
			assert.Empty(t, r.engine.applied())

			control, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{"metadata":{"name":"n"}},"mode":"no-reboot"}`)
			assert.Equal(t, http.StatusOK, control, "the same request without status reaches the engine")
			assert.Len(t, r.engine.applied(), 1)
		})
	}
}
