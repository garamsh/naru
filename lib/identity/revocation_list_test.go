package identity_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/naru/lib/identity"
)

// source serves the list set, or its error.
type source struct {
	list []byte
	err  error
	n    int
}

func (s *source) CRL(context.Context) ([]byte, error) { s.n++; return s.list, s.err }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type rig struct {
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
	src  *source
	clk  *clock
	list *identity.RevocationList
}

func (r *rig) sign(t *testing.T, seq uint64, serials ...string) []byte {
	t.Helper()
	b, err := identity.SignCRL(r.priv, identity.CRL{Seq: seq, IssuedAt: r.clk.t, Serials: serials})
	require.NoError(t, err)
	return b
}

func newRig(t *testing.T) *rig {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	r := &rig{pub: pub, priv: priv, src: &source{}, clk: &clock{t: now}}
	r.src.list = r.sign(t, 1)
	r.list = &identity.RevocationList{Source: r.src, Key: pub, TTL: time.Minute, MaxAge: 10 * time.Minute, Now: r.clk.now}
	return r
}

func TestRevocationList_RefusesEveryCheckWhenNoListWasEverHad(t *testing.T) {
	r := newRig(t)
	r.src.err = errors.New("signer down")
	_, err := r.list.Current(t.Context())
	assert.ErrorIs(t, err, identity.ErrNoRevocationList)
	assert.Equal(t, "signer down", r.list.Status().LastError)
}

func TestRevocationList_KeepsTheLastListUntilMaxAgeThenRefuses(t *testing.T) {
	r := newRig(t)
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)

	r.src.err = errors.New("signer down")
	r.clk.t = r.clk.t.Add(9 * time.Minute)
	_, err = r.list.Current(t.Context())
	assert.NoError(t, err, "within the max age the last list is kept")

	r.clk.t = r.clk.t.Add(2 * time.Minute)
	_, err = r.list.Current(t.Context())
	assert.ErrorIs(t, err, identity.ErrStaleRevocationList, "older than the max age and not refreshed: fail closed")
}

func TestRevocationList_RefusesAListSignedByAnotherKeyAndOneWithALowerSeq(t *testing.T) {
	r := newRig(t)
	r.src.list = r.sign(t, 5)
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)
	r.clk.t = r.clk.t.Add(2 * time.Minute)

	r.src.list = r.sign(t, 4)
	crl, err := r.list.Current(t.Context())
	require.NoError(t, err, "the list held is kept while the lower one is refused")
	assert.EqualValues(t, 5, crl.Seq, "a list with a lower seq than the one held does not replace it")
	assert.Contains(t, r.list.Status().LastError, "seq")

	_, other, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	r.src.list, err = identity.SignCRL(other, identity.CRL{Seq: 9, IssuedAt: r.clk.t})
	require.NoError(t, err)
	r.clk.t = r.clk.t.Add(2 * time.Minute)
	crl, err = r.list.Current(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 5, crl.Seq, "a list another key signed is not taken")
}

func TestRevocationList_UsesTheListForTTLWithoutAskingAgain(t *testing.T) {
	r := newRig(t)
	for range 3 {
		_, err := r.list.Current(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, 1, r.src.n)
	r.clk.t = r.clk.t.Add(2 * time.Minute)
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 2, r.src.n)
}

func TestRevocationList_KeptFileLetsARestartWithinMaxAgeNeedNoFetch(t *testing.T) {
	r := newRig(t)
	r.list.File = filepath.Join(t.TempDir(), "sub", "crl.json")
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)
	info, err := os.Stat(r.list.File)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	r.clk.t = r.clk.t.Add(5 * time.Minute)
	down := &source{err: errors.New("orch down")}
	restarted := &identity.RevocationList{Source: down, Key: r.pub, TTL: time.Minute, MaxAge: 10 * time.Minute, File: r.list.File, Now: r.clk.now}
	_, err = restarted.Current(t.Context())
	assert.NoError(t, err, "the kept list is within the max age")

	r.clk.t = r.clk.t.Add(10 * time.Minute)
	later := &identity.RevocationList{Source: down, Key: r.pub, TTL: time.Minute, MaxAge: 10 * time.Minute, File: r.list.File, Now: r.clk.now}
	_, err = later.Current(t.Context())
	assert.ErrorIs(t, err, identity.ErrStaleRevocationList, "the kept list is older than the max age")
}

