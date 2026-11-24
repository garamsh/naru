package hostapi_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/hostapi"
)

func TestClient_GuestCallsSendTheirBodyAsTheRouteTakesIt(t *testing.T) {
	type seen struct{ method, path, query, contentType, body string }
	var got seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = seen{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), string(b)}
		switch r.Method {
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"exitCode":143,"signal":15,"stdout":"b3V0","stderr":""}`))
		case http.MethodGet:
			_, _ = w.Write([]byte("content, as it is"))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)
	c := hostapi.NewClient(srv.URL, srv.Client())
	ctx := t.Context()

	out, err := c.Exec(ctx, "web", hostapi.ExecRequest{Command: []string{"cat", "/data/a"}, TimeoutSeconds: 5})
	require.NoError(t, err)
	assert.Equal(t, seen{"POST", "/v1/microvms/web/exec", "", "application/json", `{"command":["cat","/data/a"],"timeoutSeconds":5}`}, got)
	assert.Equal(t, hostapi.ExecResult{ExitCode: 143, Signal: 15, Stdout: []byte("out"), Stderr: []byte{}}, out)

	b, err := c.ReadFile(ctx, "web", "/data/a b")
	require.NoError(t, err)
	assert.Equal(t, seen{"GET", "/v1/microvms/web/files", "path=%2Fdata%2Fa+b", "", ""}, got)
	assert.Equal(t, "content, as it is", string(b), "the body, not JSON")

	require.NoError(t, c.WriteFile(ctx, "web", "/data/a", []byte("raw")))
	assert.Equal(t, seen{"PUT", "/v1/microvms/web/files", "path=%2Fdata%2Fa", "application/octet-stream", "raw"}, got)
}

func TestClient_ReadsAGuestFileAsLargeAsTheBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, hostapi.MaxGuestBytes))
	}))
	t.Cleanup(srv.Close)
	b, err := hostapi.NewClient(srv.URL, srv.Client()).ReadFile(t.Context(), "web", "/data/a")
	require.NoError(t, err)
	assert.Len(t, b, hostapi.MaxGuestBytes)
}

func TestExecRequest_CheckRefusesAnEmptyCommandAndATimeoutAboveTheMost(t *testing.T) {
	field, err := hostapi.ExecRequest{}.Check()
	assert.Error(t, err)
	assert.Equal(t, "command", field)
	field, err = hostapi.ExecRequest{Command: []string{"cat"}, TimeoutSeconds: hostapi.MaxExecSeconds + 1}.Check()
	assert.Error(t, err)
	assert.Equal(t, "timeoutSeconds", field)
	_, err = hostapi.ExecRequest{Command: []string{"cat"}, TimeoutSeconds: hostapi.MaxExecSeconds}.Check()
	assert.NoError(t, err, "control: the most")
	assert.Equal(t, uint32(hostapi.DefaultExecSeconds), hostapi.ExecRequest{}.Timeout(), "0 is the default")
	assert.Equal(t, uint32(5), hostapi.ExecRequest{TimeoutSeconds: 5}.Timeout())
}
