package signer_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/host/internal/signer"
	"github.com/garamsh/naru/lib/identity"
)

// clock is the time a signer under test reads; a test moves it by advance.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

func (c *clock) advance(d time.Duration) { c.offset.Add(int64(d)) }

// openClocked opens the signer of dir on c's time.
func openClocked(t *testing.T, dir string, c *clock) *signer.Signer {
	t.Helper()
	s, err := signer.Open(signer.Options{Dir: dir, Log: quiet, Now: c.now})
	require.NoError(t, err)
	return s
}

// request is a reissue request for a new key, with nonce, expiring a minute after c's time.
func (e *env) request(c *clock, nonce string) (signed []byte, key *ecdsa.PrivateKey) {
	e.t.Helper()
	key = newKey(e.t)
	return reissueRequest(e.t, e.recovery.Ed25519, csrFor(e.t, key), nonce, c.now().Add(time.Minute)), key
}

// pushList gives to the list from holds, the way orch's sync does.
func pushList(t *testing.T, to, from *signer.Signer) {
	t.Helper()
	data, err := from.CRL()
	require.NoError(t, err)
	_, err = to.PushCRL(data)
	require.NoError(t, err)
}

func noncesOf(t *testing.T, e *env, s *signer.Signer) map[string]time.Time {
	t.Helper()
	data, err := s.CRL()
	require.NoError(t, err)
	crl, err := identity.VerifyCRL(ed25519.PublicKey(e.authorities().RevocationKey), data)
	require.NoError(t, err)
	return crl.Nonces
}

func TestReissueRoot_ANonceUsedOnOneSignerIsRefusedOnAnotherAfterTheSync(t *testing.T) {
	e := newEnv(t)
	a, b, unsynced := e.s, replica(t, e), replica(t, e)
	var c clock
	signed, key := e.request(&c, freshNonce(t))

	issued, err := a.ReissueRoot(signed)
	require.NoError(t, err)
	fresh := person{key: key, cert: parseCert(t, issued.Certificate)}

	_, err = unsynced.ReissueRoot(signed)
	require.NoError(t, err, "control: a signer that has not had the list takes the same request")

	pushList(t, b, a)
	_, err = b.ReissueRoot(signed)
	require.ErrorIs(t, err, signer.ErrReplay, "B has the nonce A used")
	_, err = b.IssueOrch(e.signed(fresh, identity.ActionIssueOrch, identity.IssueRequest{Name: "orch-1", CSR: csrFor(t, newKey(t))}))
	assert.NoError(t, err, "the replay did not revoke the root A issued")
}

func TestReissueRoot_ANonceReachesASignerThatMergesItWithARevocationOfItsOwn(t *testing.T) {
	e := newEnv(t)
	a, b := e.s, replica(t, e)
	var c clock
	op := e.issue(identity.RoleOperator, "alice")
	signed, _ := e.request(&c, freshNonce(t))
	_, err := a.ReissueRoot(signed)
	require.NoError(t, err)
	_, err = b.Revoke(e.signed(e.rootID, identity.ActionRevoke, identity.RevokeRequest{Serial: op.cert.SerialNumber.Text(16)}))
	require.NoError(t, err)

	syncAll(t, a, b)

	for name, s := range map[string]*signer.Signer{"A": a, "B": b} {
		_, err := s.ReissueRoot(signed)
		assert.ErrorIs(t, err, signer.ErrReplay, name+" keeps the nonce through the merge")
		serials, _ := serialsOf(t, e, s)
		assert.Contains(t, serials, op.cert.SerialNumber.Text(16), name+" keeps the revocation too")
	}
}

