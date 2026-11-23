package signer_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func (e *env) lookupHost(by person, hostID string) (int, []byte) {
	return e.do(http.MethodPost, "/v1/lookup-host", e.signed(by, identity.ActionLookupHost, identity.HostSerialsRequest{HostID: hostID}))
}

func (e *env) revokeHost(by person, hostID string) (int, []byte) {
	return e.do(http.MethodPost, "/v1/revoke-host", e.signed(by, identity.ActionRevokeHost, identity.HostSerialsRequest{HostID: hostID}))
}

func decodeHostSerials(t *testing.T, body []byte) identity.HostSerials {
	t.Helper()
	var out identity.HostSerials
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

func TestLookupHost_ListsEverySerialTheSignerHoldsWithoutRevokingAny(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	first := e.hostCert("host-1")
	e.hostCert("host-2")
	status, body := e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, string(body))
	var second identity.Issued
	require.NoError(t, json.Unmarshal(body, &second))
	status, body = e.register(op, "host-1", "abc123")
	require.Equal(t, http.StatusOK, status, string(body))

	status, body = e.lookupHost(op, "host-1")
	require.Equal(t, http.StatusOK, status, string(body))
	got := decodeHostSerials(t, body)
	assert.Equal(t, "host-1", got.HostID)
	assert.Equal(t, []identity.HostSerial{{Serial: identity.SerialOf(first)}, {Serial: second.Serial}, {Serial: "abc123"}}, got.Serials, "every serial, none revoked, host-2's left out")
	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials, "a lookup revokes nothing")
}

func TestLookupHost_RefusesAHostTheSignerHoldsNoSerialForAndNamesRegisterHost(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	e.hostCert("host-1")

	status, body := e.lookupHost(op, "host-1")
	require.Equal(t, http.StatusOK, status, "control: a host the signer holds a serial for is answered")
	status, body = e.lookupHost(op, "host-9")
	assert.Equal(t, http.StatusConflict, status, string(body))
	assert.Contains(t, string(body), "naructl signer register-host")
	status, _ = e.lookupHost(op, "host 1")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = e.lookupHost(e.issue(identity.RoleReader, "bob"), "host-1")
	assert.Equal(t, http.StatusForbidden, status, "a reader may not look a host up")
}

func TestRevokeHost_RevokesEverySerialOfThatHostInOneListAndRetryingChangesNothing(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	first := e.hostCert("host-1")
	other := e.hostCert("host-2")
	status, body := e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, string(body))
	var second identity.Issued
	require.NoError(t, json.Unmarshal(body, &second))
	_, before := serialsOf(t, e, e.s)

	status, body = e.revokeHost(op, "host-1")
	require.Equal(t, http.StatusOK, status, string(body))
	serials, seq := serialsOf(t, e, openAt(t, e.dir))
	assert.ElementsMatch(t, []string{identity.SerialOf(first), second.Serial}, serials)
	assert.NotContains(t, serials, identity.SerialOf(other), "another host's serial is not touched")
	assert.Equal(t, before+1, seq, "one signing for every serial")
	assert.Equal(t, []identity.HostSerial{{Serial: identity.SerialOf(first), Revoked: true}, {Serial: second.Serial, Revoked: true}}, decodeHostSerials(t, body).Serials)

	status, body = e.lookupHost(op, "host-1")
	require.Equal(t, http.StatusOK, status, string(body))
	assert.Equal(t, []identity.HostSerial{{Serial: identity.SerialOf(first), Revoked: true}, {Serial: second.Serial, Revoked: true}}, decodeHostSerials(t, body).Serials, "the lookup after reports them revoked")
	status, _ = e.revokeHost(op, "host-1")
	require.Equal(t, http.StatusOK, status)
	_, again := serialsOf(t, e, e.s)
	assert.Equal(t, seq, again, "a retry publishes no new list")
}

func TestRevokeHost_RefusesWhatItCannotRevokeAndRevokesNothing(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	e.hostCert("host-1")

	status, body := e.revokeHost(op, "host-9")
	assert.Equal(t, http.StatusConflict, status, string(body))
	assert.Contains(t, string(body), "naructl signer register-host")
	status, _ = e.revokeHost(op, "host 1")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = e.revokeHost(e.issue(identity.RoleReader, "bob"), "host-1")
	assert.Equal(t, http.StatusForbidden, status)
	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials, "a refused call revoked nothing")
	status, _ = e.revokeHost(op, "host-1")
	require.Equal(t, http.StatusOK, status, "control: the same operator revokes a host the signer holds")
}
