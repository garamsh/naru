package api

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/identity"
)

// resetEnv is a server whose reset is configured: the marker file, the host ID, and a Reboot that
// reports on rebooted. check, when not nil, is the CheckPair the server calls before it clears the disk.
type resetEnv struct {
	*env
	marker     string
	rebooted   chan struct{}
	failReboot bool
}

func startReset(t *testing.T, check func(context.Context) error, list *identity.RevocationList) *resetEnv {
	t.Helper()
	re := &resetEnv{marker: filepath.Join(t.TempDir(), "reset.json"), rebooted: make(chan struct{}, 1)}
	re.env = startEnvRevoking(t, netip.MustParseAddr("10.1.0.1"), true, list, func(d *Deps) {
		d.HostID, d.ResetMarker, d.CheckPair = "host-1", re.marker, check
		d.Reboot = func() error {
			if re.failReboot {
				return errors.New("no")
			}
			re.rebooted <- struct{}{}
			return nil
		}
	})
	return re
}

func (re *resetEnv) markerScope(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(re.marker)
	require.NoError(t, err)
	var m resetMarker
	require.NoError(t, json.Unmarshal(b, &m))
	return m.Scope
}

func TestReset_RecordsTheScopeForInitAndRebootsAfterTheAnswer(t *testing.T) {
	for _, scope := range []string{hostapi.ResetStore, hostapi.ResetData, hostapi.ResetAll} {
		t.Run(scope, func(t *testing.T) {
			re := startReset(t, nil, nil)

			code, b := re.do(t, re.as(t, ProvisionerOrganization), "POST", "/v1/reset", hostapi.ResetRequest{Scope: scope, Confirm: "host-1"})

			require.Equal(t, 202, code, string(b))
			assert.Equal(t, scope, re.markerScope(t))
			select {
			case <-re.rebooted:
			case <-time.After(5 * time.Second):
				t.Fatal("the host did not reboot")
			}
		})
	}
}

func TestReset_RefusesWhatItCannotCarryOutAndLeavesNoMarker(t *testing.T) {
	re := startReset(t, nil, nil)
	prov := re.as(t, ProvisionerOrganization)
	cases := []struct {
		name string
		who  func() (int, []byte)
		code int
	}{
		{"a reader", func() (int, []byte) {
			return re.do(t, re.as(t, ReaderOrganization), "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store", Confirm: "host-1"})
		}, 403},
		{"another host's ID", func() (int, []byte) {
			return re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store", Confirm: "host-2"})
		}, 422},
		{"no confirmation", func() (int, []byte) {
			return re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store"})
		}, 422},
		{"an unknown scope", func() (int, []byte) {
			return re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: "everything", Confirm: "host-1"})
		}, 422},
		{"an unknown field", func() (int, []byte) {
			return re.do(t, prov, "POST", "/v1/reset", map[string]any{"scope": "store", "confirm": "host-1", "force": true})
		}, 400},
	}
	for _, c := range cases {
		code, _ := c.who()
		assert.Equal(t, c.code, code, c.name)
	}
	_, err := os.Stat(re.marker)
	assert.True(t, os.IsNotExist(err), "a refused reset left a marker")
	assert.Empty(t, re.rebooted, "a refused reset rebooted")

	// The control: the same call from a provisioner with the right ID is accepted.
	code, _ := re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store", Confirm: "host-1"})
	assert.Equal(t, 202, code)
}

func TestReset_AHostStartedWithoutOneAnswers501(t *testing.T) {
	e := start(t)
	code, _ := e.do(t, e.as(t, ProvisionerOrganization), "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store", Confirm: "x"})
	assert.Equal(t, 501, code)
}

