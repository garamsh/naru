package signer_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// reissueRequest is a request to reissue root for key, signed by key's holder as the recovery key.
func reissueRequest(t *testing.T, signWith ed25519.PrivateKey, csr []byte, nonce string, expires time.Time) []byte {
	t.Helper()
	b, err := identity.SignRecoveryRequest(identity.ActionReissueRoot, identity.ReissueRootRequest{Nonce: nonce, CSR: csr}, expires, signWith)
	require.NoError(t, err)
	return b
}

func freshNonce(t *testing.T) string {
	t.Helper()
	n, err := identity.NewNonce()
	require.NoError(t, err)
	return n
}

// reissue posts a request the recovery key signs for a new root key, and returns the status, the
// body and the new key.
func (e *env) reissue(nonce string) (int, []byte, person) {
	e.t.Helper()
	key := newKey(e.t)
	status, body := e.do(http.MethodPost, "/v1/reissue-root", reissueRequest(e.t, e.recovery.Ed25519, csrFor(e.t, key), nonce, time.Now().Add(time.Minute)))
	var p person
	if status == http.StatusOK {
		var issued identity.Issued
		require.NoError(e.t, json.Unmarshal(body, &issued))
		p = person{key: key, cert: parseCert(e.t, issued.Certificate)}
	}
	return status, body, p
}

// canIssue reports whether who is admitted as root: whether root's call to issue an operator is taken.
func (e *env) canIssue(who person) bool {
	e.t.Helper()
	status, _ := e.do(http.MethodPost, "/v1/operator", e.signed(who, identity.ActionIssueOperator,
		identity.IssueRequest{Name: "probe", Role: identity.RoleReader, CSR: csrFor(e.t, newKey(e.t))}))
	return status == http.StatusOK
}

func TestReissueRoot_TheRecoveryKeyGetsANewRootAndTheOldOneIsNoLongerAdmitted(t *testing.T) {
	e := newEnv(t)
	require.True(t, e.canIssue(e.rootID), "control: the old root is admitted before")
	oldSerial := e.rootID.cert.SerialNumber.Text(16)

	status, body, fresh := e.reissue(freshNonce(t))
	require.Equal(t, http.StatusOK, status, string(body))

	assert.Equal(t, identity.RoleRoot, roleOf(t, fresh.cert))
	assert.NoError(t, fresh.cert.CheckSignatureFrom(parseCert(t, e.authorities().Operator)), "the new root chains to the operator authority")
	assert.True(t, e.canIssue(fresh), "the new root is admitted")
	assert.False(t, e.canIssue(e.rootID), "the old root is not")
	status, _ = e.do(http.MethodGet, "/v1/bundle", nil, "X-Naru-Request", base64.StdEncoding.EncodeToString(e.signed(e.rootID, identity.ActionBundle, struct{}{})))
	assert.Equal(t, http.StatusForbidden, status, "nor for the bundle")
	status, _ = e.do(http.MethodGet, "/v1/bundle", nil, "X-Naru-Request", base64.StdEncoding.EncodeToString(e.signed(fresh, identity.ActionBundle, struct{}{})))
	assert.Equal(t, http.StatusOK, status, "the new root takes the bundle")

	serials, _ := serialsOf(t, e, e.s)
	assert.Equal(t, []string{oldSerial}, serials, "the old root's serial is on the list, and the new root's is not")

	reopened := openAt(t, e.dir)
	serials, _ = serialsOf(t, e, reopened)
	assert.Equal(t, []string{oldSerial}, serials, "the revocation is kept in the directory")
	status, _ = e.revoke(e.rootID, "1")
	assert.Equal(t, http.StatusForbidden, status)
}

func TestReissueRoot_EachReissueRevokesTheRootBeforeIt(t *testing.T) {
	e := newEnv(t)
	status, _, second := e.reissue(freshNonce(t))
	require.Equal(t, http.StatusOK, status)
	status, _, third := e.reissue(freshNonce(t))
	require.Equal(t, http.StatusOK, status)

	assert.False(t, e.canIssue(e.rootID))
	assert.False(t, e.canIssue(second), "a root that was reissued over is no longer admitted")
	assert.True(t, e.canIssue(third))
}

func TestReissueRoot_RefusedWhenTheRecoveryKeyDidNotSignIt(t *testing.T) {
	e := newEnv(t)
	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	csr := csrFor(t, newKey(t))
	exp := time.Now().Add(time.Minute)

	status, _ := e.do(http.MethodPost, "/v1/reissue-root", reissueRequest(t, other, csr, freshNonce(t), exp))
	assert.Equal(t, http.StatusForbidden, status, "a key that is not the recovery key")
	status, _ = e.do(http.MethodPost, "/v1/reissue-root", e.signed(e.rootID, identity.ActionReissueRoot, identity.ReissueRootRequest{Nonce: freshNonce(t), CSR: csr}))
	assert.Equal(t, http.StatusForbidden, status, "root's own signature is not the recovery key's")
	status, _ = e.do(http.MethodPost, "/v1/reissue-root", []byte("not a request"))
	assert.Equal(t, http.StatusBadRequest, status)

	signed := reissueRequest(t, e.recovery.Ed25519, csr, freshNonce(t), exp)
	tampered := bytes.Replace(signed, []byte(`"signature":"`), []byte(`"signature":"AAAA`), 1)
	status, _ = e.do(http.MethodPost, "/v1/reissue-root", tampered)
	assert.Equal(t, http.StatusForbidden, status, "a damaged signature")

	assert.True(t, e.canIssue(e.rootID), "no refusal revoked the root")
	serials, _ := serialsOf(t, e, e.s)
	assert.Empty(t, serials)
}

