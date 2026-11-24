package signer_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
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

// registerCaller posts token and the request of key to /v1/caller-register.
func (e *env) registerCaller(token string, csr []byte) (int, []byte) {
	e.t.Helper()
	b, err := json.Marshal(identity.CallerRegistration{Token: token, CSR: csr})
	require.NoError(e.t, err)
	return e.do(http.MethodPost, "/v1/caller-register", b)
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
	_, err = s.RegisterCaller(identity.CallerRegistration{Token: "naru-ct-not-a-token", CSR: csrFor(t, newKey(t))})
	require.ErrorIs(t, err, signer.ErrNotSynced, "a bad token gets the same answer, so the answer says nothing of the token")
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
	assert.ErrorIs(t, err, signer.ErrRenewed, "a certificate renews once")
	assert.NotErrorIs(t, err, identity.ErrRevoked, "the old certificate is not revoked by its renewal")
	assert.Equal(t, http.StatusForbidden, signer.StatusOf(err))
	_, err = s.RenewCaller(renewal(t, person{key: key, cert: next}, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrCallerNotDue, "the new one is not due")
}

func TestCallerRenewal_ARenewalForTheCurrentKeyIsRefusedAndSpendsNothing(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	p := e.registered("ws-provider")
	c.advance(61 * 24 * time.Hour)

	_, err := s.RenewCaller(renewal(t, p, p.key, &c))
	assert.ErrorIs(t, err, signer.ErrBadRequest)
	assert.Equal(t, http.StatusBadRequest, signer.StatusOf(err))
	_, err = s.RenewCaller(renewal(t, p, newKey(t), &c))
	assert.NoError(t, err, "control: a new key renews, and the refusal did not spend the renewal")
}

// signedAt is a request by for action with body, signed at c's time.
func signedAt(t *testing.T, by person, c *clock, action string, body any) []byte {
	t.Helper()
	b, err := identity.SignRequest(action, body, c.now().Add(time.Minute), by.key, by.cert)
	require.NoError(t, err)
	return b
}

// clockedReplica is replica on c's time.
func clockedReplica(t *testing.T, e *env, c *clock) *signer.Signer {
	t.Helper()
	bundle, err := e.s.Bundle(e.signed(e.rootID, identity.ActionBundle, struct{}{}))
	require.NoError(t, err)
	sum := sha256.Sum256(bundle)
	authz := e.signed(e.rootID, identity.ActionRestore, identity.RestoreRequest{BundleSHA256: hex.EncodeToString(sum[:])})
	dir := privateDir(t, t.TempDir())
	require.NoError(t, openAt(t, dir).Restore(identity.RestoreBody{Bundle: bundle, Authorisation: authz, RecoveryKey: e.recoveryFile, PassphraseFile: e.passFile}))
	return openClocked(t, dir, c)
}

func TestCallerRenewal_ACertificateRenewsOnceOnEverySignerTheListReaches(t *testing.T) {
	e := newEnv(t)
	var c clock
	p := e.registered("ws-provider")
	a, b := openClocked(t, e.dir, &c), clockedReplica(t, e, &c)
	c.advance(61 * 24 * time.Hour)

	_, err := a.RenewCaller(renewal(t, p, newKey(t), &c))
	require.NoError(t, err)
	pushList(t, b, a)
	_, err = b.RenewCaller(renewal(t, p, newKey(t), &c))
	assert.ErrorIs(t, err, signer.ErrRenewed, "the list carried the renewal")

	q := e.registered("other")
	_, err = b.RenewCaller(renewal(t, q, newKey(t), &c))
	assert.NoError(t, err, "control: another certificate renews on the synced signer")
}

// revokeCaller has by revoke the caller name through /v1/revoke-caller.
func (e *env) revokeCaller(by person, name string) (int, identity.CallerRevocation) {
	e.t.Helper()
	status, body := e.do(http.MethodPost, "/v1/revoke-caller", e.signed(by, identity.ActionRevokeCaller, identity.CallerRevokeRequest{Name: name}))
	var out identity.CallerRevocation
	if status == http.StatusOK {
		require.NoError(e.t, json.Unmarshal(body, &out))
	}
	return status, out
}

func TestCallerRevocation_RevokesEveryCertificateOfTheCallerWhicheverSignerIssuedItAndNotALaterRegistration(t *testing.T) {
	e := newEnv(t)
	var c clock
	p, other := e.registered("ws-provider"), e.registered("other")
	b := clockedReplica(t, e, &c)
	a := openClocked(t, e.dir, &c)
	c.advance(61 * 24 * time.Hour)
	key := newKey(t)
	issued, err := b.RenewCaller(renewal(t, p, key, &c))
	require.NoError(t, err, "the successor is issued by the other signer, which a's records do not hold")
	child := person{key: key, cert: parseCert(t, issued.Certificate)}

	got, err := a.RevokeCaller(signedAt(t, e.rootID, &c, identity.ActionRevokeCaller, identity.CallerRevokeRequest{Name: "ws-provider"}))
	require.NoError(t, err)
	assert.Equal(t, []identity.CallerSerial{{Serial: identity.SerialOf(p.cert), Revoked: true}}, got.Serials, "this signer's record of the caller")
	assert.WithinDuration(t, c.now().Add(-time.Minute), got.RevokedAt, time.Second, "the NotBefore a certificate issued at the revocation has")

	pushList(t, b, a)
	_, err = b.RenewCaller(renewal(t, child, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrRevoked, "the successor another signer issued is revoked by the caller's name")
	_, err = a.RenewCaller(renewal(t, child, newKey(t), &c))
	assert.ErrorIs(t, err, identity.ErrRevoked)
	_, err = b.RenewCaller(renewal(t, other, newKey(t), &c))
	assert.NoError(t, err, "control: another caller renews")

	c.advance(time.Second)
	pushList(t, b, a)
	tok, err := b.IssueCallerToken(signedAt(t, e.rootID, &c, identity.ActionIssueCallerToken, identity.CallerTokenRequest{Name: "ws-provider"}))
	require.NoError(t, err)
	again, err := b.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.NoError(t, err, "a registration after the revocation")
	data, err := b.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), data)
	require.NoError(t, err)
	_, err = identity.Authenticate([][]*x509.Certificate{{parseCert(t, again.Certificate)}}, crl)
	assert.NoError(t, err, "a certificate issued after the revocation is valid")
}

