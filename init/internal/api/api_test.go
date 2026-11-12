package api_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/init/internal/api"
	"github.com/garamsh/naru/init/internal/apply"
	"github.com/garamsh/naru/init/internal/config"
	"github.com/garamsh/naru/init/internal/console"
	"github.com/garamsh/naru/init/internal/pkitest"
	"github.com/garamsh/naru/init/internal/service"
	"github.com/garamsh/naru/init/internal/store"
)

// lockedBuffer is a log the test reads while the server writes it.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeEngine holds a configuration and answers Apply as the test says.
type fakeEngine struct {
	mu       sync.Mutex
	revision int
	cfg      config.NodeConfig
	requests []apply.Request
	result   apply.Result
	err      error
	panics   bool
}

func (e *fakeEngine) Config() (int, config.NodeConfig) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.revision, e.cfg
}

func (e *fakeEngine) Apply(req apply.Request) (apply.Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.panics {
		panic("boom")
	}
	e.requests = append(e.requests, req)
	return e.result, e.err
}

func (e *fakeEngine) applied() []apply.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]apply.Request(nil), e.requests...)
}

type fakeServices struct{ status []service.Status }

func (f fakeServices) Status() []service.Status { return f.status }

// rig is a Node API serving on a loopback port, and the authorities of its two sides.
type rig struct {
	server   *api.Server
	con      *console.Console
	engine   *fakeEngine
	logs     *lockedBuffer
	chosen   store.Stored
	clientCA pkitest.CA
	serverCA pkitest.CA
	services fakeServices
	// rebooted receives one value for each call of Deps.Reboot, after onReboot returns.
	rebooted chan struct{}
	onReboot func()
}

var startedAt = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{
		con:      console.New(io.Discard, console.KeptBytes),
		logs:     &lockedBuffer{},
		clientCA: pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)),
		serverCA: pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)),
		rebooted: make(chan struct{}, 8),
		services: fakeServices{status: []service.Status{
			{Name: "mvmd", State: service.StateRunning, PID: 42, Restarts: 2, StartedAt: startedAt},
		}},
	}
	cert, key := pkitest.Issue(t, r.serverCA)
	r.chosen = store.Stored{Revision: 3, Config: config.NodeConfig{
		Metadata: config.Metadata{Name: "node-1"},
		Spec: config.Spec{
			Services: []config.Service{{Name: "mvmd", Binary: "/usr/bin/mvmd"}, {Name: "gaon", Binary: "/usr/bin/gaon"}},
			Files: []config.File{
				{Path: "/run/private", Mode: "0600", Content: "private-content"},
				{Path: "/run/public", Mode: "0644", Content: "public-content"},
				{Path: "/run/empty", Mode: "0600"},
			},
			PKI: config.PKI{Authorities: []config.Authority{{Name: "ca", Certificate: "ca-certificate", Key: "ca-private-key"}}},
			API: &config.API{Listen: "127.0.0.1:0", ClientCA: r.clientCA.Cert, Certificate: cert, Key: key},
		},
	}}
	r.engine = &fakeEngine{revision: r.chosen.Revision, cfg: r.chosen.Config}
	r.server = api.New(api.Deps{
		Services: &r.services,
		Logs:     r.con,
		Version:  "v1.2.3",
		BootID:   func() (string, error) { return "boot-1", nil },
		Uptime:   func() time.Duration { return 90 * time.Second },
		Reboot: func() {
			if r.onReboot != nil {
				r.onReboot()
			}
			r.rebooted <- struct{}{}
		},
		Log: slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	return r
}

// start serves r.chosen and attaches the engine.
func (r *rig) start(t *testing.T) {
	t.Helper()
	require.NoError(t, r.server.Start(r.chosen))
	r.server.Attach(r.engine)
}

// client returns a client that trusts the serving certificate and presents a certificate that
// ca signed, with the Organizations given.
func (r *rig) client(t *testing.T, ca pkitest.CA, organizations ...string) *http.Client {
	t.Helper()
	return r.clientTrusting(t, r.serverCA, ca, organizations...)
}

func (r *rig) clientTrusting(t *testing.T, serverCA, ca pkitest.CA, organizations ...string) *http.Client {
	t.Helper()
	certPEM, keyPEM := pkitest.Issue(t, ca, organizations...)
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(serverCA.Cert)))
	tr := &http.Transport{
		TLSClientConfig:   &tls.Config{Certificates: []tls.Certificate{pair}, RootCAs: pool, MinVersion: tls.VersionTLS13},
		DisableKeepAlives: true,
	}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}