func TestReissueRoot_ARequestIsTakenOnceAndTheReplayIsRefused(t *testing.T) {
	e := newEnv(t)
	key := newKey(t)
	signed := reissueRequest(t, e.recovery.Ed25519, csrFor(t, key), freshNonce(t), time.Now().Add(time.Minute))

	status, body := e.do(http.MethodPost, "/v1/reissue-root", signed)
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	fresh := person{key: key, cert: parseCert(t, issued.Certificate)}

	status, _ = e.do(http.MethodPost, "/v1/reissue-root", signed)
	assert.Equal(t, http.StatusForbidden, status, "the same request again")
	assert.True(t, e.canIssue(fresh), "the replay did not revoke the root the first request made")

	reopened := openAt(t, e.dir)
	_, err := reopened.ReissueRoot(signed)
	assert.ErrorIs(t, err, signer.ErrReplay, "a signer that was restarted still refuses it")

	status, _, _ = e.reissue(freshNonce(t))
	assert.Equal(t, http.StatusOK, status, "control: a request with a new nonce is taken")
}

func TestReissueRoot_ANonceUsedByAnotherRequestIsRefused(t *testing.T) {
	e := newEnv(t)
	nonce := freshNonce(t)
	status, _, _ := e.reissue(nonce)
	require.Equal(t, http.StatusOK, status)

	status, _, _ = e.reissue(nonce)
	assert.Equal(t, http.StatusForbidden, status, "another request, the same nonce")
}

func TestReissueRoot_RefusesAStaleOrMisshapenRequest(t *testing.T) {
	e := newEnv(t)
	csr := csrFor(t, newKey(t))
	post := func(b []byte) int {
		status, _ := e.do(http.MethodPost, "/v1/reissue-root", b)
		return status
	}

	assert.Equal(t, http.StatusForbidden, post(reissueRequest(t, e.recovery.Ed25519, csr, freshNonce(t), time.Now().Add(-time.Second))), "expired")
	assert.Equal(t, http.StatusBadRequest, post(reissueRequest(t, e.recovery.Ed25519, csr, freshNonce(t), time.Now().Add(time.Hour))), "expiring too far ahead")
	assert.Equal(t, http.StatusBadRequest, post(reissueRequest(t, e.recovery.Ed25519, csr, "short", time.Now().Add(time.Minute))), "a nonce too short")
	assert.Equal(t, http.StatusBadRequest, post(reissueRequest(t, e.recovery.Ed25519, []byte("junk"), freshNonce(t), time.Now().Add(time.Minute))), "no certificate request")
	wrong, err := identity.SignRecoveryRequest(identity.ActionRestore, identity.ReissueRootRequest{Nonce: freshNonce(t), CSR: csr}, time.Now().Add(time.Minute), e.recovery.Ed25519)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, post(wrong), "signed for another action")

	assert.True(t, e.canIssue(e.rootID), "no refusal revoked the root")
}

func TestReissueRoot_IsAuditedAndTheOldRootIsRevokedOnAReplicaAfterTheSync(t *testing.T) {
	e := newEnv(t)
	r := replica(t, e)
	var logs bytes.Buffer
	audited, err := signer.Open(signer.Options{Dir: e.dir, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	require.NoError(t, err)
	key := newKey(t)

	issued, err := audited.ReissueRoot(reissueRequest(t, e.recovery.Ed25519, csrFor(t, key), freshNonce(t), time.Now().Add(time.Minute)))
	require.NoError(t, err)

	line := logs.String()
	assert.Contains(t, line, "reissued root from the recovery key")
	assert.Contains(t, line, issued.Serial)
	assert.Contains(t, line, keyHash(t, csrFor(t, key)), "the new key's hash")
	assert.Contains(t, line, e.rootID.cert.SerialNumber.Text(16), "the serial it revoked")

	data, err := audited.CRL()
	require.NoError(t, err)
	_, err = r.PushCRL(data)
	require.NoError(t, err)
	_, err = r.IssueOrch(e.signed(e.rootID, identity.ActionIssueOrch, identity.IssueRequest{Name: "orch-1", CSR: csrFor(t, newKey(t))}))
	assert.ErrorIs(t, err, identity.ErrRevoked, "the replica refuses the old root once it has the list")
}

func TestReissueRoot_AnUninitialisedSignerRefuses(t *testing.T) {
	s := openAt(t, privateDir(t, t.TempDir()))
	_, err := s.ReissueRoot([]byte("{}"))
	assert.ErrorIs(t, err, signer.ErrNotInitialised)
}