func TestReissueRoot_TheNonceIsKeptInTheDirectoryAndInTheBundle(t *testing.T) {
	e := newEnv(t)
	var c clock
	signed, key := e.request(&c, freshNonce(t))
	issued, err := e.s.ReissueRoot(signed)
	require.NoError(t, err)
	e.rootID = person{key: key, cert: parseCert(t, issued.Certificate)}

	restored := replica(t, e)

	_, err = restored.ReissueRoot(signed)
	assert.ErrorIs(t, err, signer.ErrReplay, "a signer restored from a bundle taken after the use")
	_, err = openAt(t, e.dir).ReissueRoot(signed)
	assert.ErrorIs(t, err, signer.ErrReplay, "a signer reopened on the directory")
}

func TestReissueRoot_ANonceIsDroppedFromTheListOnceItsRequestHasExpired(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	first, second := freshNonce(t), freshNonce(t)
	signed, _ := e.request(&c, first)
	_, err := s.ReissueRoot(signed)
	require.NoError(t, err)
	require.Contains(t, noncesOf(t, e, s), first, "control: the list holds the nonce while its request is live")

	c.advance(2 * time.Minute)
	next, _ := e.request(&c, second)
	_, err = s.ReissueRoot(next)
	require.NoError(t, err)

	got := noncesOf(t, e, s)
	assert.Contains(t, got, second)
	assert.NotContains(t, got, first, "the first request has expired, so its nonce is not carried on")
}

func TestReissueRoot_APeeredSignerRefusesUntilItHasTakenTheListLately(t *testing.T) {
	e := newEnv(t)
	var c clock
	s := openClocked(t, e.dir, &c)
	pushList(t, s, e.s)

	c.advance(20 * time.Second)
	signed, _ := e.request(&c, freshNonce(t))
	_, err := s.ReissueRoot(signed)
	require.NoError(t, err, "control: the list was taken 20 seconds ago")

	c.advance(40 * time.Second)
	stale, _ := e.request(&c, freshNonce(t))
	_, err = s.ReissueRoot(stale)
	require.ErrorIs(t, err, signer.ErrNotSynced, "the list was taken 60 seconds ago")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/reissue-root", bytes.NewReader(stale)))
	assert.Equal(t, http.StatusConflict, rec.Code, "refused as a state the signer is not in")

	pushList(t, s, e.s)
	_, err = s.ReissueRoot(stale)
	assert.NoError(t, err, "taking the list again lifts the refusal, and the refused request was not used up")
}

func TestReissueRoot_ARestartedPeeredSignerWaitsForTheNextSync(t *testing.T) {
	e := newEnv(t)
	var c clock
	pushList(t, openClocked(t, e.dir, &c), e.s)

	restarted := openClocked(t, e.dir, &c)
	signed, _ := e.request(&c, freshNonce(t))
	_, err := restarted.ReissueRoot(signed)
	require.ErrorIs(t, err, signer.ErrNotSynced, "it has taken no list since it started")

	pushList(t, restarted, e.s)
	_, err = restarted.ReissueRoot(signed)
	assert.NoError(t, err)
}

func TestReissueRoot_ANonceIsTakenFromAListThatAddsNoSerial(t *testing.T) {
	e := newEnv(t)
	a, b := e.s, replica(t, e)
	var c clock
	_, err := b.Revoke(e.signed(e.rootID, identity.ActionRevoke, identity.RevokeRequest{Serial: e.rootID.cert.SerialNumber.Text(16)}))
	require.NoError(t, err, "B already revoked the root that A's reissue revokes")
	signed, _ := e.request(&c, freshNonce(t))
	_, err = a.ReissueRoot(signed)
	require.NoError(t, err)
	serialsA, _ := serialsOf(t, e, a)
	serialsB, _ := serialsOf(t, e, b)
	require.Equal(t, serialsA, serialsB, "the lists differ in the nonce alone")
	require.Empty(t, noncesOf(t, e, b))

	pushList(t, b, a)

	assert.NotEmpty(t, noncesOf(t, e, b), "B took the nonce")
	_, err = b.ReissueRoot(signed)
	assert.ErrorIs(t, err, signer.ErrReplay)
}