func TestCallerRevocation_RootOrAnOperatorRevokesAValidName(t *testing.T) {
	e := newEnv(t)
	p := e.registered("ws-provider")
	status, _ := e.revokeCaller(p, "ws-provider")
	assert.Equal(t, http.StatusForbidden, status, "a caller revokes no one")
	status, _ = e.revokeCaller(e.issue(identity.RoleOperator, "alice"), "Not_A_Label")
	assert.Equal(t, http.StatusBadRequest, status)
	status, got := e.revokeCaller(e.rootID, "never-registered")
	assert.Equal(t, http.StatusOK, status, "control: a name this signer issued nothing to, which another may have")
	assert.Empty(t, got.Serials)
}

func TestCallerRegistration_TheOldRouteSaysTheCallerIsOlderThanTheSigner(t *testing.T) {
	e := newEnv(t)
	b, err := json.Marshal(identity.CallerRegistration{Token: "x", CSR: []byte("y")})
	require.NoError(t, err)
	status, body := e.do(http.MethodPost, "/v1/caller", b)
	assert.Equal(t, http.StatusGone, status)
	assert.Contains(t, string(body), "/v1/caller-issue")
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

func TestCallerRegistration_LogsTheTokenNonceOfARegistrationAndOfARefusedSpend(t *testing.T) {
	e := newEnv(t)
	status, tok := e.callerToken(e.rootID, "ci-runner", 0)
	require.Equal(t, http.StatusOK, status)
	nonce := identity.CallerTokenNonce(tok.Token)
	require.NotEmpty(t, nonce, "the token names its nonce")
	var logs bytes.Buffer
	audited, err := signer.Open(signer.Options{Dir: e.dir, Log: slog.New(slog.NewTextHandler(&logs, nil))})
	require.NoError(t, err)

	_, err = audited.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.NoError(t, err)
	_, err = audited.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.ErrorIs(t, err, signer.ErrBadToken)

	lines := strings.Split(logs.String(), "\n")
	var registered, refused bool
	for _, l := range lines {
		registered = registered || strings.Contains(l, "registered a caller") && strings.Contains(l, "token_nonce="+nonce)
		refused = refused || strings.Contains(l, "refused a caller registration") && strings.Contains(l, "token_nonce="+nonce)
	}
	assert.True(t, registered, "the registration names the nonce: %s", logs.String())
	assert.True(t, refused, "the refused second spend names the nonce: %s", logs.String())
	assert.Empty(t, identity.CallerTokenNonce("naru-ct-%%%.x"), "a token that does not read names no nonce")
	assert.Empty(t, identity.CallerTokenNonce("other"), "a string without the prefix names no nonce")
	long := identity.CallerTokenPrefix + base64.RawURLEncoding.EncodeToString([]byte(`{"nonce":"`+strings.Repeat("a", 129)+`"}`)) + ".x"
	assert.Empty(t, identity.CallerTokenNonce(long), "a nonce longer than any the signer issues is not logged")
}

// listOf is the revocation list s holds.
func listOf(t *testing.T, e *env, s *signer.Signer) *identity.CRL {
	t.Helper()
	data, err := s.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), data)
	require.NoError(t, err)
	return crl
}

