package signer_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// callerToken has by ask /v1/caller-token for name, living ttl seconds.
func (e *env) callerToken(by person, name string, ttl int) (int, identity.CallerToken) {
	e.t.Helper()
	status, body := e.do(http.MethodPost, "/v1/caller-token", e.signed(by, identity.ActionIssueCallerToken, identity.CallerTokenRequest{Name: name, TTLSeconds: ttl}))
	var tok identity.CallerToken
	if status == http.StatusOK {
		require.NoError(e.t, json.Unmarshal(body, &tok))
	}
	return status, tok
}

// registerCaller posts token and the request of key to /v1/caller.
func (e *env) registerCaller(token string, csr []byte) (int, []byte) {
	e.t.Helper()
	b, err := json.Marshal(identity.CallerRegistration{Token: token, CSR: csr})
	require.NoError(e.t, err)
	return e.do(http.MethodPost, "/v1/caller", b)
}

// registered registers a caller named name with a key of its own, through a token root asked for.
func (e *env) registered(name string) person {
	e.t.Helper()
	status, tok := e.callerToken(e.rootID, name, 0)
	require.Equal(e.t, http.StatusOK, status)
	key := newKey(e.t)
	status, body := e.registerCaller(tok.Token, csrFor(e.t, key))
	require.Equal(e.t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(e.t, json.Unmarshal(body, &issued))
	return person{key: key, cert: parseCert(e.t, issued.Certificate)}
}

// renewal is p's request to renew itself for key, signed at c's time.
func renewal(t *testing.T, p person, key *ecdsa.PrivateKey, c *clock) []byte {
	t.Helper()
	b, err := identity.SignRequest(identity.ActionRenewCaller, identity.CallerRenewal{CSR: csrFor(t, key)}, c.now().Add(time.Minute), p.key, p.cert)
	require.NoError(t, err)
	return b
}

func TestCallerRegistration_ATokenRootAskedForRegistersTheNamedCallerForItsOwnKey(t *testing.T) {
	e := newEnv(t)
	op := e.issue(identity.RoleOperator, "alice")
	status, _ := e.callerToken(op, "garam-bot", 0)
	assert.Equal(t, http.StatusForbidden, status, "only root asks for a token")

	status, tok := e.callerToken(e.rootID, "garam-bot", 0)
	require.Equal(t, http.StatusOK, status)
	assert.True(t, strings.HasPrefix(tok.Token, "naru-ct-"))
	assert.WithinDuration(t, time.Now().Add(identity.DefaultCallerTokenTTL), tok.ExpiresAt, time.Minute, "an hour by default")

	key := newKey(t)
	status, body := e.registerCaller(tok.Token, csrFor(t, key))
	require.Equal(t, http.StatusOK, status, string(body))
	var issued identity.Issued
	require.NoError(t, json.Unmarshal(body, &issued))
	cert := parseCert(t, issued.Certificate)
	assert.Equal(t, "garam-bot", cert.Subject.CommonName, "the name is the token's")
	assert.Equal(t, identity.RoleCaller, roleOf(t, cert))
	assert.True(t, cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey), "the certificate is for the key the caller made")
	assert.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, cert.ExtKeyUsage)
	assert.WithinDuration(t, time.Now().AddDate(0, 0, identity.MaxCallerDays), cert.NotAfter, time.Minute)

	// The serial is a caller's, so an operator may revoke it.
	status, body = e.revoke(op, issued.Serial)
	assert.Equal(t, http.StatusOK, status, string(body))
}

func TestCallerRegistration_ATokenIsSpentOnceWhateverTheOutcome(t *testing.T) {
	e := newEnv(t)
	_, tok := e.callerToken(e.rootID, "garam-bot", 0)
	status, _ := e.registerCaller(tok.Token, csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status, "control: the first presentation registers")
	status, body := e.registerCaller(tok.Token, csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusForbidden, status, "a token spent twice is refused: %s", body)

	_, tok = e.callerToken(e.rootID, "garam-bot", 0)
	status, _ = e.registerCaller(tok.Token, []byte("not a request"))
	require.Equal(t, http.StatusBadRequest, status)
	status, _ = e.registerCaller(tok.Token, csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusForbidden, status, "a refused registration spent its token")
}

func TestCallerRegistration_AnExpiredTokenIsRefused(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	_, late := e.callerToken(e.rootID, "garam-bot", 60)
	_, timely := e.callerToken(e.rootID, "garam-bot", 120)

	c.advance(90 * time.Second)
	_, err := s.RegisterCaller(identity.CallerRegistration{Token: late.Token, CSR: csrFor(t, newKey(t))})
	assert.ErrorIs(t, err, signer.ErrBadToken, "past its 60 s")
	_, err = s.RegisterCaller(identity.CallerRegistration{Token: timely.Token, CSR: csrFor(t, newKey(t))})
	assert.NoError(t, err, "control: a token that lives 120 s still registers")
}

