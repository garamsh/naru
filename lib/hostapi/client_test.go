package hostapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

// serve answers each request with status and body, and records the last one.
func serve(t *testing.T, status int, body any) (*hostapi.Client, *http.Request, *[]byte) {
	t.Helper()
	var got http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = *r
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		gotBody = raw
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return hostapi.NewClient(srv.URL+"/", srv.Client()), &got, &gotBody
}

func TestClient_EachCallSendsItsMethodPathAndBody(t *testing.T) {
	ctx := t.Context()
	lapsed := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name         string
		call         func(c *hostapi.Client) error
		method, path string
		sent         string
		answer       any
	}{
		{"host", func(c *hostapi.Client) error { _, err := c.Host(ctx); return err }, "GET", "/v1/host", "", hostapi.Host{Region: "eu"}},
		{"create", func(c *hostapi.Client) error {
			_, err := c.CreateMicroVM(ctx, hostapi.CreateRequest{Name: "web", Fenced: true})
			return err
		}, "POST", "/v1/microvms", `"fenced":true`, hostapi.MicroVM{Name: "web"}},
		{"get", func(c *hostapi.Client) error { _, err := c.MicroVM(ctx, "web"); return err }, "GET", "/v1/microvms/web", "", hostapi.MicroVM{}},
		{"list", func(c *hostapi.Client) error { _, err := c.MicroVMs(ctx); return err }, "GET", "/v1/microvms", "", map[string]any{"microvms": []any{}}},
		{"stop", func(c *hostapi.Client) error { _, err := c.StopMicroVM(ctx, "web"); return err }, "POST", "/v1/microvms/web/stop", "", hostapi.MicroVM{}},
		{"start", func(c *hostapi.Client) error { _, err := c.StartMicroVM(ctx, "web"); return err }, "POST", "/v1/microvms/web/start", "", hostapi.MicroVM{}},
		{"delete", func(c *hostapi.Client) error { return c.DeleteMicroVM(ctx, "web") }, "DELETE", "/v1/microvms/web", "", nil},
		{"renew", func(c *hostapi.Client) error {
			_, err := c.RenewFence(ctx, "web", hostapi.FenceRequest{LengthSeconds: 60, Renewal: 2})
			return err
		}, "POST", "/v1/microvms/web/fence", `{"lengthSeconds":60,"renewal":2}`, hostapi.Fence{}},
		{"fence", func(c *hostapi.Client) error { _, err := c.Fence(ctx, "web"); return err }, "GET", "/v1/microvms/web/fence", "", hostapi.Fence{}},
		{"snapshot", func(c *hostapi.Client) error {
			_, err := c.SnapshotMicroVM(ctx, "web", hostapi.SnapshotRequest{Name: "s"})
			return err
		}, "POST", "/v1/microvms/web/snapshot", `{"name":"s"}`, hostapi.Snapshot{}},
		{"snapshots", func(c *hostapi.Client) error { _, err := c.Snapshots(ctx); return err }, "GET", "/v1/snapshots", "", map[string]any{"snapshots": []any{}}},
		{"snapshot get", func(c *hostapi.Client) error { _, err := c.Snapshot(ctx, "s"); return err }, "GET", "/v1/snapshots/s", "", hostapi.Snapshot{}},
		{"snapshot delete", func(c *hostapi.Client) error { return c.DeleteSnapshot(ctx, "s") }, "DELETE", "/v1/snapshots/s", "", nil},
		{"resume", func(c *hostapi.Client) error {
			_, err := c.ResumeSnapshot(ctx, "s", hostapi.ResumeRequest{Name: "web2"})
			return err
		}, "POST", "/v1/snapshots/s/resume", `"name":"web2"`, hostapi.MicroVM{FenceLapsedAt: lapsed}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status := http.StatusOK
			if tc.answer == nil {
				status = http.StatusNoContent
			}
			c, got, sent := serve(t, status, tc.answer)
			require.NoError(t, tc.call(c))
			assert.Equal(t, tc.method, got.Method)
			assert.Equal(t, tc.path, got.URL.Path)
			if tc.sent != "" {
				assert.Contains(t, string(*sent), tc.sent)
			}
		})
	}
}

func TestClient_DecodesTheAnswers(t *testing.T) {
	lapsed := time.Date(2026, 11, 14, 3, 0, 0, 0, time.UTC)
	c, _, _ := serve(t, http.StatusOK, hostapi.MicroVM{Name: "web", Fenced: true, FenceLapsedAt: lapsed})
	m, err := c.MicroVM(t.Context(), "web")
	require.NoError(t, err)
	assert.True(t, m.Fenced)
	assert.True(t, m.FenceLapsedAt.Equal(lapsed))

	c, _, _ = serve(t, http.StatusOK, map[string]any{"snapshots": []hostapi.Snapshot{{Name: "a"}, {Name: "b"}}})
	all, err := c.Snapshots(t.Context())
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestClient_ARefusalIsATypedError(t *testing.T) {
	c, _, _ := serve(t, http.StatusUnprocessableEntity, hostapi.ErrorBody{Error: "the request is invalid", Fields: []string{"name"}})
	_, err := c.CreateMicroVM(t.Context(), hostapi.CreateRequest{})
	var apiErr *hostapi.Error
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 422, apiErr.Status)
	assert.Equal(t, []string{"name"}, apiErr.Fields)
	assert.Contains(t, err.Error(), "name")

	c, _, _ = serve(t, http.StatusBadGateway, nil)
	err = c.DeleteMicroVM(t.Context(), "web")
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 502, apiErr.Status)
	assert.Equal(t, "Bad Gateway", apiErr.Message, "an answer with no body names the status")
}

func TestClient_ANameIsEscapedInThePath(t *testing.T) {
	c, got, _ := serve(t, http.StatusOK, hostapi.MicroVM{})
	_, err := c.MicroVM(t.Context(), "a/b")
	require.NoError(t, err)
	assert.Equal(t, "/v1/microvms/a%2Fb", got.URL.EscapedPath())
}