func TestRevocationList_AKeptFileAnotherKeySignedIsIgnored(t *testing.T) {
	r := newRig(t)
	r.list.File = filepath.Join(t.TempDir(), "crl.json")
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	other := &identity.RevocationList{Source: &source{err: errors.New("down")}, Key: pub, TTL: time.Minute, File: r.list.File, Now: r.clk.now}
	_, err = other.Current(t.Context())
	assert.ErrorIs(t, err, identity.ErrNoRevocationList)
}

func TestRevocationList_SeedCountsAsFetchedWhenTakenAndYieldsToANewerKeptList(t *testing.T) {
	r := newRig(t)
	r.list.Seed = r.sign(t, 3)
	r.list.File = filepath.Join(t.TempDir(), "crl.json")
	r.src.err = errors.New("orch down")
	crl, err := r.list.Current(t.Context())
	require.NoError(t, err, "a joined host starts on the list its configuration carried")
	assert.EqualValues(t, 3, crl.Seq)

	r.src.err = nil
	r.src.list = r.sign(t, 8)
	r.clk.t = r.clk.t.Add(2 * time.Minute)
	_, err = r.list.Current(t.Context())
	require.NoError(t, err)
	newer := &identity.RevocationList{Source: &source{err: errors.New("down")}, Key: r.pub, TTL: time.Minute, File: r.list.File, Seed: r.sign(t, 3), Now: r.clk.now}
	crl, err = newer.Current(t.Context())
	require.NoError(t, err)
	assert.EqualValues(t, 8, crl.Seq, "a seed with a lower seq than the kept list is not taken")
}

func TestRevocationList_CheckRefusesARevokedChainAndAcceptsAnotherChainOfACleanOne(t *testing.T) {
	r := newRig(t)
	c := newCA(t)
	revoked, _ := c.issue(t, "revoked", 77, now.Add(time.Hour), "operator")
	good, _ := c.issue(t, "good", 78, now.Add(time.Hour), "operator")
	r.src.list = r.sign(t, 2, identity.SerialOf(revoked))

	assert.ErrorIs(t, r.list.Check(t.Context(), [][]*x509.Certificate{{revoked, c.cert}}), identity.ErrRevoked)
	assert.NoError(t, r.list.Check(t.Context(), [][]*x509.Certificate{{good, c.cert}}))
	assert.NoError(t, r.list.Check(t.Context(), [][]*x509.Certificate{{revoked, c.cert}, {good, c.cert}}), "any chain without a revoked certificate suffices")
	assert.ErrorIs(t, r.list.Check(t.Context(), nil), identity.ErrNoChain)
}

func TestRevocationList_StatusAndAge(t *testing.T) {
	r := newRig(t)
	_, held := r.list.Age()
	assert.False(t, held)
	_, err := r.list.Current(t.Context())
	require.NoError(t, err)
	r.clk.t = r.clk.t.Add(30 * time.Second)
	age, held := r.list.Age()
	assert.True(t, held)
	assert.Equal(t, 30*time.Second, age)
	assert.EqualValues(t, 1, r.list.Status().Seq)
}

func serverLeaf(t *testing.T, c ca, serial int64, dns string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: dns}, DNSNames: []string{dns},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, c.cert, &key.PublicKey, c.key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return cert
}