func (r *rig) url(path string) string { return "https://" + r.server.Addr().String() + path }

// do sends one request and returns the status and the body.
func (r *rig) do(t *testing.T, c *http.Client, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, r.url(path), strings.NewReader(body))
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, data
}

func (r *rig) admin(t *testing.T) *http.Client {
	return r.client(t, r.clientCA, "naru:admin")
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(data, &m), string(data))
	return m
}

func TestServer_WithoutSpecAPIServesNothingAndSaysSo(t *testing.T) {
	r := newRig(t)
	r.chosen.Config.Spec.API = nil

	require.NoError(t, r.server.Start(r.chosen))

	assert.Nil(t, r.server.Addr())
	assert.Contains(t, r.logs.String(), "the Node API is off")
}

func TestServer_WithSpecAPIListensOnItsAddress(t *testing.T) {
	r := newRig(t)

	r.start(t)

	require.NotNil(t, r.server.Addr())
	assert.Contains(t, r.logs.String(), "the Node API is listening")
	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")
	assert.Equal(t, http.StatusOK, status)
}

func TestServer_RefusesAClientCertificateFromAnotherCA(t *testing.T) {
	r := newRig(t)
	r.start(t)
	stranger := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))

	_, err := r.client(t, stranger, "naru:admin").Get(r.url("/v1/version"))
	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	assert.Error(t, err, "a certificate another CA signed must fail the handshake, whatever its Organization")
	assert.Equal(t, http.StatusOK, status, "the control: the same request with the right CA succeeds")
}

func TestServer_RefusesAClientWithoutACertificate(t *testing.T) {
	r := newRig(t)
	r.start(t)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM([]byte(r.serverCA.Cert)))
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13}, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)

	_, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get(r.url("/v1/version"))

	assert.Error(t, err)
}

func TestServer_RefusesTLSOlderThan13(t *testing.T) {
	r := newRig(t)
	r.start(t)
	c := r.admin(t)
	c.Transport.(*http.Transport).TLSClientConfig.MinVersion = tls.VersionTLS12
	c.Transport.(*http.Transport).TLSClientConfig.MaxVersion = tls.VersionTLS12

	_, err := c.Get(r.url("/v1/version"))
	c13 := r.admin(t)
	status, _ := r.do(t, c13, http.MethodGet, "/v1/version", "")

	assert.Error(t, err)
	assert.Equal(t, http.StatusOK, status, "the control: TLS 1.3 is served")
}

func TestServer_RoleDecidesWhatAClientMayDo(t *testing.T) {
	const applyBody = `{"revision":3,"patch":{"metadata":{"name":"x"}}}`
	tests := map[string]struct {
		organizations []string
		get, post     int
	}{
		"admin":                  {[]string{"naru:admin"}, http.StatusOK, http.StatusOK},
		"reader":                 {[]string{"naru:reader"}, http.StatusOK, http.StatusForbidden},
		"another organization":   {[]string{"someone-else"}, http.StatusForbidden, http.StatusForbidden},
		"no organization":        {nil, http.StatusForbidden, http.StatusForbidden},
		"reader and admin":       {[]string{"naru:reader", "naru:admin"}, http.StatusOK, http.StatusOK},
		"admin and reader":       {[]string{"naru:admin", "naru:reader"}, http.StatusOK, http.StatusOK},
		"admin and another one":  {[]string{"someone-else", "naru:admin"}, http.StatusOK, http.StatusOK},
		"reader and unknown too": {[]string{"naru:reader", "someone-else"}, http.StatusOK, http.StatusForbidden},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.engine.result = apply.Result{Revision: 4, Mode: apply.NoReboot}
			r.start(t)
			c := r.client(t, r.clientCA, tt.organizations...)

			getStatus, _ := r.do(t, c, http.MethodGet, "/v1/config", "")
			postStatus, _ := r.do(t, c, http.MethodPost, "/v1/config", applyBody)

			assert.Equal(t, tt.get, getStatus, "GET")
			assert.Equal(t, tt.post, postStatus, "POST")
			assert.Len(t, r.engine.applied(), map[bool]int{true: 1, false: 0}[tt.post == http.StatusOK], "a refused apply never reaches the engine")
		})
	}
}

func TestServer_AForbiddenBodyNamesNoField(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.client(t, r.clientCA, "naru:reader"), http.MethodPost, "/v1/config", `{"revision":3}`)

	assert.Equal(t, http.StatusForbidden, status)
	assert.NotEmpty(t, decode(t, body)["error"])
}

