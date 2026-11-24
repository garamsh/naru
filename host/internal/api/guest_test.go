package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
)

// running starts a server with the microVM web running on it, and returns it with a provisioner.
func running(t *testing.T) (*env, *http.Client) {
	t.Helper()
	e := start(t)
	p := e.as(t, ProvisionerOrganization)
	code, body := e.do(t, p, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code, string(body))
	require.Equal(t, "running", e.rt.machines["web"].State, "the microVM runs")
	return e, p
}

// raw sends body as it is.
func (e *env) raw(t *testing.T, c *http.Client, method, path string, body []byte) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+e.addr+path, bytes.NewReader(body))
	require.NoError(t, err)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func execOf(t *testing.T, body []byte) hostapi.ExecResult {
	t.Helper()
	var r hostapi.ExecResult
	require.NoError(t, json.Unmarshal(body, &r), string(body))
	return r
}

func codeOf(t *testing.T, body []byte) hostapi.ErrorBody {
	t.Helper()
	var eb hostapi.ErrorBody
	require.NoError(t, json.Unmarshal(body, &eb), string(body))
	return eb
}

func TestExec_RunsTheCommandAndAnswersItsOutputAndExitCode(t *testing.T) {
	e, p := running(t)
	code, _ := e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/a", []byte("hello"))
	require.Equal(t, 204, code)

	code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"cat", "/data/a"}})
	require.Equal(t, 200, code, string(body))
	assert.Equal(t, hostapi.ExecResult{Stdout: []byte("hello"), Stderr: []byte{}}, execOf(t, body))

	code, body = e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"cat", "/data/b"}})
	require.Equal(t, 200, code, string(body))
	r := execOf(t, body)
	assert.Equal(t, int32(1), r.ExitCode, "the command's own exit code")
	assert.Equal(t, "cat: /data/b: No such file or directory\n", string(r.Stderr))

	code, body = e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"absent"}})
	require.Equal(t, 200, code, string(body))
	assert.Equal(t, hostapi.ExecResult{ExitCode: 127, Stdout: []byte{}, Stderr: []byte("absent: no such file")}, execOf(t, body), "a command that did not start is 127 with the reason")
}

func TestExec_EndsACommandAtItsTimeoutWithSIGKILLAndSaysSo(t *testing.T) {
	e, p := running(t)
	began := time.Now()
	code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"sleep"}, "timeoutSeconds": 1})
	require.Equal(t, 200, code, string(body))
	assert.Less(t, time.Since(began), 5*time.Second)
	assert.Equal(t, hostapi.ExecResult{ExitCode: 137, Signal: 9, TimedOut: true, Stdout: []byte("started\n"), Stderr: []byte{}}, execOf(t, body),
		"the output before the timeout, and the kill")
}

func TestExec_AnswersACommandASignalEndedWithTheSignal(t *testing.T) {
	e, p := running(t)
	code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"kill"}})
	require.Equal(t, 200, code, string(body))
	assert.Equal(t, hostapi.ExecResult{ExitCode: 143, Signal: 15, Stdout: []byte{}, Stderr: []byte{}}, execOf(t, body), "not timed out")
}

func TestExec_EndsACommandWhoseOutputPassesTheBound(t *testing.T) {
	e, p := running(t)
	code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"flood"}})
	assert.Equal(t, 413, code, "%.200s", body)
	e.rt.mu.Lock()
	defer e.rt.mu.Unlock()
	require.Len(t, e.rt.execs, 1)
	for _, x := range e.rt.execs {
		assert.Equal(t, []int32{9}, x.signals, "the command is ended")
	}
}

