package api

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/mvmd"
	"github.com/garamsh/naru/lib/hostapi"
	"github.com/garamsh/naru/lib/wire"
)

// doToken is do with the leader token header set.
func (e *env) doToken(t *testing.T, c *http.Client, method, path string, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, "https://"+e.addr+path, nil)
	require.NoError(t, err)
	req.Header.Set(wire.LeaderTokenHeader, token)
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestLeaderToken_ALowerTokenIsRefusedAndTheCurrentOneAndAnOperatorAreNot(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	code, b := e.do(t, c, "POST", "/v1/microvms", createBody())
	require.Equal(t, 201, code, string(b))

	code, b = e.doToken(t, c, "POST", "/v1/microvms/web/stop", "20")
	require.Equal(t, 200, code, string(b))
	require.Equal(t, mvmd.StateStopped, e.rt.machines["web"].State)

	// The earlier leader's token is lower than the one the host has seen.
	code, b = e.doToken(t, c, "POST", "/v1/microvms/web/start", "19")
	require.Equal(t, 409, code, string(b))
	var body hostapi.ErrorBody
	require.NoError(t, json.Unmarshal(b, &body))
	assert.Equal(t, hostapi.CodeStaleLeader, body.Code)
	assert.Equal(t, mvmd.StateStopped, e.rt.machines["web"].State, "a refused call reached mvmd")

	// Controls: the token it equals, a higher one and a call with no token all go through.
	code, b = e.doToken(t, c, "POST", "/v1/microvms/web/start", "20")
	assert.Equal(t, 200, code, string(b))
	code, b = e.doToken(t, c, "POST", "/v1/microvms/web/stop", "21")
	assert.Equal(t, 200, code, string(b))
	code, b = e.do(t, c, "POST", "/v1/microvms/web/start", nil)
	assert.Equal(t, 200, code, string(b))
	// A read is not fenced, whatever it carries.
	code, _ = e.doToken(t, e.as(t, ReaderOrganization), "GET", "/v1/microvms", "1")
	assert.Equal(t, 200, code)
}

func TestLeaderToken_ATokenThatIsNotAPositiveIntegerIsRefused(t *testing.T) {
	e := start(t)
	c := e.as(t, ProvisionerOrganization)
	for _, token := range []string{"x", "0", "-4", "1.5"} {
		code, _ := e.doToken(t, c, "POST", "/v1/microvms/web/stop", token)
		assert.Equal(t, 400, code, token)
	}
}

func TestLeaderToken_TheHighestTokenSurvivesARestartOfTheHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leader-token")
	first := leaderTokens{path: path}
	ok, err := first.admit(7)
	require.NoError(t, err)
	require.True(t, ok)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(7), string(b))

	restarted := leaderTokens{path: path}
	ok, err = restarted.admit(6)
	require.NoError(t, err)
	assert.False(t, ok, "a restart forgot the highest token")
	ok, err = restarted.admit(7)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestLeaderToken_AFileThatHoldsNoTokenFailsTheCallAndKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leader-token")
	require.NoError(t, os.WriteFile(path, []byte("garbage"), 0o600))
	l := leaderTokens{path: path}
	_, err := l.admit(5)
	require.Error(t, err)
	b, _ := os.ReadFile(path)
	assert.Equal(t, "garbage", string(b))
}