func TestCallerRevocation_TheRecordsAndTheEntryTravelWithTheBundleTheSyncAndLaterLists(t *testing.T) {
	e := newEnv(t)
	var c clock
	p := e.registered("ws-provider")
	b := clockedReplica(t, e, &c)
	a := openClocked(t, e.dir, &c)
	c.advance(61 * 24 * time.Hour)
	issued, err := b.RenewCaller(renewal(t, p, newKey(t), &c))
	require.NoError(t, err)

	revoke := signedAt(t, e.rootID, &c, identity.ActionRevokeCaller, identity.CallerRevokeRequest{Name: "ws-provider"})
	got, err := b.RevokeCaller(revoke)
	require.NoError(t, err)
	want := []identity.CallerSerial{{Serial: identity.SerialOf(p.cert), Revoked: true}, {Serial: issued.Serial, Parent: identity.SerialOf(p.cert), Revoked: true}}
	assert.Equal(t, want, got.Serials, "the replica holds the record the bundle carried and its own renewal, with its parent")
	again, err := b.RevokeCaller(revoke)
	require.NoError(t, err)
	assert.Equal(t, want, again.Serials)
	serials := listOf(t, e, b).Serials
	assert.Len(t, serials, len(slices.Compact(slices.Clone(serials))), "a second revocation adds no serial twice")

	pushList(t, a, b)
	_, err = b.RevokeCaller(signedAt(t, e.rootID, &c, identity.ActionRevokeCaller, identity.CallerRevokeRequest{Name: "elsewhere"}))
	require.NoError(t, err)
	pushList(t, a, b)
	assert.Contains(t, listOf(t, e, a).Callers, "elsewhere", "the sync carries an entry that changes nothing else on the list")

	tok, err := b.IssueCallerToken(signedAt(t, e.rootID, &c, identity.ActionIssueCallerToken, identity.CallerTokenRequest{Name: "later"}))
	require.NoError(t, err)
	_, err = b.RegisterCaller(identity.CallerRegistration{Token: tok.Token, CSR: csrFor(t, newKey(t))})
	require.NoError(t, err)
	assert.Contains(t, listOf(t, e, b).Callers, "ws-provider", "a later list, which spent the token, keeps the entry")

	c.advance(time.Hour)
	later, err := b.RevokeCaller(signedAt(t, e.rootID, &c, identity.ActionRevokeCaller, identity.CallerRevokeRequest{Name: "ws-provider"}))
	require.NoError(t, err)
	pushList(t, a, b)
	assert.True(t, later.RevokedAt.Equal(listOf(t, e, a).Callers["ws-provider"]), "the sync keeps the later of two revocations of a name")
}
