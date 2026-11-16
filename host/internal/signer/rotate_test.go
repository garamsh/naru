package signer_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

func (e *env) rotate(by person, hostID string, csr []byte) (int, []byte) {
	return e.do(http.MethodPost, "/v1/reissue-host", e.signed(by, identity.ActionReissueHost, identity.ReissueHostRequest{HostID: hostID, CSR: csr}))
}

func (e *env) register(by person, hostID, serial string) (int, []byte) {
	return e.do(http.MethodPost, "/v1/register-host", e.signed(by, identity.ActionRegisterHost, identity.RegisterHostRequest{HostID: hostID, Serial: serial}))
}

func (e *env) retire(by person, hostID, keep string) (int, []byte) {
	return e.do(http.MethodPost, "/v1/retire-host", e.signed(by, identity.ActionRetireHost, identity.RetireHostRequest{HostID: hostID, Keep: keep}))
}

func TestReissueHost_IssuesANewCertificateAndRevokesNothing(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	old := e.hostCert("host-1")

	newKeyPair := newKey(t)
	status, body := e.rotate(op, "host-1", csrFor(t, newKeyPair))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	fresh := parseCert(t, issued.Certificate)
	assert.Equal(t, "host-1", fresh.Subject.CommonName)
	assert.Equal(t, []string{"host-1"}, fresh.DNSNames)
	assert.True(t, newKeyPair.PublicKey.Equal(fresh.PublicKey), "the certificate is for the new key")
	assert.Equal(t, identity.RoleHost, roleOf(t, fresh))
	assert.NotEqual(t, identity.SerialOf(old), issued.Serial)

	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials, "the old pair stays valid until the host holds the new one")
}

func TestRetireHost_RevokesEverySerialButTheKeptOneAndRetryingChangesNothing(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	old := e.hostCert("host-1")
	other := e.hostCert("host-2")
	status, body := e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, string(body))
	var abandoned identity.Issued
	require.NoError(t, json.Unmarshal(body, &abandoned))
	status, body = e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, string(body))
	var kept identity.Issued
	require.NoError(t, json.Unmarshal(body, &kept))

	status, body = e.retire(op, "host-1", kept.Serial)
	require.Equal(t, http.StatusOK, status, string(body))
	serials, seq := serialsOf(t, e, openAt(t, e.dir))
	assert.ElementsMatch(t, []string{identity.SerialOf(old), abandoned.Serial}, serials, "the old pair and the one the host never received")
	assert.NotContains(t, serials, kept.Serial)
	assert.NotContains(t, serials, identity.SerialOf(other), "another host's serial is not touched")

	status, _ = e.retire(op, "host-1", kept.Serial)
	require.Equal(t, http.StatusOK, status)
	_, again := serialsOf(t, e, e.s)
	assert.Equal(t, seq, again, "a retry publishes no new list")
}

func TestRetireHost_RefusesASerialTheHostDoesNotHold(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	old := e.hostCert("host-1")
	other := e.hostCert("host-2")

	status, body := e.retire(op, "host-1", identity.SerialOf(other))
	assert.Equal(t, http.StatusConflict, status, string(body))
	status, _ = e.retire(op, "host-1", "zz")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = e.retire(e.issue(identity.RoleReader, "bob"), "host-1", identity.SerialOf(old))
	assert.Equal(t, http.StatusForbidden, status)
	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials, "a refused retirement revoked nothing")
}

func TestReissueHost_RefusesAHostTheSignerHoldsNoSerialFor(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	e.hostCert("host-1")

	status, body := e.rotate(op, "host-9", csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusConflict, status, string(body))
	assert.Contains(t, string(body), "register-host")
	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials)

	reader := e.issue(identity.RoleReader, "bob")
	status, _ = e.rotate(reader, "host-1", csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusForbidden, status, "a reader may not rotate")
	status, _ = e.rotate(op, "host 1", csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusBadRequest, status)
}

func TestRegisterHost_AnOfflinePairIsRevokedByTheRetirementAfterARotation(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	const offline = "abc123def456"

	status, body := e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusConflict, status, string(body), "control: unregistered, the host cannot be rotated")

	status, body = e.register(op, "host-1", "ABC123DEF456")
	require.Equal(t, http.StatusOK, status, string(body))
	status, _ = e.register(op, "host-1", offline)
	require.Equal(t, http.StatusOK, status, "registering twice changes nothing")
	status, _ = e.register(op, "host-1", "zz")
	assert.Equal(t, http.StatusBadRequest, status)
	status, _ = e.register(e.issue(identity.RoleReader, "bob"), "host-1", offline)
	assert.Equal(t, http.StatusForbidden, status)
	status, body = e.register(op, "host-1", e.rootID.cert.SerialNumber.Text(16))
	assert.Equal(t, http.StatusBadRequest, status, "root's serial is not a host's: "+string(body))

	status, body = e.rotate(op, "host-1", csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	status, body = e.retire(op, "host-1", issued.Serial)
	require.Equal(t, http.StatusOK, status, string(body))
	serials, _ := serialsOf(t, e, openAt(t, e.dir))
	assert.Equal(t, []string{offline}, serials)
}

func TestReissueHost_TheRegistryTravelsInTheBundle(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	old := e.hostCert("host-1")

	restored := replica(t, e)
	issued, err := restored.ReissueHost(e.signed(op, identity.ActionReissueHost, identity.ReissueHostRequest{HostID: "host-1", CSR: csrFor(t, newKey(t))}))
	require.NoError(t, err)
	require.NoError(t, restored.RetireHost(e.signed(op, identity.ActionRetireHost, identity.RetireHostRequest{HostID: "host-1", Keep: issued.Serial})))
	serials, _ := serialsOf(t, e, restored)
	assert.Equal(t, []string{identity.SerialOf(old)}, serials)
}