func TestServer_VersionReportsTheVersionTheBootIDAndTheUptime(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, map[string]any{"version": "v1.2.3", "bootId": "boot-1", "uptimeSeconds": float64(90)}, decode(t, body))
}

func TestServer_VersionIs500WhenTheBootIDCannotBeRead(t *testing.T) {
	r := newRig(t)
	r.server = api.New(api.Deps{
		Services: &r.services, Logs: r.con, Version: "v1",
		BootID: func() (string, error) { return "", errors.New("no boot_id") },
		Uptime: func() time.Duration { return 0 },
		Log:    slog.New(slog.NewTextHandler(r.logs, nil)),
	})
	t.Cleanup(func() { _ = r.server.Close() })
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.NotEmpty(t, decode(t, body)["error"])
}

func TestServer_ServicesListsEachConfiguredServiceWithItsState(t *testing.T) {
	r := newRig(t)
	r.services.status = append(r.services.status, service.Status{Name: "extra", State: service.StateBackoff})
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/services", "")

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, map[string]any{"services": []any{
		map[string]any{"name": "mvmd", "state": "running", "pid": float64(42), "restarts": float64(2), "startedAt": "2030-01-02T03:04:05Z"},
		map[string]any{"name": "gaon", "state": "stopped", "pid": float64(0), "restarts": float64(0)},
		map[string]any{"name": "extra", "state": "backoff", "pid": float64(0), "restarts": float64(0)},
	}}, decode(t, body))
}

func TestServer_LogsReturnsOnlyTheLinesOfThatService(t *testing.T) {
	r := newRig(t)
	_, _ = r.con.Source("mvmd").Write([]byte("mvmd one\nmvmd two\n"))
	_, _ = r.con.Source("gaon").Write([]byte("gaon one\n"))
	_, _ = r.con.Write([]byte("init line\n"))
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/services/mvmd/logs", "")
	_, other := r.do(t, r.admin(t), http.MethodGet, "/v1/services/gaon/logs", "")

	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "mvmd one\nmvmd two\n", string(body))
	assert.Equal(t, "gaon one\n", string(other), "the control: another service's lines are served for its own name")
}

func TestServer_LogsOfAnUnknownServiceIs404(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/services/nope/logs", "")

	assert.Equal(t, http.StatusNotFound, status)
	assert.NotEmpty(t, decode(t, body)["error"])
}

func TestServer_LogsRefusesAFollowThatIsNotABoolean(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/services/mvmd/logs?follow=maybe", "")

	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, []any{"follow"}, decode(t, body)["fields"])
}

func TestServer_LogsWithFollowStreamsLinesAsTheyAreKept(t *testing.T) {
	r := newRig(t)
	_, _ = r.con.Source("mvmd").Write([]byte("before\n"))
	r.start(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url("/v1/services/mvmd/logs?follow=true"), nil)
	require.NoError(t, err)
	c := r.admin(t)
	c.Timeout = 0

	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	lines := bufio.NewScanner(resp.Body)
	next := func() string {
		got := make(chan string, 1)
		go func() {
			if lines.Scan() {
				got <- lines.Text()
			} else {
				got <- "<closed>"
			}
		}()
		select {
		case l := <-got:
			return l
		case <-time.After(5 * time.Second):
			t.Fatal("no line arrived")
			return ""
		}
	}

	assert.Equal(t, "before", next())
	_, _ = r.con.Source("gaon").Write([]byte("not for mvmd\n"))
	_, _ = r.con.Source("mvmd").Write([]byte("after\n"))
	assert.Equal(t, "after", next(), "a line another service kept is not streamed")
}

func TestServer_ConfigReportsTheRevisionAndRedactsPrivateMaterial(t *testing.T) {
	r := newRig(t)
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	assert.Equal(t, http.StatusOK, status)
	got := decode(t, body)
	assert.Equal(t, float64(3), got["revision"])
	spec := got["config"].(map[string]any)["spec"].(map[string]any)
	assert.Equal(t, "REDACTED", spec["api"].(map[string]any)["key"], "spec.api.key")
	assert.Equal(t, "REDACTED", spec["pki"].(map[string]any)["authorities"].([]any)[0].(map[string]any)["key"], "spec.pki.authorities[].key")
	files := spec["files"].([]any)
	assert.Equal(t, "REDACTED", files[0].(map[string]any)["content"], "a file no group or other can read")
	assert.Equal(t, "public-content", files[1].(map[string]any)["content"], "the control: a file group and other can read keeps its content")
	assert.Equal(t, "", files[2].(map[string]any)["content"], "an empty file holds nothing to hide")
	assert.Equal(t, "ca-certificate", spec["pki"].(map[string]any)["authorities"].([]any)[0].(map[string]any)["certificate"], "a certificate is not private")
	assert.NotContains(t, string(body), "ca-private-key")
	assert.NotContains(t, string(body), "private-content")
	assert.Equal(t, "node-1", got["config"].(map[string]any)["metadata"].(map[string]any)["name"])
}

