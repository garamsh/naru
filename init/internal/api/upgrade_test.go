package api_test

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/api"
	"github.com/garamsh/naru/init/internal/upgrade"
)

// fakeUpgrades is the Node's upgrade: it keeps what it was given and answers with err.
type fakeUpgrades struct {
	err    error
	slot   string
	last   *upgrade.Outcome
	body   string
	sha256 string
	calls  int
}

func (f *fakeUpgrades) Install(body io.Reader, sha256 string) (upgrade.Staged, error) {
	f.calls++
	b, _ := io.ReadAll(body)
	f.body, f.sha256 = string(b), sha256
	if f.err != nil {
		return upgrade.Staged{}, f.err
	}
	return upgrade.Staged{From: "v1.2.3", To: "v2"}, nil
}

func (f *fakeUpgrades) Slot() string { return f.slot }

func (f *fakeUpgrades) Last() (upgrade.Outcome, bool) {
	if f.last == nil {
		return upgrade.Outcome{}, false
	}
	return *f.last, true
}

// withUpgrades replaces the rig's server with one that has u as its upgrade.
func (r *rig) withUpgrades(t *testing.T, u api.Upgrades) {
	t.Helper()
	r.server = api.New(api.Deps{
		Services: &r.services, Logs: r.con, Version: "v1.2.3",
		BootID:   func() (string, error) { return "boot-1", nil },
		Uptime:   func() time.Duration { return 90 * time.Second },
		Reboot:   func() { r.rebooted <- struct{}{} },
		Upgrades: u,
		Log:      slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	r.start(t)
}

func TestServer_UpgradePassesTheBodyAndTheDigestOnAndAnswers202ThenReboots(t *testing.T) {
	r := newRig(t)
	u := &fakeUpgrades{slot: "default"}
	r.withUpgrades(t, u)

	status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/upgrade?sha256=abc123", "the bundle")

	require.Equal(t, http.StatusAccepted, status, string(body))
	assert.Equal(t, "the bundle", u.body)
	assert.Equal(t, "abc123", u.sha256)
	assert.Equal(t, map[string]any{"actions": []any{
		map[string]any{"action": "upgrade", "target": "v2"},
		map[string]any{"action": "reboot", "target": "node"},
	}}, decode(t, body))
	r.waitReboot(t)
}

func TestServer_UpgradeThatIsRefusedAnswersTheStatusOfTheReasonAndDoesNotReboot(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
	}{
		"a bundle that is wrong":       {fmt.Errorf("%w: the SHA-256 differs", upgrade.ErrBundle), http.StatusBadRequest},
		"a bundle that is too large":   {fmt.Errorf("%w: over the cap", upgrade.ErrTooLarge), http.StatusRequestEntityTooLarge},
		"a Node that cannot take one":  {fmt.Errorf("%w: a trial is running", upgrade.ErrBusy), http.StatusConflict},
		"a failure of the Node itself": {errors.New("mount the ESP /dev/vda1: secret detail"), http.StatusInternalServerError},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.withUpgrades(t, &fakeUpgrades{err: tt.err})

			status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/upgrade?sha256=abc", "x")

			assert.Equal(t, tt.status, status)
			assert.NotEmpty(t, decode(t, body)["error"])
			assert.NotContains(t, string(body), "secret detail", "a failure of the Node is left to its log")
			r.noReboot(t)
		})
	}
}

func TestServer_UpgradeIsForAnAdmin(t *testing.T) {
	tests := map[string]struct {
		organizations []string
		status        int
	}{
		"admin":   {[]string{"naru:admin"}, http.StatusAccepted},
		"reader":  {[]string{"naru:reader"}, http.StatusForbidden},
		"no role": {nil, http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			u := &fakeUpgrades{}
			r.withUpgrades(t, u)

			status, _ := r.do(t, r.client(t, r.clientCA, tt.organizations...), http.MethodPost, "/v1/upgrade?sha256=abc", "x")

			assert.Equal(t, tt.status, status)
			if tt.status == http.StatusAccepted {
				assert.Equal(t, 1, u.calls)
				r.waitReboot(t)
			} else {
				assert.Zero(t, u.calls, "a refused client's body is not read")
				r.noReboot(t)
			}
		})
	}
}

func TestServer_UpgradeIs501OnANodeWithoutOne(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/upgrade?sha256=abc", "x")

	assert.Equal(t, http.StatusNotImplemented, status)
	r.noReboot(t)
}

func TestServer_UpgradeIsRefusedOnceARebootIsAccepted(t *testing.T) {
	r := newRig(t)
	u := &fakeUpgrades{}
	r.withUpgrades(t, u)
	c := r.admin(t)
	status, _ := r.do(t, c, http.MethodPost, "/v1/reboot", "")
	require.Equal(t, http.StatusAccepted, status, "the control: a reboot is accepted")
	r.waitReboot(t)

	status, _ = r.do(t, c, http.MethodPost, "/v1/upgrade?sha256=abc", "x")

	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Zero(t, u.calls)
}

func TestServer_VersionReportsTheSlotAndTheLastUpgrade(t *testing.T) {
	r := newRig(t)
	last := &upgrade.Outcome{From: "v1", To: "v2", Result: "rolled back", Reason: "the boot sequence refused", Time: "2030-01-02T03:04:05Z"}
	r.withUpgrades(t, &fakeUpgrades{slot: "trial", last: last})

	status, body := r.do(t, r.client(t, r.clientCA, "naru:reader"), http.MethodGet, "/v1/version", "")

	require.Equal(t, http.StatusOK, status)
	got := decode(t, body)
	assert.Equal(t, "trial", got["slot"])
	assert.Equal(t, map[string]any{"from": "v1", "to": "v2", "result": "rolled back", "reason": "the boot sequence refused", "time": "2030-01-02T03:04:05Z"}, got["lastUpgrade"])
}