func TestReset_ARebootThatFailsRemovesTheMarker(t *testing.T) {
	re := startReset(t, nil, nil)
	re.failReboot = true

	code, _ := re.do(t, re.as(t, ProvisionerOrganization), "POST", "/v1/reset", hostapi.ResetRequest{Scope: "store", Confirm: "host-1"})

	require.Equal(t, 202, code)
	require.Eventually(t, func() bool { _, err := os.Stat(re.marker); return os.IsNotExist(err) }, 5*time.Second, 20*time.Millisecond,
		"a reset that did not reboot stayed recorded, and the next boot for another reason would carry it out")
}

func TestReset_AScopeThatClearsTheDiskIsRefusedWhileThePairThatWouldServeIsRevoked(t *testing.T) {
	var verdict error
	re := startReset(t, func(context.Context) error { return verdict }, nil)
	prov := re.as(t, ProvisionerOrganization)

	// The control: while the pair is not revoked every scope is accepted.
	code, b := re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: hostapi.ResetAll, Confirm: "host-1"})
	require.Equal(t, 202, code, string(b))
	require.NoError(t, os.Remove(re.marker))

	verdict = errors.Join(errors.New("the certificate in the configuration's pair is on the revocation list"), identity.ErrRevoked)
	for _, scope := range []string{hostapi.ResetData, hostapi.ResetAll} {
		code, b = re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: scope, Confirm: "host-1"})
		require.Equal(t, 409, code, scope)
		var eb hostapi.ErrorBody
		require.NoError(t, json.Unmarshal(b, &eb))
		assert.Equal(t, hostapi.CodeRevokedPair, eb.Code, scope)
	}
	_, err := os.Stat(re.marker)
	assert.True(t, os.IsNotExist(err), "a refused reset left a marker")

	// The store scope leaves the pair on disk, so it is not refused.
	code, _ = re.do(t, prov, "POST", "/v1/reset", hostapi.ResetRequest{Scope: hostapi.ResetStore, Confirm: "host-1"})
	assert.Equal(t, 202, code)
}

func TestMaintenanceHandler_ServesOnlyTheReportAndTheReset(t *testing.T) {
	srv := New(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: prometheus.NewRegistry(), HostID: "host-1"})
	h := srv.MaintenanceHandler(MaintenanceInfo{HostID: "host-1", Reason: "refused"})
	e := serveHandler(t, h)

	code, b := e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/maintenance", nil)
	require.Equal(t, 200, code)
	var m hostapi.Maintenance
	require.NoError(t, json.Unmarshal(b, &m))
	assert.Equal(t, hostapi.Maintenance{HostID: "host-1", Reason: "refused"}, m)

	code, _ = e.do(t, e.as(t, ReaderOrganization), "GET", "/v1/microvms", nil)
	assert.Equal(t, 404, code, "a microVM call has no runtime behind it")
	code, _ = e.do(t, e.as(t, "other"), "GET", "/v1/maintenance", nil)
	assert.Equal(t, 403, code, "the role is checked here as everywhere")
}

// serveHandler serves h on a listener of its own, as a maintenance host does, with a client CA the
// returned env issues from.
func serveHandler(t *testing.T, h http.Handler) *env {
	t.Helper()
	ca, srvCA := newAuthority(t, "clients"), newAuthority(t, "servers")
	pair, _, _ := srvCA.issue(t, nil, true)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	e := &env{addr: ln.Addr().String(), ca: ca, pool: x509.NewCertPool()}
	e.pool.AddCert(srvCA.cert)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	srv := New(Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: prometheus.NewRegistry()})
	go func() {
		_ = srv.ServeHandler(ctx, ln, ServerTLS(func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &pair, nil }, pool, nil), h)
	}()
	return e
}

func TestHost_ReportsTheIDAResetConfirms(t *testing.T) {
	re := startReset(t, nil, nil)

	code, b := re.do(t, re.as(t, ReaderOrganization), "GET", "/v1/host", nil)

	require.Equal(t, 200, code)
	var h hostapi.Host
	require.NoError(t, json.Unmarshal(b, &h))
	assert.Equal(t, "host-1", h.HostID)
}