func TestPeerCheck_ChainNameAndRevocation(t *testing.T) {
	r := newRig(t)
	c := newCA(t)
	leaf := serverLeaf(t, c, 90, "host-1")
	revoked := serverLeaf(t, c, 91, "host-1")
	r.src.list = r.sign(t, 2, identity.SerialOf(revoked))
	check := identity.PeerCheck{Roots: c.pool, Usage: x509.ExtKeyUsageServerAuth, Names: []string{"host-1"}, Revocations: r.list, Now: r.clk.now}

	assert.NoError(t, check.Verify(t.Context(), []*x509.Certificate{leaf}))
	assert.ErrorIs(t, check.Verify(t.Context(), []*x509.Certificate{revoked}), identity.ErrRevoked)

	check.Names = []string{"host-2"}
	assert.ErrorContains(t, check.Verify(t.Context(), []*x509.Certificate{leaf}), "names none of")

	other := newCA(t)
	check.Names = nil
	check.Roots = other.pool
	assert.Error(t, check.Verify(t.Context(), []*x509.Certificate{leaf}), "a chain to another authority")

	check.Roots = c.pool
	check.Revocations = nil
	assert.NoError(t, check.Verify(t.Context(), []*x509.Certificate{revoked}), "without a list no revocation is checked")
	assert.Error(t, check.Verify(t.Context(), nil))
}

func TestPeerCheck_RefusesEveryPeerWhenTheListIsStale(t *testing.T) {
	r := newRig(t)
	c := newCA(t)
	leaf := serverLeaf(t, c, 92, "host-1")
	check := identity.PeerCheck{Roots: c.pool, Usage: x509.ExtKeyUsageServerAuth, Revocations: r.list, Now: r.clk.now}
	require.NoError(t, check.Verify(t.Context(), []*x509.Certificate{leaf}))
	r.src.err = errors.New("down")
	r.clk.t = r.clk.t.Add(11 * time.Minute)
	check.Now = func() time.Time { return now }
	assert.ErrorIs(t, check.Verify(t.Context(), []*x509.Certificate{leaf}), identity.ErrStaleRevocationList)
}

// blockingSource blocks every fetch until release is closed, and counts them.
type blockingSource struct {
	calls   atomic.Int32
	release chan struct{}
}

func (s *blockingSource) CRL(ctx context.Context) ([]byte, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	return nil, errors.New("signer down")
}

func TestRevocationList_ASourceThatIsDownIsAskedOncePerIntervalAndNoCheckWaitsForIt(t *testing.T) {
	r := newRig(t)
	src := &blockingSource{release: make(chan struct{})}
	defer close(src.release)
	r.list.Source = src
	r.list.RetryInterval = 10 * time.Second
	r.list.Seed = r.sign(t, 1)
	r.clk.t = r.clk.t.Add(2 * time.Minute) // past TTL, within MaxAge

	var wg sync.WaitGroup
	first := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); close(first); _, _ = r.list.Current(context.Background()) }()
	<-first
	require.Eventually(t, func() bool { return src.calls.Load() == 1 }, 5*time.Second, time.Millisecond, "one caller fetches")

	done := make(chan error, 20)
	for range 20 {
		go func() { _, err := r.list.Current(context.Background()); done <- err }()
	}
	for range 20 {
		select {
		case err := <-done:
			assert.NoError(t, err, "the list held is within MaxAge")
		case <-time.After(2 * time.Second):
			t.Fatal("a Check waited for the source that is down")
		}
	}
	assert.EqualValues(t, 1, src.calls.Load(), "no second fetch while one runs")

	src.release <- struct{}{} // the running fetch fails
	wg.Wait()
	_, err := r.list.Current(context.Background())
	require.NoError(t, err)
	assert.EqualValues(t, 1, src.calls.Load(), "a failed fetch holds the next off for the interval")

	r.clk.t = r.clk.t.Add(11 * time.Second)
	go func() { src.release <- struct{}{} }()
	_, _ = r.list.Current(context.Background())
	assert.EqualValues(t, 2, src.calls.Load(), "and the next is made after it")
}

func TestRevocationList_ASeedIsAsOldAsItsSigningAndIsRefusedPastMaxAgeWithNoFileAndADeadSource(t *testing.T) {
	r := newRig(t)
	old := r.sign(t, 4) // signed at now
	r.clk.t = r.clk.t.Add(time.Hour)
	r.list.Seed = old
	r.list.File = filepath.Join(t.TempDir(), "crl.json")
	r.src.err = errors.New("orch down")
	_, err := r.list.Current(t.Context())
	require.ErrorIs(t, err, identity.ErrStaleRevocationList)
}
