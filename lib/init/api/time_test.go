package api_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/init/timesync"
)

func TestServer_TimeReportsANodeThatKeepsNoTimeBeforeTheClockIsGiven(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/time", "")

	require.Equal(t, http.StatusOK, status)
	got := decode(t, body)
	assert.Equal(t, false, got["configured"])
	assert.Equal(t, []any{}, got["servers"], "an empty list, not null")
	assert.Equal(t, false, got["synchronised"])
	assert.NotContains(t, got, "lastSync")
	assert.NotContains(t, got, "lastError")
}

func TestServer_TimeReportsTheServersAndTheLastSynchronisation(t *testing.T) {
	r := newRig(t)
	at := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	r.server.UseTime(func() timesync.Status {
		return timesync.Status{
			Servers: []string{"a.example:123", "b.example:123"},
			Last:    &timesync.Result{Server: "b.example:123", Offset: -1500 * time.Millisecond, Delay: 40 * time.Millisecond, Correction: timesync.Step, At: at},
		}
	})
	r.start(t)

	status, body := r.do(t, r.client(t, r.clientCA, "naru:reader"), http.MethodGet, "/v1/time", "")

	require.Equal(t, http.StatusOK, status, "a reader may read the clock")
	got := decode(t, body)
	assert.Equal(t, true, got["configured"])
	assert.Equal(t, []any{"a.example:123", "b.example:123"}, got["servers"])
	assert.Equal(t, true, got["synchronised"])
	assert.Equal(t, map[string]any{
		"at":            "2030-06-01T12:00:00Z",
		"server":        "b.example:123",
		"offsetSeconds": -1.5,
		"delaySeconds":  0.04,
		"correction":    "step",
	}, got["lastSync"])
	assert.NotContains(t, got, "lastError")
}

func TestServer_TimeReportsAFailedAttemptAndKeepsTheLastSynchronisation(t *testing.T) {
	r := newRig(t)
	at := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
	r.server.UseTime(func() timesync.Status {
		return timesync.Status{
			Servers: []string{"a.example:123"},
			Last:    &timesync.Result{Server: "a.example:123", Correction: timesync.Slew, At: at},
			Failed:  at.Add(time.Hour),
			Err:     "a.example:123: no answer",
		}
	})
	r.start(t)

	_, body := r.do(t, r.admin(t), http.MethodGet, "/v1/time", "")

	got := decode(t, body)
	assert.Equal(t, true, got["synchronised"], "an earlier synchronisation still stands")
	assert.Equal(t, "a.example:123: no answer", got["lastError"])
	assert.Equal(t, "2030-06-01T13:00:00Z", got["lastAttemptAt"])
}

func TestServer_TimeIsRefusedToAClientWithoutARole(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, _ := r.do(t, r.client(t, r.clientCA, "someone-else"), http.MethodGet, "/v1/time", "")

	assert.Equal(t, http.StatusForbidden, status)
}
