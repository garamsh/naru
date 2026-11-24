package machineapi_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/machineapi"
)

// serve answers each request with status and body, and records the last one and its body.
func serve(t *testing.T, status int, body string) (*machineapi.Client, *http.Request, *string) {
	t.Helper()
	var got http.Request
	var sent string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got, sent = *r, string(b)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	c, err := machineapi.New(srv.URL, srv.Client())
	require.NoError(t, err)
	return c, &got, &sent
}

func TestClient_EachCallSendsItsMethodPathAndBody(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name         string
		call         func(c *machineapi.Client) error
		method, path string
		query, sent  string
	}{
		{"create", func(c *machineapi.Client) error {
			_, _, err := c.CreateMachine(ctx, "web", machineapi.Spec{CPU: 2, State: machineapi.StateStopped})
			return err
		}, "PUT", "/machines/v1alpha1/machines/web", "", `"cpu":2`},
		{"read", func(c *machineapi.Client) error { _, err := c.Machine(ctx, "web"); return err }, "GET", "/machines/v1alpha1/machines/web", "", ""},
		{"list", func(c *machineapi.Client) error { _, err := c.Machines(ctx); return err }, "GET", "/machines/v1alpha1/machines", "", ""},
		{"start", func(c *machineapi.Client) error { _, err := c.StartMachine(ctx, "web"); return err }, "POST", "/machines/v1alpha1/machines/web/start", "", ""},
		{"stop", func(c *machineapi.Client) error {
			_, err := c.StopMachine(ctx, "web", machineapi.StopRequest{GraceSeconds: 30})
			return err
		}, "POST", "/machines/v1alpha1/machines/web/stop", "", `{"graceSeconds":30}`},
		{"delete", func(c *machineapi.Client) error { return c.DeleteMachine(ctx, "web") }, "DELETE", "/machines/v1alpha1/machines/web", "", ""},
		{"snapshot", func(c *machineapi.Client) error {
			_, _, err := c.PutSnapshot(ctx, "s", machineapi.SnapshotRequest{Machine: "web"})
			return err
		}, "PUT", "/machines/v1alpha1/snapshots/s", "", `{"machine":"web"}`},
		{"snapshot read", func(c *machineapi.Client) error { _, err := c.Snapshot(ctx, "s"); return err }, "GET", "/machines/v1alpha1/snapshots/s", "", ""},
		{"snapshots", func(c *machineapi.Client) error { _, err := c.Snapshots(ctx); return err }, "GET", "/machines/v1alpha1/snapshots", "", ""},
		{"snapshot delete", func(c *machineapi.Client) error { return c.DeleteSnapshot(ctx, "s") }, "DELETE", "/machines/v1alpha1/snapshots/s", "", ""},
		{"exec", func(c *machineapi.Client) error {
			_, err := c.Exec(ctx, "web", machineapi.ExecRequest{Command: []string{"cat"}, Stdin: []byte("hi"), TimeoutSeconds: 5})
			return err
		}, "POST", "/machines/v1alpha1/machines/web/exec", "", `{"command":["cat"],"stdin":"aGk=","timeoutSeconds":5}`},
		{"file read", func(c *machineapi.Client) error { _, err := c.ReadFile(ctx, "web", "/data/a b"); return err }, "GET", "/machines/v1alpha1/machines/web/files", "path=%2Fdata%2Fa+b", ""},
		{"file write", func(c *machineapi.Client) error { return c.WriteFile(ctx, "web", "/data/a", []byte("raw")) }, "PUT", "/machines/v1alpha1/machines/web/files", "path=%2Fdata%2Fa", "raw"},
		{"events", func(c *machineapi.Client) error { _, err := c.Events(ctx, "42"); return err }, "GET", "/machines/v1alpha1/events", "since=42", ""},
		{"events from the start", func(c *machineapi.Client) error { _, err := c.Events(ctx, ""); return err }, "GET", "/machines/v1alpha1/events", "", ""},
		{"alias", func(c *machineapi.Client) error {
			_, _, err := c.PutAlias(ctx, "app.example.test", machineapi.AliasRequest{Machine: "web"})
			return err
		}, "PUT", "/machines/v1alpha1/aliases/app.example.test", "", `{"machine":"web"}`},
		{"alias read", func(c *machineapi.Client) error { _, err := c.Alias(ctx, "app.example.test"); return err }, "GET", "/machines/v1alpha1/aliases/app.example.test", "", ""},
		{"alias delete", func(c *machineapi.Client) error { return c.DeleteAlias(ctx, "app.example.test") }, "DELETE", "/machines/v1alpha1/aliases/app.example.test", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, got, sent := serve(t, http.StatusOK, "{}")
			require.NoError(t, tc.call(c))
			assert.Equal(t, tc.method, got.Method)
			assert.Equal(t, tc.path, got.URL.Path)
			assert.Equal(t, tc.query, got.URL.RawQuery)
			assert.Contains(t, *sent, tc.sent)
		})
	}
}

func TestClient_ReportsWhetherAPutCreated(t *testing.T) {
	for status, want := range map[int]bool{http.StatusCreated: true, http.StatusOK: false} {
		c, _, _ := serve(t, status, `{"name":"web","id":"0123456789abcdef"}`)
		m, created, err := c.CreateMachine(t.Context(), "web", machineapi.Spec{})
		require.NoError(t, err)
		assert.Equal(t, want, created, "status %d", status)
		assert.Equal(t, "0123456789abcdef", m.ID)
	}
}

func TestClient_ARefusalIsAnErrorWithItsStatusAndCode(t *testing.T) {
	c, _, _ := serve(t, http.StatusConflict, `{"error":"the Machine web exists with another spec","code":"conflict"}`)
	_, _, err := c.CreateMachine(t.Context(), "web", machineapi.Spec{})
	var e *machineapi.Error
	require.True(t, errors.As(err, &e))
	assert.Equal(t, &machineapi.Error{Status: 409, Message: "the Machine web exists with another spec", Code: machineapi.CodeConflict}, e)

	c, _, _ = serve(t, http.StatusConflict, `{"error":"later","code":"a-code-of-a-later-version"}`)
	_, err = c.Machine(t.Context(), "web")
	require.True(t, errors.As(err, &e))
	assert.Equal(t, machineapi.Code("a-code-of-a-later-version"), e.Code, "an unknown code is kept as it is")

	c, _, _ = serve(t, http.StatusBadGateway, "upstream down\n")
	_, err = c.Machine(t.Context(), "web")
	require.True(t, errors.As(err, &e))
	assert.Equal(t, &machineapi.Error{Status: 502, Message: "upstream down"}, e, "a body that is not the error's JSON is the message")
}

func TestClient_ReadsAFileAsItsBytes(t *testing.T) {
	c, _, _ := serve(t, http.StatusOK, "\x00not json")
	b, err := c.ReadFile(t.Context(), "web", "/data/a")
	require.NoError(t, err)
	assert.Equal(t, []byte("\x00not json"), b)
}

func TestNew_RefusesAURLThatIsNotHTTPS(t *testing.T) {
	_, err := machineapi.New("http://10.0.0.1:7600", http.DefaultClient)
	require.Error(t, err)
	_, err = machineapi.New("https://10.0.0.1:7600", http.DefaultClient)
	require.NoError(t, err, "an https URL is taken")
}