func TestServer_ConfigRedactionLeavesTheEnginesConfigurationAlone(t *testing.T) {
	r := newRig(t)
	r.start(t)

	r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	_, held := r.engine.Config()
	assert.Equal(t, "private-content", held.Spec.Files[0].Content)
	assert.Equal(t, "ca-private-key", held.Spec.PKI.Authorities[0].Key)
	assert.Equal(t, r.chosen.Config.Spec.API.Key, held.Spec.API.Key)
}

func TestServer_PostPassesTheRequestToTheEngineAndReportsItsActions(t *testing.T) {
	r := newRig(t)
	r.engine.result = apply.Result{Revision: 4, Mode: apply.NoReboot, DryRun: true, Actions: []apply.Action{
		{Action: "store", Target: "config"}, {Action: "restart", Target: "mvmd"},
	}}
	r.start(t)

	status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/config",
		`{"revision":3,"patch":{"spec":{"controlPlane":true}},"mode":"no-reboot","dryRun":true}`)

	assert.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, map[string]any{
		"revision": float64(4), "mode": "no-reboot", "dryRun": true,
		"actions": []any{
			map[string]any{"action": "store", "target": "config"},
			map[string]any{"action": "restart", "target": "mvmd"},
		},
	}, decode(t, body))
	require.Len(t, r.engine.applied(), 1)
	got := r.engine.applied()[0]
	assert.Equal(t, 3, got.Revision)
	assert.Equal(t, map[string]any{"spec": map[string]any{"controlPlane": true}}, got.Patch)
	assert.Equal(t, apply.NoReboot, got.Mode)
	assert.True(t, got.DryRun)
}

func TestServer_PostWithNoActionsReportsAnEmptyList(t *testing.T) {
	r := newRig(t)
	r.engine.result = apply.Result{Revision: 3, Mode: apply.Staged}
	r.start(t)

	_, body := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"config":{},"mode":"staged"}`)

	assert.Equal(t, []any{}, decode(t, body)["actions"])
}

func TestServer_PostMapsEachEngineErrorToItsStatus(t *testing.T) {
	tests := map[string]struct {
		err    error
		status int
		check  func(*testing.T, map[string]any)
	}{
		"stale revision": {&apply.StaleError{Stored: 5, Requested: 3}, http.StatusConflict, func(t *testing.T, b map[string]any) {
			assert.Equal(t, float64(5), b["revision"])
		}},
		"invalid request": {&apply.InvalidError{Fields: []string{"spec.services[0].binary"}, Message: "is required"}, http.StatusUnprocessableEntity, func(t *testing.T, b map[string]any) {
			assert.Equal(t, []any{"spec.services[0].binary"}, b["fields"])
		}},
		"a live action failed": {&apply.FailedError{
			Revision: 4, Done: []apply.Action{{Action: "store", Target: "config"}},
			Failed: apply.Action{Action: "restart", Target: "mvmd"}, Err: errors.New("no such service"),
		}, http.StatusInternalServerError, func(t *testing.T, b map[string]any) {
			assert.Equal(t, float64(4), b["revision"])
			assert.Equal(t, []any{map[string]any{"action": "store", "target": "config"}}, b["actions"])
			assert.Equal(t, map[string]any{"action": "restart", "target": "mvmd"}, b["failed"])
		}},
		"the store failed": {&apply.StoreError{Err: errors.New("disk full")}, http.StatusInternalServerError, func(t *testing.T, b map[string]any) {
			assert.Contains(t, b["error"], "disk full")
		}},
		"anything else": {errors.New("surprise"), http.StatusInternalServerError, func(t *testing.T, b map[string]any) {
			assert.Contains(t, b["error"], "surprise")
		}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.engine.err = tt.err
			r.start(t)

			status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{}}`)

			assert.Equal(t, tt.status, status, string(body))
			got := decode(t, body)
			assert.NotEmpty(t, got["error"])
			tt.check(t, got)
		})
	}
}

