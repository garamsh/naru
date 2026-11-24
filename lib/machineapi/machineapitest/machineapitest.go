// Package machineapitest is the conformance test of the Machine API, machines/v1alpha1. It drives
// an implementation through machineapi.Client alone, reaches into none, and fails it where it
// departs from docs/architecture/machines.md §The API (ADR 0108 item 2).
//
// Run runs every scenario; each is also exported, so an implementation that builds the API in
// parts runs the scenarios of the parts it has. Each scenario names its Machines, snapshots and
// aliases uniquely and deletes them after, so it runs again against the same implementation.
package machineapitest

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

// Config is what the scenarios need of the implementation under test.
type Config struct {
	// Caller and Other are clients of the implementation holding the certificates of two callers.
	Caller *machineapi.Client
	Other  *machineapi.Client
	// OtherRole is a client holding a certificate of a role other than caller, such as operator.
	OtherRole *machineapi.Client
	// Zone is the zone the implementation gives stable names under, such as "m.garam.sh".
	Zone string
	// AliasDomain is a domain outside the zone the operator granted Caller, and not Other.
	AliasDomain string
	// Spec is a spec the implementation runs: at least one volume and one service, and an image
	// whose guest runs "cat", reading stdin to stdout.
	Spec machineapi.Spec
	// ExpireLog makes the implementation drop every event Caller's log holds, as the log's age
	// would, so a cursor given before it is older than the log keeps.
	ExpireLog func(t *testing.T)
	// Timeout bounds each wait for a phase or for a Machine to be gone. Zero is 2 minutes.
	Timeout time.Duration
	// Anonymous is a client of the implementation that presents no certificate, and Token makes a
	// registration token for a caller name, as root asks the signer for one.
	Anonymous *machineapi.Client
	Token     func(t *testing.T, name string) string
	// CallerKey and CallerCert are the pair Caller presents, which must be in the last 30 days of
	// its life, so that it renews.
	CallerKey  crypto.Signer
	CallerCert *x509.Certificate
}

// Run runs every scenario against the implementation c reaches.
func Run(t *testing.T, c Config) {
	for _, s := range []struct {
		name string
		run  func(*testing.T, Config)
	}{
		{"Create", Create},
		{"StartStop", StartStop},
		{"Delete", Delete},
		{"Snapshots", Snapshots},
		{"Restore", Restore},
		{"Isolation", Isolation},
		{"Events", Events},
		{"CursorExpired", CursorExpired},
		{"OtherRole", OtherRole},
		{"Secrets", Secrets},
		{"RunningOnly", RunningOnly},
		{"Aliases", Aliases},
		{"Credentials", Credentials},
	} {
		t.Run(s.name, func(t *testing.T) { s.run(t, c) })
	}
}

func (c Config) check(t *testing.T) Config {
	t.Helper()
	require.NotNil(t, c.Caller, "Config.Caller")
	require.NotNil(t, c.Other, "Config.Other")
	require.NotEmpty(t, c.Zone, "Config.Zone")
	require.NotEmpty(t, c.Spec.Volumes, "Config.Spec has a volume")
	require.NotEmpty(t, c.Spec.Services, "Config.Spec has a service")
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Minute
	}
	return c
}

// unique returns prefix with a random suffix, a DNS label no earlier run used.
func unique(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// create creates the Machine name with spec through cl, asserts it was made by this call, and
// deletes it when the test ends.
func create(t *testing.T, cl *machineapi.Client, name string, spec machineapi.Spec) machineapi.Machine {
	t.Helper()
	m, created, err := cl.CreateMachine(t.Context(), name, spec)
	require.NoError(t, err, "create %s", name)
	require.True(t, created, "create %s answered 201", name)
	t.Cleanup(func() { _ = cl.DeleteMachine(context.Background(), name) })
	return m
}

// refused asserts that err is the Machine API's refusal with status and code.
func refused(t *testing.T, err error, status int, code machineapi.Code, what string) {
	t.Helper()
	var e *machineapi.Error
	require.True(t, errors.As(err, &e), "%s: refused with a Machine API error, got %v", what, err)
	require.Equal(t, status, e.Status, "%s: status (%s)", what, e.Message)
	require.Equal(t, code, e.Code, "%s: code (%s)", what, e.Message)
}

// waitPhase waits until the caller's Machine name shows phase, and returns it as it then is.
func waitPhase(t *testing.T, c Config, cl *machineapi.Client, name string, phase machineapi.Phase) machineapi.Machine {
	t.Helper()
	var m machineapi.Machine
	var err error
	deadline := time.Now().Add(c.Timeout)
	for time.Now().Before(deadline) {
		if m, err = cl.Machine(t.Context(), name); err == nil && m.Status.Phase == phase {
			return m
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.FailNow(t, "timed out", "the Machine %s did not reach %s within %s: last %s (%s), %v", name, phase, c.Timeout, m.Status.Phase, m.Status.Reason, err)
	return m
}

// waitGone waits until the caller's Machine name is not found.
func waitGone(t *testing.T, c Config, cl *machineapi.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(c.Timeout)
	var err error
	for time.Now().Before(deadline) {
		_, err = cl.Machine(t.Context(), name)
		var e *machineapi.Error
		if errors.As(err, &e) && e.Status == http.StatusNotFound {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.FailNow(t, "timed out", "the Machine %s was not gone within %s: %v", name, c.Timeout, err)
}

// waitSnapshot waits until the caller's snapshot name is ready.
func waitSnapshot(t *testing.T, c Config, cl *machineapi.Client, name string) {
	t.Helper()
	deadline := time.Now().Add(c.Timeout)
	var s machineapi.Snapshot
	var err error
	for time.Now().Before(deadline) {
		if s, err = cl.Snapshot(t.Context(), name); err == nil && s.Phase == machineapi.SnapshotReady {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	require.FailNow(t, "timed out", "the snapshot %s was not ready within %s: last %s (%s), %v", name, c.Timeout, s.Phase, s.Reason, err)
}