func TestCallerRegistration_RefusesATokenItDidNotSignOrThatWasChanged(t *testing.T) {
	e := newEnv(t)
	other := newEnv(t)
	_, foreign := other.callerToken(other.rootID, "garam-bot", 0)
	_, own := e.callerToken(e.rootID, "garam-bot", 0)

	payload, sig, ok := strings.Cut(strings.TrimPrefix(own.Token, "naru-ct-"), ".")
	require.True(t, ok)
	claims, err := base64.RawURLEncoding.DecodeString(payload)
	require.NoError(t, err)
	renamed := "naru-ct-" + base64.RawURLEncoding.EncodeToString([]byte(strings.Replace(string(claims), "garam-bot", "garam-eve", 1))) + "." + sig

	for name, token := range map[string]string{
		"another signer's": foreign.Token,
		"a changed name":   renamed,
		"no prefix":        strings.TrimPrefix(own.Token, "naru-ct-"),
		"empty":            "",
	} {
		status, body := e.registerCaller(token, csrFor(t, newKey(t)))
		assert.Equal(t, http.StatusForbidden, status, "%s: %s", name, body)
	}
	status, _ := e.registerCaller(own.Token, csrFor(t, newKey(t)))
	assert.Equal(t, http.StatusOK, status, "control: the token as the signer gave it")
}

func TestCallerRegistration_RefusesANameOrALifeOutOfBounds(t *testing.T) {
	e := newEnv(t)
	for name, req := range map[string]identity.CallerTokenRequest{
		"not a DNS label": {Name: "Garam_Bot"},
		"over 24 hours":   {Name: "garam-bot", TTLSeconds: int(identity.MaxCallerTokenTTL/time.Second) + 1},
		"negative":        {Name: "garam-bot", TTLSeconds: -1},
	} {
		status, _ := e.do(http.MethodPost, "/v1/caller-token", e.signed(e.rootID, identity.ActionIssueCallerToken, req))
		assert.Equal(t, http.StatusBadRequest, status, name)
	}
	status, _ := e.callerToken(e.rootID, "garam-bot", int(identity.MaxCallerTokenTTL/time.Second))
	assert.Equal(t, http.StatusOK, status, "control: 24 hours")
}

func TestCallerRegistration_ATokenSpentOnOneSignerIsRefusedOnAnotherAfterTheSync(t *testing.T) {
	e := newEnv(t)
	b, unsynced := replica(t, e), replica(t, e)
	_, tok := e.callerToken(e.rootID, "garam-bot", 0)
	status, _ := e.registerCaller(tok.Token, csrFor(t, newKey(t)))
	require.Equal(t, http.StatusOK, status)

	_, err := unsynced.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.NoError(t, err, "control, and the accepted risk: a signer the list has not reached takes the token")

	pushList(t, b, e.s)
	_, err = b.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	assert.ErrorIs(t, err, signer.ErrBadToken, "the list carried the spent token")
}

func TestCallerRegistration_APeeredSignerThatMissedTheSyncRefusesAndKeepsTheToken(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	pushList(t, s, replica(t, e))
	_, tok := e.callerToken(e.rootID, "garam-bot", 0)

	c.advance(time.Minute)
	_, err := s.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.ErrorIs(t, err, signer.ErrNotSynced)
	pushList(t, s, replica(t, e))
	_, err = s.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	assert.NoError(t, err, "the refusal did not spend the token")
}

func TestCallerRenewal_OnlyInTheLastThirtyDaysForANewKeyWithTheOldOneStillValid(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	p := e.registered("ws-provider")

	_, err := s.RenewCaller(renewal(t, p, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrCallerNotDue, "61 days before it expires")
	c.advance(59 * 24 * time.Hour)
	_, err = s.RenewCaller(renewal(t, p, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrCallerNotDue, "31 days before it expires")
	assert.Equal(t, http.StatusConflict, signer.StatusOf(err))

	c.advance(2 * 24 * time.Hour)
	key := newKey(t)
	issued, err := s.RenewCaller(renewal(t, p, key, &c))
	require.NoError(t, err, "29 days before it expires")
	next := parseCert(t, issued.Certificate)
	assert.Equal(t, "ws-provider", next.Subject.CommonName, "the name is the current certificate's")
	assert.Equal(t, identity.RoleCaller, roleOf(t, next))
	assert.True(t, next.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey))
	assert.WithinDuration(t, c.now().AddDate(0, 0, identity.MaxCallerDays), next.NotAfter, time.Minute)

	_, err = s.RenewCaller(renewal(t, p, newKey(t), &c))
	assert.NoError(t, err, "the old certificate is not revoked by its renewal")
	_, err = s.RenewCaller(renewal(t, person{key: key, cert: next}, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrCallerNotDue, "the new one is not due")
}

func TestCallerRenewal_ARevokedOrExpiredCertificateOrAnotherRoleIsRefused(t *testing.T) {
	e := newEnv(t)
	var c clock
	p, revoked := e.registered("ws-provider"), e.registered("other")
	op := e.issue(identity.RoleOperator, "alice")
	status, _ := e.revoke(op, identity.SerialOf(revoked.cert))
	require.Equal(t, http.StatusOK, status)
	s := openClocked(t, e.dir, &c)
	c.advance(61 * 24 * time.Hour)

	_, err := s.RenewCaller(renewal(t, revoked, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrRevoked, "a revoked certificate renews nothing")
	_, err = s.RenewCaller(renewal(t, op, newKey(t), &c))
	assert.ErrorIs(t, err, signer.ErrForbidden, "an operator is no caller")
	_, err = s.RenewCaller(renewal(t, p, newKey(t), &c))
	require.NoError(t, err, "control: the unrevoked caller in its window renews")

	c.advance(30 * 24 * time.Hour)
	_, err = s.RenewCaller(renewal(t, p, newKey(t), &c))
	require.Error(t, err, "an expired certificate renews nothing")
	assert.Equal(t, http.StatusForbidden, signer.StatusOf(err))
}