func TestExec_EndsTheCommandTheRequestsEndLeavesRunning(t *testing.T) {
	e, p := running(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", "https://"+e.addr+"/v1/microvms/web/exec", bytes.NewReader([]byte(`{"command":["sleep"]}`)))
	require.NoError(t, err)
	_, err = p.Do(req)
	require.Error(t, err, "the client gave up")
	require.Eventually(t, func() bool {
		e.rt.mu.Lock()
		defer e.rt.mu.Unlock()
		for _, x := range e.rt.execs {
			return slices.Equal(x.signals, []int32{9})
		}
		return false
	}, 5*time.Second, 20*time.Millisecond)
}

func TestExec_RefusesWhatItsBoundsDoNotTake(t *testing.T) {
	e, p := running(t)
	for what, c := range map[string]struct {
		body  map[string]any
		code  int
		field string
	}{
		"a timeout above the most":   {map[string]any{"command": []string{"cat"}, "timeoutSeconds": hostapi.MaxExecSeconds + 1}, 422, "timeoutSeconds"},
		"an empty command":           {map[string]any{"command": []string{}}, 422, "command"},
		"input, which it takes none": {map[string]any{"command": []string{"cat"}, "stdin": "aGk="}, 400, ""},
		"control: the most":          {map[string]any{"command": []string{"cat"}, "timeoutSeconds": hostapi.MaxExecSeconds}, 200, ""},
	} {
		code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", c.body)
		assert.Equal(t, c.code, code, what+": "+string(body))
		if c.field != "" {
			assert.Equal(t, []string{c.field}, codeOf(t, body).Fields, what)
		}
	}
}

func TestGuest_AMicroVMThatDoesNotRunIsNotRunning(t *testing.T) {
	e, p := running(t)
	code, body := e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"stop"}})
	assert.Equal(t, 409, code)
	assert.Equal(t, hostapi.CodeNotRunning, codeOf(t, body).Code, "a command the stop cut short")

	e.rt.mu.Lock()
	e.rt.failAt["exec web"] = fmt.Errorf("%w: the Machine is stopped", mvmd.ErrPrecondition)
	e.rt.mu.Unlock()
	code, body = e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"cat"}})
	assert.Equal(t, 409, code)
	assert.Equal(t, hostapi.CodeNotRunning, codeOf(t, body).Code, "a microVM that stopped after the host looked")

	code, _ = e.do(t, p, "POST", "/v1/microvms/web/stop", nil)
	require.Equal(t, 200, code)
	require.Equal(t, "stopped", e.rt.machines["web"].State)
	for what, call := range map[string]func() (int, []byte){
		"exec": func() (int, []byte) {
			return e.do(t, p, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"cat"}})
		},
		"file read":  func() (int, []byte) { return e.raw(t, p, "GET", "/v1/microvms/web/files?path=/data/a", nil) },
		"file write": func() (int, []byte) { return e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/a", []byte("x")) },
	} {
		code, body := call()
		assert.Equal(t, 409, code, what)
		assert.Equal(t, hostapi.CodeNotRunning, codeOf(t, body).Code, what)
	}
	code, _ = e.do(t, p, "POST", "/v1/microvms/other/exec", map[string]any{"command": []string{"cat"}})
	assert.Equal(t, 404, code, "a microVM the host has not")
}

func TestFiles_WriteAndReadAGuestFileWithinTheBound(t *testing.T) {
	e, p := running(t)
	code, _ := e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/a", []byte("hello"))
	require.Equal(t, 204, code)
	code, body := e.raw(t, p, "GET", "/v1/microvms/web/files?path=/data/a", nil)
	assert.Equal(t, 200, code)
	assert.Equal(t, "hello", string(body))

	code, body = e.raw(t, p, "GET", "/v1/microvms/web/files?path=/data/b", nil)
	assert.Equal(t, 404, code)
	assert.Equal(t, hostapi.CodeFileNotFound, codeOf(t, body).Code)

	for _, bad := range []string{"data/a", "/data/../a", "/proc/1", "/"} {
		code, body := e.raw(t, p, "GET", "/v1/microvms/web/files?path="+bad, nil)
		assert.Equal(t, 422, code, bad)
		assert.Equal(t, []string{"path"}, codeOf(t, body).Fields, bad)
	}

	e.rt.mu.Lock()
	e.rt.calls = nil
	e.rt.mu.Unlock()
	code, _ = e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/big", make([]byte, hostapi.MaxGuestBytes+1))
	assert.Equal(t, 413, code)
	e.rt.mu.Lock()
	assert.Empty(t, e.rt.calls, "a file above the bound reaches no guest")
	e.rt.mu.Unlock()
	code, _ = e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/big", make([]byte, hostapi.MaxGuestBytes))
	assert.Equal(t, 204, code, "control: the most a file holds")

	e.rt.mu.Lock()
	e.rt.guest["web"]["/data/huge"] = make([]byte, hostapi.MaxGuestBytes+1)
	e.rt.mu.Unlock()
	code, _ = e.raw(t, p, "GET", "/v1/microvms/web/files?path=/data/huge", nil)
	assert.Equal(t, 413, code, "a guest's file above the bound")
}

func TestRoles_AReaderMayNotReadAGuestsFileNorExec(t *testing.T) {
	e, p := running(t)
	code, _ := e.raw(t, p, "PUT", "/v1/microvms/web/files?path=/data/a", []byte("s3cret"))
	require.Equal(t, 204, code)
	reader := e.as(t, ReaderOrganization)
	code, body := e.raw(t, reader, "GET", "/v1/microvms/web/files?path=/data/a", nil)
	assert.Equal(t, 403, code)
	assert.NotContains(t, string(body), "s3cret")
	code, _ = e.do(t, reader, "POST", "/v1/microvms/web/exec", map[string]any{"command": []string{"cat", "/data/a"}})
	assert.Equal(t, 403, code)
	code, _ = e.do(t, reader, "GET", "/v1/microvms/web", nil)
	assert.Equal(t, 200, code, "control: the reader reads the microVM")
	code, _ = e.raw(t, p, "GET", "/v1/microvms/web/files?path=/data/a", nil)
	assert.Equal(t, 200, code, "control: a provisioner reads the file")
}
