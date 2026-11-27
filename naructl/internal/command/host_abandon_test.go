package command_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
	"github.com/garamsh/naru/lib/orchapi"
)

func TestHostAbandon_SendsTheFenceWithTheOperatorsSignedRevocationOfThatHost(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	out, err := run(t, op.args(f, "host", "abandon", "host-a", "--fenced", "powered off at the PDU")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, "host host-a is abandoned; its revocation is pending: signer unreachable; host abandon again retries it")

	f.mu.Lock()
	defer f.mu.Unlock()
	require.Len(t, f.abandons, 1)
	sent := f.abandons[0]
	assert.Equal(t, "host-a", sent.host)
	assert.Equal(t, "powered off at the PDU", sent.req.Fenced)
	who, body, err := identity.VerifyRequest(sent.req.RevokeHost, identity.ActionRevokeHost, op.pool, nil, time.Now())
	require.NoError(t, err, "a revoke-host request the signer verifies")
	assert.Equal(t, "alice", who.Subject, "signed by the operator's own identity")
	var req identity.HostSerialsRequest
	require.NoError(t, json.Unmarshal(body, &req))
	assert.Equal(t, "host-a", req.HostID)
}

func TestHostAbandon_NeedsAnAccountOfTheFence(t *testing.T) {
	f := newFakeOrch(t)
	op := newOperator(t, f, "operator")

	_, err := run(t, op.args(f, "host", "abandon", "host-a")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--fenced is required")
	_, err = run(t, op.args(f, "host", "abandon", "host-a", "--fenced", " ")...)
	require.Error(t, err)

	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Empty(t, f.abandons, "nothing was sent")
}

func TestHostGet_PrintsTheHostWithItsAbandonment(t *testing.T) {
	f := newFakeOrch(t)
	f.hosts = []orchapi.Host{{ID: "host-a"}, {ID: "host-b", Abandonment: &orchapi.HostAbandonment{
		By: "alice", Fenced: "powered off", Revocation: orchapi.HostRevocation{RevokedAt: time.Unix(1, 0).UTC(), Serials: []string{"0a"}},
	}}}
	op := newOperator(t, f, "reader")

	out, err := run(t, op.args(f, "host", "get", "host-b")...)
	require.NoError(t, err, out)
	assert.Contains(t, out, `"fenced": "powered off"`)
	assert.Contains(t, out, `"serials": [`)
	out, err = run(t, op.args(f, "host", "get", "host-a")...)
	require.NoError(t, err, out)
	assert.NotContains(t, out, "abandonment", "control: a host not abandoned")
	_, err = run(t, op.args(f, "host", "get", "host-z")...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no admitted host "host-z"`)
}
