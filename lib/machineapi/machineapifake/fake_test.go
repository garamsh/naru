package machineapifake_test

import (
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
	"github.com/garamsh/naru/lib/machineapi/machineapifake"
	"github.com/garamsh/naru/lib/machineapi/machineapitest"
)

const zone = "m.example.test"

func spec() machineapi.Spec {
	return machineapi.Spec{
		Image:         "registry.example.test/app@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CPU:           1,
		MemoryBytes:   512 << 20,
		RootDiskBytes: 1 << 30,
		Volumes:       []machineapi.Volume{{Name: "data", SizeBytes: 1 << 30, MountPath: "/data"}},
		Services:      []machineapi.Service{{Name: "web", Port: 8080, Protocol: machineapi.ServiceHTTP}},
	}
}

func TestFake_PassesTheConformanceTest(t *testing.T) {
	s := machineapifake.Start(t, machineapifake.Options{
		Zone:         zone,
		AliasDomains: map[string][]string{"garam": {"workspaces.example.test"}},
		Latency:      300 * time.Millisecond,
	})
	caller, key, cert := s.ClientWithPair("garam", machineapifake.CallerRole)
	machineapitest.Run(t, machineapitest.Config{
		Caller:      caller,
		CallerKey:   key,
		CallerCert:  cert,
		Anonymous:   s.AnonymousClient(),
		Token:       func(_ *testing.T, name string) string { return s.Token(name) },
		Other:       s.Client("functions"),
		OtherRole:   s.ClientOfRole("garam", "operator"),
		ExpireLog:   func(*testing.T) { s.ExpireLog("garam") },
		Zone:        zone,
		AliasDomain: "workspaces.example.test",
		Spec:        spec(),
		Timeout:     10 * time.Second,
	})
}

func TestFake_AnswersAPathItDoesNotServeWithTheErrorBody(t *testing.T) {
	s := machineapifake.Start(t, machineapifake.Options{Zone: zone})
	_, err := s.Client("garam").Machine(t.Context(), "")
	var e *machineapi.Error
	require.True(t, errors.As(err, &e), "refused: %v", err)
	assert.Equal(t, http.StatusNotFound, e.Status)
	assert.Equal(t, machineapi.CodeNotFound, e.Code, "with a code")
}

func TestFake_RefusesACursorOlderThanTheLogKeeps(t *testing.T) {
	s := machineapifake.Start(t, machineapifake.Options{Zone: zone, LogSize: 2, Latency: time.Hour})
	c := s.Client("garam")
	start, err := c.Machines(t.Context())
	require.NoError(t, err)
	for i := range 3 {
		_, _, err := c.CreateMachine(t.Context(), "m"+strconv.Itoa(i), spec())
		require.NoError(t, err)
	}
	page, err := c.Events(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, page.Events, 2, "the log keeps its newest 2")

	_, err = c.Events(t.Context(), start.Cursor)
	var e *machineapi.Error
	require.True(t, errors.As(err, &e), "refused: %v", err)
	assert.Equal(t, http.StatusGone, e.Status)
	assert.Equal(t, machineapi.CodeCursorExpired, e.Code)

	kept, err := c.Events(t.Context(), "1")
	require.NoError(t, err, "the cursor just before the oldest event kept misses nothing")
	assert.Len(t, kept.Events, 2)
}