func TestServer_PostRefusesABodyThatIsNotAnApplyRequestWith400(t *testing.T) {
	tests := map[string]string{
		"not JSON":          `revision: 3`,
		"no revision":       `{"patch":{}}`,
		"an unknown field":  `{"revision":3,"patch":{},"mood":"hopeful"}`,
		"two documents":     `{"revision":3,"patch":{}}{"revision":3}`,
		"a revision string": `{"revision":"3"}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.start(t)

			status, got := r.do(t, r.admin(t), http.MethodPost, "/v1/config", body)

			assert.Equal(t, http.StatusBadRequest, status, string(got))
			assert.NotEmpty(t, decode(t, got)["error"])
			assert.Empty(t, r.engine.applied())
		})
	}
}

func TestServer_PostAnswers500WhileNoEngineIsAttached(t *testing.T) {
	r := newRig(t)
	require.NoError(t, r.server.Start(r.chosen))

	status, body := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{}}`)
	getStatus, getBody := r.do(t, r.admin(t), http.MethodGet, "/v1/config", "")

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.NotEmpty(t, decode(t, body)["error"])
	assert.Equal(t, http.StatusOK, getStatus, "reads are served: the Node holds a refusal")
	assert.Equal(t, float64(3), decode(t, getBody)["revision"], "the configuration reported is the one chosen")
}

func TestServer_AHandlerThatPanicsAnswers500AndTheServerKeepsServing(t *testing.T) {
	r := newRig(t)
	r.start(t)
	r.engine.panics = true

	status, _ := r.do(t, r.admin(t), http.MethodPost, "/v1/config", `{"revision":3,"patch":{}}`)
	after, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, http.StatusOK, after)
}

func TestServer_LogsEachRequestWithATraceID(t *testing.T) {
	r := newRig(t)
	r.start(t)

	r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	line := r.logs.String()
	assert.Contains(t, line, "trace_id=")
	assert.Contains(t, line, "path=/v1/version")
	assert.Contains(t, line, "status=200")
}

func TestServer_ReloadSwapsTheServingPairAndTheClientCAWithoutClosingTheListener(t *testing.T) {
	r := newRig(t)
	r.start(t)
	addr := r.server.Addr().String()
	oldClient := r.admin(t)
	status, _ := r.do(t, oldClient, http.MethodGet, "/v1/version", "")
	require.Equal(t, http.StatusOK, status)

	newClientCA := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	newServerCA := pkitest.NewCA(t, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	cert, key := pkitest.Issue(t, newServerCA)
	require.NoError(t, r.server.Reload(config.API{ClientCA: newClientCA.Cert, Certificate: cert, Key: key}))

	_, oldErr := r.admin(t).Get(r.url("/v1/version"))
	_, oldServerErr := r.clientTrusting(t, r.serverCA, newClientCA, "naru:admin").Get(r.url("/v1/version"))
	newStatus, _ := r.do(t, r.clientTrusting(t, newServerCA, newClientCA, "naru:admin"), http.MethodGet, "/v1/version", "")

	assert.Error(t, oldErr, "a certificate from the old client CA no longer passes")
	assert.Error(t, oldServerErr, "a client that trusts only the old serving certificate no longer connects")
	assert.Equal(t, http.StatusOK, newStatus, "the new pair and client CA are served on the same listener")
	assert.Equal(t, addr, r.server.Addr().String())
}

func TestServer_ReloadKeepsTheOldPairWhenTheNewOneIsInvalid(t *testing.T) {
	r := newRig(t)
	r.start(t)

	err := r.server.Reload(config.API{ClientCA: r.clientCA.Cert, Certificate: "nope", Key: "nope"})
	status, _ := r.do(t, r.admin(t), http.MethodGet, "/v1/version", "")

	assert.Error(t, err)
	assert.Equal(t, http.StatusOK, status)
}

func TestServer_ReloadWithNoListenerIsAnError(t *testing.T) {
	r := newRig(t)
	r.chosen.Config.Spec.API = nil
	require.NoError(t, r.server.Start(r.chosen))

	err := r.server.Reload(config.API{})

	assert.Error(t, err)
}

func TestServer_StartFailsWhenTheAddressIsTaken(t *testing.T) {
	r := newRig(t)
	r.start(t)
	other := newRig(t)
	other.chosen.Config.Spec.API.Listen = r.server.Addr().String()

	err := other.server.Start(other.chosen)

	assert.Error(t, err)
}

func TestServer_StartFailsWhenTheServingPairIsInvalid(t *testing.T) {
	r := newRig(t)
	r.chosen.Config.Spec.API.Key = "nope"

	err := r.server.Start(r.chosen)

	assert.Error(t, err)
	assert.Nil(t, r.server.Addr())
}
